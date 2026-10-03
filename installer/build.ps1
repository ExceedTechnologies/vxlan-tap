# Builds the vxlan-tap MSIs into installer\bin\Release.
# Needs Go and the .NET SDK (8 or later); the WiX toolset comes from NuGet.
#
# vxlan-tap.exe, setup-helper.ps1 and the MSI are signed with Azure Artifact Signing, which needs
# signtool.exe from the Windows SDK, the Artifact Signing client tools
# (winget install Microsoft.Azure.ArtifactSigningClientTools) and an Azure sign-in that holds the
# signer role on the certificate profile (az login, or the AZURE_* environment variables).
# -NoSign skips signing for local test builds.
param(
    [string]$Version = '1.0.0',
    [ValidateSet('x64', 'ARM64')]
    [string[]]$Platform = @('x64', 'ARM64'),
    [switch]$NoSign,
    # Endpoint, CodeSigningAccountName and CertificateProfileName for Artifact Signing.
    [string]$SigningMetadata = "$PSScriptRoot\..\metadata.json",
    [string]$SigningDlib = "$env:LOCALAPPDATA\Microsoft\MicrosoftArtifactSigningClientTools\Azure.CodeSigning.Dlib.dll",
    # Defaults to the x64 signtool from the newest Windows SDK.
    [string]$SignTool
)
$ErrorActionPreference = 'Stop'

$signArgs = @()
if (-not $NoSign) {
    if (-not $SignTool) {
        $SignTool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\10.*\x64\signtool.exe" -ErrorAction SilentlyContinue |
            Sort-Object { [version]$_.Directory.Parent.Name } | Select-Object -Last 1 -ExpandProperty FullName
        if (-not $SignTool) { throw 'signtool.exe not found; install the Windows SDK or pass -SignTool' }
    }
    foreach ($f in $SignTool, $SigningDlib, $SigningMetadata) {
        if (-not (Test-Path $f)) { throw "$f not found" }
    }
    $signArgs = @(
        '-p:SignOutput=true'
        "-p:SignToolExe=$((Resolve-Path $SignTool).Path)"
        "-p:SignDlib=$((Resolve-Path $SigningDlib).Path)"
        "-p:SignMetadata=$((Resolve-Path $SigningMetadata).Path)"
    )
}

foreach ($p in $Platform) {
    dotnet build "$PSScriptRoot\vxlan-tap.wixproj" -c Release -p:Platform=$p -p:ProductVersion=$Version @signArgs
    if ($LASTEXITCODE -ne 0) { throw "build for $p failed" }
}
Get-ChildItem "$PSScriptRoot\bin" -Recurse -Filter *.msi | ForEach-Object FullName
