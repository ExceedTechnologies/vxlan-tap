<#
.SYNOPSIS
    TAP driver steps for the vxlan-tap MSI.

.DESCRIPTION
    Run by the installer's deferred custom actions as LocalSystem. Output goes
    to the MSI log (msiexec /l*v). Each action is safe to run more than once.

      InstallDriver   If no TAP-Windows6 driver package is in the driver store,
                      install the bundled driver and create one adapter, named
                      -AdapterName when that name is free. Records the
                      published INF so that rollback and uninstall remove only
                      a driver that this package installed.
      RemoveDriver    If InstallDriver installed the driver, remove the TAP
                      adapters and the driver package.
      WriteConfig     Write config.yaml next to this script for the peers in
                      -RemoteIps (comma separated). Entries that are not a
                      single address, such as subnets, are skipped. local_ip
                      is -LocalIp, or else the source address Windows uses to
                      reach the first peer. A different existing config.yaml
                      is kept as config.yaml.bak.
      InstallService  Register the vxlan-tap service with config.yaml, unless
                      a service of that name exists, and start it. A failed
                      start, for example until a reboot finishes the driver
                      install, is only logged: the service starts at boot.
      RemoveService   Remove the vxlan-tap service if it runs the
                      vxlan-tap.exe next to this script.
#>
param(
    [Parameter(Mandatory)]
    [ValidateSet('InstallDriver', 'RemoveDriver', 'WriteConfig', 'InstallService', 'RemoveService')]
    [string]$Action,
    [string]$AdapterName = 'VXLAN',
    [string]$RemoteIps,
    [string]$LocalIp,
    [string]$Vni = '100',
    [string]$Port = '4789'
)

$ErrorActionPreference = 'Stop'

$StateKey = 'HKLM:\SOFTWARE\vxlan-tap'
$StateValue = 'TapDriverInf'
$HardwareId = 'root\tap0901'
$DriverDir = Join-Path $PSScriptRoot 'driver'
$Devcon = Join-Path $DriverDir 'devcon.exe'
$Exe = Join-Path $PSScriptRoot 'vxlan-tap.exe'
$ConfigPath = Join-Path $PSScriptRoot 'config.yaml'
$ServiceName = 'vxlan-tap'

function Log([string]$msg) { Write-Output "vxlan-tap setup: $msg" }

# Published INFs (oemNN.inf) of TAP-Windows6 driver packages in the driver store.
function Get-TapDriverInf {
    Get-ChildItem "$env:SystemRoot\INF\oem*.inf" |
        Where-Object { Select-String -Path $_.FullName -Pattern 'tap0901.sys' -SimpleMatch -Quiet } |
        ForEach-Object Name
}

function Get-TapAdapter {
    Get-NetAdapter -IncludeHidden | Where-Object { $_.ComponentID -in 'tap0901', $HardwareId }
}

function Install-Driver {
    $existing = @(Get-TapDriverInf)
    if ($existing) {
        Log "TAP-Windows6 driver already installed ($($existing -join ', ')); skipping driver install"
        return
    }

    $before = @(Get-TapAdapter | ForEach-Object InterfaceGuid)
    Log "installing TAP-Windows6 driver and adapter from $DriverDir"
    & $Devcon install (Join-Path $DriverDir 'OemVista.inf') $HardwareId
    # devcon: 0 = done, 1 = done but a reboot is required, 2+ = failed.
    switch ($LASTEXITCODE) {
        0 { }
        1 { Log 'devcon reports that a reboot is required to finish the driver install' }
        default { throw "devcon install failed with exit code $LASTEXITCODE" }
    }

    $inf = @(Get-TapDriverInf) | Select-Object -First 1
    if ($inf) {
        New-Item -Path $StateKey -Force | Out-Null
        Set-ItemProperty -Path $StateKey -Name $StateValue -Value $inf
        Log "driver published as $inf"
    }

    $new = @(Get-TapAdapter | Where-Object { $_.InterfaceGuid -notin $before })
    if ($new.Count -eq 1 -and $AdapterName -and $new[0].Name -ne $AdapterName) {
        if (Get-NetAdapter -Name $AdapterName -IncludeHidden -ErrorAction SilentlyContinue) {
            Log "an adapter named '$AdapterName' already exists; leaving the new adapter as '$($new[0].Name)'"
        } else {
            Rename-NetAdapter -Name $new[0].Name -NewName $AdapterName
            Log "renamed adapter '$($new[0].Name)' to '$AdapterName'"
        }
    }
}

