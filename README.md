# vxlan-tap

Attach a Windows host to a VXLAN segment. `vxlan-tap` bridges Ethernet frames
between an OpenVPN **TAP-Windows6** virtual adapter and a VXLAN segment
(RFC 7348): one local underlay IP, one VNI, and one or more remote VTEPs.

```
 Windows apps ── TAP adapter (192.168.100.1) ── vxlan-tap ══ UDP 4789 ══ remote VTEP (Linux, switch, …)
```

Packets from a configured remote VTEP with the configured VNI are written
to the adapter. Anything else is dropped. With one remote, every frame
read from the adapter is sent to it.

With several remotes (`remote_ips`), vxlan-tap behaves like a Linux VXLAN
device with a static flood list:

- It learns which remote each overlay MAC address is behind, from the
  source MAC of frames received from that remote. An entry expires after
  300 seconds without traffic and follows the MAC if it moves to another
  remote.
- Unicast frames to a learned MAC go only to that remote.
- Broadcast, multicast and unknown unicast frames are sent to every remote
  (head-end replication). ARP and other discovery traffic therefore reaches
  everyone.
- Frames from one remote are never relayed to another. If the remotes need
  to talk to each other, they must also be configured as peers of each
  other (a full mesh).

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
go generate ./cmd/vxlan-tap
go build -o vxlan-tap.exe ./cmd/vxlan-tap
go test ./...
```

`go generate` embeds the icon (`installer\icon.png`), product name,
description and version from `cmd\vxlan-tap\winres\winres.json` into
the exe; without it the exe builds but has none of them. The version stays
0.0.0.0 unless the installer build sets it.

`go test ./...` skips the tests that need a real adapter. To run them
against an installed TAP adapter that nothing else is using:

```
set VXLAN_TAP_TEST=1
set VXLAN_TAP_ADAPTER=vxlan0      (optional; defaults to the first one)
go test ./internal/tap -v
```

### Installer

`installer\build.ps1 [-Version 1.2.3]` builds an MSI for amd64 and one for
arm64 into `installer\bin`. It needs Go and the .NET SDK 8 or later; the
WiX toolset is restored from NuGet. The MSI:

1. Installs `vxlan-tap.exe` to `C:\Program Files\vxlan-tap`.
2. If no TAP-Windows6 driver is installed, installs the bundled one from
   `tap-driver` and creates an adapter named `vxlan0`.
3. Adds the firewall rule `vxlan-tap VXLAN (UDP-In)` for inbound UDP on
   the VXLAN port. Pass `ADD_FIREWALL_RULE=0` to skip it, for example
   when Group Policy already opens the port.
4. If `VXLAN_REMOTE_IPS` is set, writes `config.yaml` to the install
   folder with those peers, unless `WRITE_CONFIG=0`. Only single addresses of the first entry's
   address family are used; subnets and ranges only go into the firewall
   rule. `local_ip` is `VXLAN_LOCAL_IP`, or else the address Windows uses
   to reach the first peer. `vni` is `VXLAN_VNI` (default 100), `tap` is
   `TAP_ADAPTER_NAME` if that adapter exists, and the log goes to
   `vxlan-tap.log` in the install folder. An existing, different
   `config.yaml` is saved as `config.yaml.bak` first.
5. If `INSTALL_SERVICE=1`, installs the `vxlan-tap` service with
   `config.yaml` and starts it. An existing service is left as it is. If
   the service cannot start yet, for example until a reboot finishes the
   driver install, it starts at the next boot. The setting is remembered,
   so upgrades put the service back.

```
msiexec /i vxlan-tap-1.0.0-amd64.msi VXLAN_REMOTE_IPS=10.0.0.2,10.0.0.3 VXLAN_VNI=100 INSTALL_SERVICE=1 /l*v install.log
```

Double-clicking the MSI, or running `msiexec /i` without `/qn`, opens a
setup wizard with pages for these settings. They are prefilled with the
defaults or with any properties given on the command line. The wizard's
"Write a default config.yaml" checkbox starts unchecked unless
`WRITE_CONFIG=1` is given. The wizard only offers the service when that
box is checked with remote addresses, or there is already a `config.yaml`. Pass the properties directly for silent installs (`/qn`).

All properties are optional. By default the rule allows port 4789 from
any remote address, the adapter is named `vxlan0`, and no config or
service is created. Uninstalling removes the firewall rule and, if the
MSI installed the driver, the driver and its adapters. It also stops and
removes a `vxlan-tap` service, and leaves `config.yaml` and the log.
Upgrades keep the driver and adapter. The installer does not configure
the adapter, so continue with the setup steps below.

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
   With several remotes, list them all, separated by commas:
   `remoteip=10.0.0.2,10.0.0.3`.
4. **Write a config.** Copy `config.example.yaml` to `config.yaml` and edit it.

### Routing the underlay (`pin_remote_route`)

If the TAP adapter has a default gateway, or any route that covers
a remote VTEP, the encapsulated VXLAN packets would be routed into the
tunnel itself, and the underlay connection would fail. To prevent that,
vxlan-tap does the following at startup, before the adapter's media status
is set to connected:

1. It finds the interface that owns `local_ip`.
2. For each remote, it looks up the best route **on that interface
   only**, ignoring routes on the TAP and other adapters.
3. It adds a host route (`/32`, or `/128` for IPv6) to each remote via
   that gateway, or on-link if the remote is on the local subnet.

The routes are removed when the tunnel stops. It is not persistent, so it
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

With several Linux peers in a full mesh, create each VXLAN device without
`remote`, and add a flood entry for every other VTEP, including the
Windows host:

```
ip link add vxlan100 type vxlan id 100 local 10.0.0.2 dstport 4789 dev eth0
bridge fdb append 00:00:00:00:00:00 dev vxlan100 dst 10.0.0.1
bridge fdb append 00:00:00:00:00:00 dev vxlan100 dst 10.0.0.3
```

## Notes and limitations

- **Static peers only:** remotes are listed in the config. There is no
  multicast underlay, no BGP EVPN, and no static MAC entries.
- **Flooding costs one send per remote.** Broadcast and multicast traffic
  from Windows is sent once to each remote, so it grows with the number of
  remotes.
- **Remotes are identified by IP address,** and all of them use the same
  `port`.
- **Fixed UDP source port.** Outgoing packets use the configured port
  (4789) as the source port instead of a per-flow hash. This is RFC
  compliant and accepted by Linux and common VTEPs, but ECMP in the
  underlay will not spread the traffic across paths.
- **No fragment handling of its own.** Oversized frames are fragmented
  by the Windows IP stack on the underlay, so set the adapter MTU as
  shown above.
- With `log_level: debug`, packet and drop counters are logged every
  60s, along with the number of learned MACs when there are several
  remotes. The counters are also always logged on shutdown. `tx_packets`
  counts datagrams sent, so a flooded frame counts once per remote.
  `tx_flooded` counts the frames that were flooded.

## Layout

| Path | Purpose |
|---|---|
| `cmd/vxlan-tap` | CLI and service entry point |
| `internal/config` | YAML loading and validation |
| `internal/vxlan` | VXLAN header encode/decode |
| `internal/tap` | TAP-Windows6 discovery and overlapped I/O |
| `internal/tunnel` | TAP ⇄ UDP forwarding loops, MAC learning, counters |
| `internal/route` | Pinned host routes to the remote VTEPs |
| `internal/service` | Windows service handler and install/uninstall |
