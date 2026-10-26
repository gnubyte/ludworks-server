#Requires -Version 5.1
<#
.SYNOPSIS
  Installs (or upgrades) Ludworks as a Windows service.

.DESCRIPTION
  Run this from the folder you extracted ludworks_<version>_windows_amd64.zip into, in an
  elevated PowerShell ("Run as administrator"). It

    1. copies ludworks.exe and its documents to "$env:ProgramFiles\Ludworks"
       (a folder only administrators can write to, so nobody else can replace the
       binary the service runs),
    2. registers the "Ludworks" service (virtual account NT SERVICE\Ludworks, delayed
       automatic start, restart on failure) with "ludworks.exe service install",
       or, if it already exists, stops it for the upgrade,
    3. starts the service,
    4. optionally adds an inbound Windows Firewall rule for TCP 8443 (-AddFirewallRule),
       limited to devices on the local subnet.

  An existing "Ludworks" service that runs ludworks.exe from any other folder is refused:
  remove it with sc.exe first (the message says how).

  Data lives in "$env:ProgramData\Ludworks" and is never touched by this script.

.PARAMETER AddFirewallRule
  Also allow inbound TCP 8443 to ludworks.exe from devices on the local subnet (remote
  address LocalSubnet) on private and domain networks. Without it, an existing rule of
  that name is narrowed to the local subnet (older versions allowed any address). For
  wider access (for example a VPN range) add a separate rule of your own; see
  README-windows.txt.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install-service.ps1 -AddFirewallRule
#>
[CmdletBinding()]
param(
  [switch]$AddFirewallRule
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$ServiceName = 'Ludworks'
# The installer (netsh) and this script create the rule under the same display name.
$FirewallRuleName = 'Ludworks (HTTPS 8443)'
$Port = 8443
$InstallDir = Join-Path $env:ProgramFiles 'Ludworks'
$DataDir = Join-Path $env:ProgramData 'Ludworks'
$Payload = @('ludworks.exe', 'README-windows.txt', 'LICENSE', 'config.example.json', 'install-service.ps1', 'uninstall-service.ps1')

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Error 'This script must run elevated. Right-click PowerShell, choose "Run as administrator", and run it again.'
  exit 1
}

$source = $PSScriptRoot
$sourceExe = Join-Path $source 'ludworks.exe'
if (-not (Test-Path -LiteralPath $sourceExe -PathType Leaf)) {
  Write-Error "ludworks.exe was not found next to this script ($source)."
  exit 1
}

$exe = Join-Path $InstallDir 'ludworks.exe'
$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue

# The program of a service command line such as "C:\dir\ludworks.exe" serve --data X.
function Get-ServiceProgram([string]$ImagePath) {
  $p = $ImagePath.Trim()
  if ($p.StartsWith('"')) {
    $end = $p.IndexOf('"', 1)
    if ($end -lt 0) { return $p.Substring(1) }
    return $p.Substring(1, $end - 1)
  }
  $space = $p.IndexOf(' ')
  if ($space -ge 0) { return $p.Substring(0, $space) }
  return $p
}

# Upgrade only a service that runs the binary in Program Files. One registered from another
# folder (C:\Ludworks, Downloads, ...) may run a binary other users could replace; this script
# would otherwise keep starting it.
if ($existing) {
  $key = "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName"
  $registered = Get-ServiceProgram ([string](Get-ItemProperty -LiteralPath $key -Name ImagePath).ImagePath)
  if ($registered -ine $exe) {
    Write-Error ("The existing $ServiceName service runs `"$registered`", not `"$exe`". Other users may " +
      'have been able to change programs in that folder, so do not run anything from it. Remove the old ' +
      "service with: sc.exe stop $ServiceName; sc.exe delete $ServiceName -- then run this script again " +
      "(your data in $DataDir is kept; re-apply service options such as --data or --listen afterwards).")
    exit 1
  }
}

if ($existing -and $existing.Status -ne 'Stopped') {
  Write-Host "Stopping the $ServiceName service for the upgrade..."
  Stop-Service -Name $ServiceName -Force
  $existing.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
}

$sameFolder = (Resolve-Path -LiteralPath $source).Path.TrimEnd('\') -ieq $InstallDir.TrimEnd('\')
if (-not $sameFolder) {
  Write-Host "Copying Ludworks to $InstallDir ..."
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  foreach ($name in $Payload) {
    $from = Join-Path $source $name
    if (Test-Path -LiteralPath $from -PathType Leaf) {
      Copy-Item -LiteralPath $from -Destination (Join-Path $InstallDir $name) -Force
    }
  }
}

if (-not $existing) {
  Write-Host "Registering the $ServiceName service..."
  & $exe service install
  if ($LASTEXITCODE -ne 0) { throw "ludworks.exe service install failed (exit code $LASTEXITCODE)" }
} else {
  Write-Host "The $ServiceName service is already registered; keeping its configuration."
}

Write-Host "Starting the $ServiceName service..."
& $exe service start
if ($LASTEXITCODE -ne 0) { throw "ludworks.exe service start failed (exit code $LASTEXITCODE)" }

if ($AddFirewallRule) {
  Get-NetFirewallRule -DisplayName $FirewallRuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  # LocalSubnet: only on-link addresses (the LAN's IPv4 subnet and IPv6 prefixes), not every
  # address that reaches a private-profile interface (global IPv6, VPNs, port forwards).
  New-NetFirewallRule -DisplayName $FirewallRuleName `
    -Description 'Allows browsers and apps on the same local subnet to reach the Ludworks server.' `
    -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port -Program $exe `
    -RemoteAddress LocalSubnet -Profile Private, Domain | Out-Null
  Write-Host "Firewall rule '$FirewallRuleName' added (TCP $Port, local subnet only, private and domain networks)."
} else {
  # Rules created by older versions allowed any remote address: narrow them on upgrade.
  $rules = @(Get-NetFirewallRule -DisplayName $FirewallRuleName -ErrorAction SilentlyContinue)
  if ($rules.Count -gt 0) {
    $rules | Set-NetFirewallRule -RemoteAddress LocalSubnet
    Write-Host "Firewall rule '$FirewallRuleName' is limited to the local subnet."
  }
}

& $exe version
$tokenFile = Join-Path $DataDir 'setup-token.txt'
Write-Host ''
Write-Host 'Ludworks is running as a Windows service.'
Write-Host ''
Write-Host '  1. Read the one-time setup token (in this elevated window):'
Write-Host "       Get-Content `"$tokenFile`""
Write-Host "     It is also in the service log: $DataDir\ludworks.log"
Write-Host "  2. Open https://localhost:$Port/ and compare the certificate fingerprint with the log."
Write-Host '  3. Create the first administrator with the setup token.'
Write-Host ''
Write-Host "Configuration: $DataDir\config.json (see config.example.json in $InstallDir)."
Write-Host 'Documentation: https://ludworks.readthedocs.io'
