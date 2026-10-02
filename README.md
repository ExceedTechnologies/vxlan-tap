# vxlan-tap

Attach a Windows host to a VXLAN segment. `vxlan-tap` bridges Ethernet frames
between an OpenVPN **TAP-Windows6** virtual adapter and a point-to-point
VXLAN tunnel (RFC 7348): one local underlay IP, one remote VTEP, one VNI.

```
 Windows apps ── TAP adapter (192.168.100.1) ── vxlan-tap ══ UDP 4789 ══ remote VTEP (Linux, switch, …)
```

Every frame read from the TAP adapter is sent to the remote VTEP. Packets
from the remote VTEP with the configured VNI are written to the adapter.
Anything else is dropped. There is no MAC learning, multicast or flood
list, because the only peer is the remote VTEP.

## Requirements

- Windows 10/11 (amd64 or arm64), with an Administrator prompt
- **TAP-Windows6 driver** (`tap0901`). It comes with the OpenVPN installer
  (enable *TAP Virtual Ethernet Adapter* under Customize) or the standalone
  [tap-windows6](https://github.com/OpenVPN/tap-windows6/releases)
  installer. Wintun and ovpn-dco are layer 3 drivers and will **not** work.
- Go 1.22+ to build

To add another TAP adapter (OpenVPN ships `tapctl.exe`):

```
"C:\Program Files\OpenVPN\bin\tapctl.exe" create --hwid root\tap0901 --name vxlan0
```

Each TAP adapter can be opened by only one program at a time. Use a
dedicated adapter rather than one OpenVPN also uses.

## Build

```
go build -o vxlan-tap.exe ./cmd/vxlan-tap
go test ./...
```

`go test ./...` skips the tests that need a real adapter. To run them
against an installed TAP adapter that nothing else is using:

```
set VXLAN_TAP_TEST=1
set VXLAN_TAP_ADAPTER=vxlan0      (optional; defaults to the first one)
go test ./internal/tap -v
```

## Setup

1. **Find the adapter:** `vxlan-tap list-taps`
2. **Configure the adapter.** vxlan-tap does not change adapter settings.
   Set an overlay IP and reduce the MTU so that encapsulated frames fit
   the underlay. That is 1450 for an IPv4 underlay and 1430 for IPv6,
   assuming a 1500-byte underlay MTU.
   ```
   netsh interface ipv4 set address name="vxlan0" static 192.168.100.1 255.255.255.0
   netsh interface ipv4 set subinterface "vxlan0" mtu=1450 store=persistent
   ```
   You can also set the driver-level MTU in Device Manager → adapter →
   Advanced → MTU. vxlan-tap warns at startup if the driver reports a
   larger MTU.
3. **Open the firewall** for inbound VXLAN from the peer:
   ```
   netsh advfirewall firewall add rule name="VXLAN" dir=in action=allow protocol=UDP localport=4789 remoteip=10.0.0.2
   ```
4. **Write a config.** Copy `config.example.yaml` to `config.yaml` and edit it.

### Routing the underlay (`pin_remote_route`)

If the TAP adapter has a default gateway, or any route that covers
`remote_ip`, the encapsulated VXLAN packets would be routed into the
tunnel itself, and the underlay connection would fail. To prevent that,
vxlan-tap does the following at startup, before the adapter's media status
is set to connected:

1. It finds the interface that owns `local_ip`.
2. It looks up the best route to `remote_ip` **on that interface only**,
   ignoring routes on the TAP and other adapters.
3. It adds a host route (`remote_ip/32`, or `/128` for IPv6) via that
   gateway, or on-link if the peer is on the local subnet.

The route is removed when the tunnel stops. It is not persistent, so it
also disappears on reboot. If an identical route already exists (for
example after a crash), vxlan-tap adopts it and removes it on exit.

This is on by default. Set `pin_remote_route: false` to manage the
underlay routing yourself. The gateway is resolved once at startup. If
the upstream gateway changes while running (DHCP renew, Wi-Fi to
Ethernet), restart the tunnel. The service restarts automatically if
the tunnel fails.

## Run

Foreground (Ctrl+C to stop):

```
vxlan-tap run -config config.yaml
```

Add `-cpuprofile cpu.out` to write a Go CPU profile when it exits. On
Windows the profile also counts time that threads spend blocked in
system calls, so read it as wall-clock time per goroutine.

As a Windows service (auto-start, restarts on failure):

```
vxlan-tap install -config config.yaml     # stores the absolute config path
vxlan-tap start
vxlan-tap stop
vxlan-tap uninstall
```

Use `-name NAME` with every service command to run several tunnels side
by side. Each tunnel needs its own config, TAP adapter and local IP or
port. Start, stop and fatal errors go to the Application event log
(source = service name). Set `log_file` in the config to get detailed logs.

The service binary path is fixed at install time. If you move
`vxlan-tap.exe`, reinstall the service.

## Linux peer example

```
ip link add vxlan100 type vxlan id 100 local 10.0.0.2 remote 10.0.0.1 dstport 4789 dev eth0
ip link set vxlan100 mtu 1450 up
ip addr add 192.168.100.2/24 dev vxlan100
```

Then `ping 192.168.100.2` from Windows. To check the full MTU, ping with
the DF bit set: `ping -f -l 1422 192.168.100.2`, where 1422 is 1450 minus
28 bytes of IP and ICMP headers. On Linux,
`tcpdump -ni eth0 udp port 4789` shows the encapsulated traffic.

## Notes and limitations

- **Point-to-point only:** one remote VTEP per instance.
- **Fixed UDP source port.** Outgoing packets use the configured port
  (4789) as the source port instead of a per-flow hash. This is RFC
  compliant and accepted by Linux and common VTEPs, but ECMP in the
  underlay will not spread the traffic across paths.
- **No fragment handling of its own.** Oversized frames are fragmented
  by the Windows IP stack on the underlay, so set the adapter MTU as
  shown above.
- With `log_level: debug`, packet and drop counters are logged every
  60s. They are also always logged on shutdown.

## Layout

| Path | Purpose |
|---|---|
| `cmd/vxlan-tap` | CLI and service entry point |
| `internal/config` | YAML loading and validation |
| `internal/vxlan` | VXLAN header encode/decode |
| `internal/tap` | TAP-Windows6 discovery and overlapped I/O |
| `internal/tunnel` | TAP ⇄ UDP forwarding loops and counters |
| `internal/route` | Pinned host route to the remote VTEP |
| `internal/service` | Windows service handler and install/uninstall |
