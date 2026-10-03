// Command vxlan-tap bridges an OpenVPN TAP-Windows6 adapter onto a VXLAN
// segment with one or more remote VTEPs.
package main

// Embeds the icon, manifest and version info (cmd/vxlan-tap/winres/winres.json) as
// rsrc_windows_*.syso, which go build links in. The installer build passes the real version.
//go:generate go tool go-winres make --in winres/winres.json --out rsrc --arch amd64,arm64

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"text/tabwriter"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"vxlan-tap/internal/config"
	"vxlan-tap/internal/route"
	"vxlan-tap/internal/service"
	"vxlan-tap/internal/tap"
	"vxlan-tap/internal/tunnel"
)

const usage = `Usage: vxlan-tap <command> [flags]

Commands:
  run       -config FILE [-cpuprofile F] run the tunnel in the foreground (Ctrl+C to stop);
                                         -cpuprofile writes a Go CPU profile on exit
  install   -config FILE [-name NAME]    install as an auto-start Windows service
  uninstall [-name NAME]                 stop and remove the service
  start     [-name NAME]                 start the service
  stop      [-name NAME]                 stop the service
  list-taps                              list installed TAP-Windows6 adapters

All commands except list-taps require an elevated (Administrator) prompt.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect service mode: %w", err)
	}

	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to YAML config file")
	name := fs.String("name", service.DefaultName, "Windows service name")
	cpuProfile := fs.String("cpuprofile", "", "write a CPU profile to this file (run only)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	switch cmd {
	case "run":
		if *cfgPath == "" {
			return errors.New("run: -config is required")
		}
		if isSvc {
			return runService(*name, *cfgPath)
		}
		return runConsole(*cfgPath, *cpuProfile)
	case "install":
		return install(*name, *cfgPath)
	case "uninstall":
		if err := service.Uninstall(*name); err != nil {
			return err
		}
		fmt.Printf("service %q removed\n", *name)
	case "start":
		if err := service.Start(*name); err != nil {
			return err
		}
		fmt.Printf("service %q started\n", *name)
	case "stop":
		if err := service.Stop(*name); err != nil {
			return err
		}
		fmt.Printf("service %q stopped\n", *name)
	case "list-taps":
		return listTAPs()
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func runConsole(cfgPath, cpuProfile string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	var out io.Writer = os.Stderr
	if cfg.LogFile != "" {
		f, err := openLog(cfg.LogFile)
		if err != nil {
			return err
		}
		defer f.Close()
		out = io.MultiWriter(os.Stderr, f)
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: cfg.Level}))

	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
		log.Info("writing CPU profile on exit", "file", cpuProfile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runTunnel(ctx, cfg, log)
}

func runService(name, cfgPath string) error {
	return service.Run(name, func(ctx context.Context, elog *eventlog.Log) error {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		out := io.Discard // the event log still records start/stop/fatal errors
		if cfg.LogFile != "" {
			f, err := openLog(cfg.LogFile)
			if err != nil {
				return err
			}
			defer f.Close()
			out = f
		}
		log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: cfg.Level}))
		err = runTunnel(ctx, cfg, log)
		if err != nil {
			log.Error("tunnel failed", "err", err)
		}
		return err
	})
}

func runTunnel(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	adapter, err := tap.Find(cfg.TAP)
	if err != nil {
		return err
	}
	// Pin the underlay path before tap.Open sets the adapter's media status
	// to connected, which is when Windows activates any routes (such as a
	// default gateway) configured on it.
	if cfg.PinRoute() {
		for _, remote := range cfg.Remotes {
			pinned, err := route.Pin(cfg.Local, remote)
			if err != nil {
				return fmt.Errorf("%w (set pin_remote_route: false to skip)", err)
			}
			via := "on-link"
			if pinned.NextHop.IsValid() {
				via = pinned.NextHop.String()
			}
			log.Info("pinned route to remote VTEP", "dest", pinned.Dest, "via", via,
				"ifindex", pinned.InterfaceIndex, "already_present", pinned.Existed)
			defer func() {
				if err := pinned.Remove(); err != nil {
					log.Warn("failed to remove pinned route", "dest", pinned.Dest, "err", err)
				} else {
					log.Info("removed pinned route", "dest", pinned.Dest)
				}
			}()
		}
	}
	dev, err := tap.Open(adapter)
	if err != nil {
		return err
	}
	log.Info("opened TAP adapter", "name", adapter.Name, "guid", adapter.GUID)
	if mac, err := dev.MAC(); err == nil {
		log.Info("TAP adapter MAC", "mac", mac)
	}
	// Assume a 1500-byte underlay: VXLAN adds 50 bytes over IPv4, 70 over IPv6.
	maxMTU := uint32(1450)
	if cfg.Local.Is6() {
		maxMTU = 1430
	}
	if mtu, err := dev.MTU(); err == nil && mtu > maxMTU {
		log.Warn("TAP adapter MTU is larger than fits a 1500-byte underlay; large frames will be fragmented",
			"mtu", mtu, "recommended", maxMTU)
	}

	remotes := make([]netip.AddrPort, len(cfg.Remotes))
	for i, r := range cfg.Remotes {
		remotes[i] = netip.AddrPortFrom(r, uint16(cfg.Port))
	}
	tun, err := tunnel.New(tunnel.Config{
		Local:   netip.AddrPortFrom(cfg.Local, uint16(cfg.Port)),
		Remotes: remotes,
		VNI:     cfg.VNIValue(),
		Logger:  log,
	}, dev)
	if err != nil {
		return err
	}
	err = tun.Run(ctx)
	if n := dev.AsyncWriteErrors(); n > 0 {
		log.Warn("TAP driver rejected queued frames", "count", n)
	}
	return err
}

func install(name, cfgPath string) error {
	if cfgPath == "" {
		return errors.New("install: -config is required")
	}
	absCfg, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	// Validate now rather than failing silently at service start.
	if _, err := config.Load(absCfg); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	args := []string{"run", "-config", absCfg}
	if name != service.DefaultName {
		args = append(args, "-name", name)
	}
	if err := service.Install(name, exe, args...); err != nil {
		return err
	}
	fmt.Printf("service %q installed (config %s); start it with: vxlan-tap start", name, absCfg)
	if name != service.DefaultName {
		fmt.Printf(" -name %s", name)
	}
	fmt.Println()
	return nil
}

func listTAPs() error {
	adapters, err := tap.List()
	if err != nil {
		return err
	}
	if len(adapters) == 0 {
		fmt.Println("no TAP-Windows6 (tap0901) adapters found")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tGUID")
	for _, a := range adapters {
		fmt.Fprintf(w, "%s\t%s\n", a.Name, a.GUID)
	}
	return w.Flush()
}

func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}