function Remove-Driver {
    $inf = (Get-ItemProperty -Path $StateKey -Name $StateValue -ErrorAction SilentlyContinue).$StateValue
    if (-not $inf) {
        Log 'TAP-Windows6 driver was not installed by vxlan-tap; leaving it in place'
        return
    }
    if ($inf -notin @(Get-TapDriverInf)) {
        # Removed by hand since; the oemNN.inf name may now belong to another driver.
        Log "$inf is no longer a TAP-Windows6 driver package; leaving drivers in place"
        Remove-ItemProperty -Path $StateKey -Name $StateValue -ErrorAction SilentlyContinue
        return
    }

    Log 'removing TAP-Windows6 adapters'
    & $Devcon remove $HardwareId
    if ($LASTEXITCODE -ge 2) { Log "devcon remove exited with code $LASTEXITCODE" }

    Log "removing driver package $inf"
    & pnputil.exe /delete-driver $inf /uninstall /force
    if ($LASTEXITCODE -ne 0) { Log "pnputil exited with code $LASTEXITCODE" }

    Remove-ItemProperty -Path $StateKey -Name $StateValue -ErrorAction SilentlyContinue
}

# YAML single-quoted scalar.
function Quote([string]$s) { "'" + $s.Replace("'", "''") + "'" }

function Write-Config {
    $remotes = @()
    foreach ($entry in $RemoteIps -split ',') {
        $entry = $entry.Trim()
        if (-not $entry) { continue }
        $ip = $null
        if (-not [System.Net.IPAddress]::TryParse($entry, [ref]$ip)) {
            Log "VXLAN_REMOTE_IPS entry '$entry' is not a single address; leaving it out of the config"
        } elseif ($remotes -and $ip.AddressFamily -ne $remotes[0].AddressFamily) {
            # vxlan-tap binds one local address, so all peers share its family.
            Log "VXLAN_REMOTE_IPS entry '$entry' is not the same address family as $($remotes[0]); leaving it out of the config"
        } else {
            $remotes += $ip
        }
    }
    if (-not $remotes) {
        Log 'VXLAN_REMOTE_IPS has no single addresses; not writing a config'
        return
    }

    if ($LocalIp) {
        $local = $LocalIp
    } else {
        $local = Find-NetRoute -RemoteIPAddress $remotes[0].ToString() -ErrorAction SilentlyContinue |
            Where-Object { $_.IPAddress } | Select-Object -First 1 -ExpandProperty IPAddress
        if (-not $local) { throw "no local address routes to $($remotes[0]); set VXLAN_LOCAL_IP" }
        Log "using local address $local, the source address for $($remotes[0])"
    }

    # Name the adapter only if it exists; otherwise vxlan-tap picks the first TAP adapter.
    $tap = ''
    if ($AdapterName -and (Get-NetAdapter -Name $AdapterName -IncludeHidden -ErrorAction SilentlyContinue)) {
        $tap = $AdapterName
    }

    $lines = @(
        '# vxlan-tap configuration, written by the installer. See config.example.yaml.'
        "local_ip: $(Quote $local)"
        'remote_ips:'
        $remotes | ForEach-Object { "  - $(Quote $_.ToString())" }
        "vni: $Vni"
        "port: $Port"
        "tap: $(Quote $tap)"
        'pin_remote_route: true'
        'log_level: info'
        "log_file: $(Quote (Join-Path $PSScriptRoot 'vxlan-tap.log'))"
    )
    $content = ($lines -join "`r`n") + "`r`n"

    if (Test-Path $ConfigPath) {
        if ((Get-Content -Raw $ConfigPath) -eq $content) {
            Log "$ConfigPath is already up to date"
            return
        }
        Copy-Item $ConfigPath "$ConfigPath.bak" -Force
        Log "saved the existing config as $ConfigPath.bak"
    }
    [System.IO.File]::WriteAllText($ConfigPath, $content)
    Log "wrote $ConfigPath"
}

function Install-Service {
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        Log "service $ServiceName already exists; leaving its configuration unchanged"
    } else {
        if (-not (Test-Path $ConfigPath)) {
            throw "no config at $ConfigPath; pass VXLAN_REMOTE_IPS to generate one"
        }
        & $Exe install -config $ConfigPath
        if ($LASTEXITCODE -ne 0) { throw "vxlan-tap install failed with exit code $LASTEXITCODE" }
    }
    if ((Get-Service -Name $ServiceName).Status -eq 'Running') { return }
    & $Exe start
    if ($LASTEXITCODE -ne 0) {
        Log "service $ServiceName did not start (exit code $LASTEXITCODE); it starts at the next boot"
    }
}

function Remove-Service {
    $svc = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'"
    if (-not $svc) { return }
    if ($svc.PathName.IndexOf($Exe, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
        Log "service $ServiceName runs $($svc.PathName); leaving it in place"
        return
    }
    & $Exe uninstall
    if ($LASTEXITCODE -ne 0) { Log "vxlan-tap uninstall exited with code $LASTEXITCODE" }
}

try {
    switch ($Action) {
        'InstallDriver' { Install-Driver }
        'RemoveDriver' { Remove-Driver }
        'WriteConfig' { Write-Config }
        'InstallService' { Install-Service }
        'RemoveService' { Remove-Service }
    }
    exit 0
} catch {
    Write-Output "vxlan-tap setup: $Action failed: $_"
    exit 1
}
