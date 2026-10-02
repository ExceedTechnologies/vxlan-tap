// Package service runs vxlan-tap as a Windows service and manages its
// installation.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const DefaultName = "vxlan-tap"

// RunFunc is the service body. It must return promptly once ctx is done.
type RunFunc func(ctx context.Context, elog *eventlog.Log) error

// Run hands control to the service control manager. It blocks until the
// service stops.
func Run(name string, run RunFunc) error {
	elog, err := eventlog.Open(name)
	if err != nil {
		return err
	}
	defer elog.Close()
	return svc.Run(name, &handler{run: run, elog: elog})
}

type handler struct {
	run  RunFunc
	elog *eventlog.Log
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx, h.elog) }()

	status <- svc.Status{State: svc.Running, Accepts: accepts}
	h.elog.Info(1, "vxlan-tap started")

	for {
		select {
		case err := <-done:
			// The tunnel stopped on its own: that is a failure.
			if err == nil {
				err = errors.New("tunnel exited unexpectedly")
			}
			h.elog.Error(1, "vxlan-tap stopped: "+err.Error())
			// Non-zero exit lets the SCM recovery actions restart us.
			return false, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-done; err != nil {
					h.elog.Warning(1, "vxlan-tap stopped with error: "+err.Error())
				} else {
					h.elog.Info(1, "vxlan-tap stopped")
				}
				return false, 0
			}
		}
	}
}

// Install registers the service to run exePath with args at boot.
func Install(name, exePath string, args ...string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(name); err == nil {
		s.Close()
		return fmt.Errorf("service %q already exists", name)
	}
	s, err := m.CreateService(name, exePath, mgr.Config{
		DisplayName:  "VXLAN TAP bridge (" + name + ")",
		Description:  "Bridges an OpenVPN TAP adapter onto a point-to-point VXLAN tunnel.",
		StartType:    mgr.StartAutomatic,
		Dependencies: []string{"Tcpip"},
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Restart after 5s if the tunnel dies (e.g. adapter briefly removed).
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 24*60*60)

	if err := eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		s.Delete()
		return fmt.Errorf("register event log source: %w", err)
	}
	return nil
}

// Uninstall stops (if running) and removes the service.
func Uninstall(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		stopAndWait(s)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	eventlog.Remove(name)
	return nil
}

// Start starts the installed service.
func Start(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	return s.Start()
}

// Stop stops the service and waits for it to exit.
func Stop(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	return stopAndWait(s)
}

func stopAndWait(s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for service to stop")
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return fmt.Errorf("query service: %w", err)
		}
	}
	return nil
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, errors.New("access denied connecting to the service manager; run as Administrator")
	}
	return m, err
}
