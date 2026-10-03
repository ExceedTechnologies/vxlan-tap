# Builds the vxlan-tap MSIs into installer\bin\Release.
# Needs Go and the .NET SDK (8 or later); the WiX toolset comes from NuGet.
param(
    [string]$Version = '1.0.0',
    [ValidateSet('x64', 'ARM64')]
    [string[]]$Platform = @('x64', 'ARM64')
)
$ErrorActionPreference = 'Stop'

foreach ($p in $Platform) {
    dotnet build "$PSScriptRoot\vxlan-tap.wixproj" -c Release -p:Platform=$p -p:ProductVersion=$Version
    if ($LASTEXITCODE -ne 0) { throw "build for $p failed" }
}
Get-ChildItem "$PSScriptRoot\bin" -Recurse -Filter *.msi | ForEach-Object FullName
