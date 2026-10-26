#Requires -Version 5.1
<#
.SYNOPSIS
  Removes the Ludworks Windows service and program files. Your data is kept.

.DESCRIPTION
  Run in an elevated PowerShell. It stops and unregisters the "Ludworks" service, removes
  the firewall rule added by install-service.ps1 and deletes "$env:ProgramFiles\Ludworks".
  The data directory "$env:ProgramData\Ludworks" (database, files, keys, configuration,
  backups and logs) is NOT deleted.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File "$env:ProgramFiles\Ludworks\uninstall-service.ps1"
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$ServiceName = 'Ludworks'
$FirewallRuleName = 'Ludworks (HTTPS 8443)'
$InstallDir = Join-Path $env:ProgramFiles 'Ludworks'
$DataDir = Join-Path $env:ProgramData 'Ludworks'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Error 'This script must run elevated. Right-click PowerShell, choose "Run as administrator", and run it again.'
  exit 1
}

$exe = Join-Path $InstallDir 'ludworks.exe'
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
  if (Test-Path -LiteralPath $exe -PathType Leaf) {
    # Stops the service (waiting up to 40 s) and unregisters it.
    & $exe service uninstall
    if ($LASTEXITCODE -ne 0) { throw "ludworks.exe service uninstall failed (exit code $LASTEXITCODE)" }
  } else {
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    & "$env:SystemRoot\System32\sc.exe" delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "sc.exe delete $ServiceName failed (exit code $LASTEXITCODE)" }
    Write-Host "Service $ServiceName removed."
  }
} else {
  Write-Host "The $ServiceName service is not installed."
}

$rule = Get-NetFirewallRule -DisplayName $FirewallRuleName -ErrorAction SilentlyContinue
if ($rule) {
  $rule | Remove-NetFirewallRule
  Write-Host "Firewall rule '$FirewallRuleName' removed."
}

if (Test-Path -LiteralPath $InstallDir) {
  # The service process may need a moment to release ludworks.exe after stopping.
  for ($i = 0; $i -lt 10; $i++) {
    try {
      Remove-Item -LiteralPath $InstallDir -Recurse -Force
      break
    } catch {
      if ($i -eq 9) { throw }
      Start-Sleep -Seconds 1
    }
  }
  Write-Host "Removed $InstallDir."
}

Write-Host ''
Write-Host "Your data was KEPT in $DataDir (database, files, keys, configuration and backups)."
Write-Host 'Reinstalling Ludworks picks it up again. Delete that folder yourself to remove it for good.'
