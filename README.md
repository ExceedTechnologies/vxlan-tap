# vxlan-tap

Allows you to attach a Windows host to a VXLAN segment.  
`vxlan-tap` bridges Ethernet frames between upstream VTEPs and the 
OpenVPN TAP-Windows6 virtual adapter on a local machine.

This project has been tested on Windows 11 and Windows Server 2025.

## Build

The PowerShell script located in installer/build.ps1 will build the main
Go executable, optionally digitally sign it, then wrap it into a MSI
for installation.

If building the MSI, you will need to copy the latest version of the
OpenVPN TAP-Windows6 distribution into the folder tap-driver in the
root of the project.

Example command to build for development on amd64:
```
build.ps1 -Version 1.0.2 -Platform x64 -NoSign
```

## MSI

The MSI installer will automatically perform the following tasks:

1. Installs `vxlan-tap.exe` to `C:\Program Files\vxlan-tap`.
2. If no TAP-Windows6 driver is installed, installs the bundled one from
   `tap-driver` and creates an adapter named `VXLAN`.
3. Adds the firewall rule `vxlan-tap VXLAN (UDP-In)` for inbound UDP on
   the VXLAN port.
4. Optionally writes a config.yaml in the system install folder (used when 
   running as service).
5. Optionally installs the executable as a service and starts it.

### Available options for MSI

Most installation properties are available as MSI properties for automated
or unattended installations.  Running the MSI interactively will present
a wizard that allow customization of these parameters.


| Property Name | Description |
| ------------- | ----------- |
| VXLAN_PORT | Set to the UDP port number the VTEP and the local machine are listening on. Defaults to 4789. |
| VXLAN_REMOTE_IPS | Set to the VTEP IP address.  Accepts multiples seperated by commas. |
| ADD_FIREWALL_RULE | Set to 1 (default) to add the VXLAN incoming firewall rule.  Set to 0 to skip. |
| TAP_ADAPTER_NAME | Name of the new TAP adapter to add if one isn't already present.  Defaults to "VXLAN". |
| VXLAN_LOCAL_IP | Local IP address of the Windows host to source VXLAN packets from.  If not provided, will be automatically detected. |
| VXLAN_VNI | VNI of the VXLAN network to attach to.  Defaults to 100 if not provided. |
| WRITE_CONFIG | Set to 1 to write a system config (default).  Set to 0 to skip. |
| INSTALL_SERVICE | Set to 1 to install as a Windows server.  Set to 0 (default) to skip. |

Example install command (unattended):
```
msiexec /i vxlan-tap-1.0.0-amd64.msi VXLAN_REMOTE_IPS=10.0.0.2,10.0.0.3 VXLAN_VNI=100 INSTALL_SERVICE=1 /l*v install.log /qb
```

## Run

### Foreground

The executable can be manually run with a runtime specified configuration file if not installed as service:

```
vxlan-tap run -config config.yaml
```

### Manually installed service

The executable can automatically register itself as Windows service if the installer option wasn't selected:

```
vxlan-tap install -config config.yaml     # stores the absolute config path
vxlan-tap start
vxlan-tap stop
vxlan-tap uninstall
```

### Installer created service

The MSI installer can automatically create a Windows service that runs using the config.yaml in Program Files.

## Common Troubleshooting

### Adapter MTU

The executable and the installer will not adjust the TAP adapter MTU automatically.  Be sure to adjust the adapter settings
to match the MTU of the VTEP and what your underlay network can support.

### Static Addressing

If your VXLAN network does not provide a DHCP server, make sure to manually configure the TAP adapter with a static
IP address.
