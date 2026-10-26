<#
.SYNOPSIS
  Build release binaries for linux/amd64, linux/arm64 and windows/amd64 into dist\.
.EXAMPLE
  .\scripts\build.ps1
  .\scripts\build.ps1 -Version 1.2.3 -Targets windows/amd64
.NOTES
  Static (CGO_ENABLED=0), -trimpath, reproducible (-buildid=), -mod=readonly.
#>
param(
  [string]$Version = $env:VERSION,
  [string[]]$Targets = @('linux/amd64', 'linux/arm64', 'windows/amd64')
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
  if (-not $Version) {
    $Version = ''
    try { $Version = (git describe --tags --always --dirty 2>$null) } catch { }
    if ($Version) { $Version = $Version.TrimStart('v') } else { $Version = '0.0.0-dev' }
  }
  # The version is spliced into -ldflags: allow only a conservative character set.
  if ($Version -notmatch '^[0-9A-Za-z][0-9A-Za-z.+_-]*$') { throw "invalid Version: $Version" }

  $ldflags = "-s -w -buildid= -X ludworks/internal/version.Version=$Version"
  New-Item -ItemType Directory -Force -Path dist | Out-Null
  $saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
  try {
    foreach ($t in $Targets) {
      $os, $arch = $t.Split('/')
      $out = "dist/ludworks-$os-$arch"
      if ($os -eq 'windows') { $out += '.exe' }
      Write-Host "==> $out ($Version)"
      $env:CGO_ENABLED = '0'; $env:GOOS = $os; $env:GOARCH = $arch
      & go build -mod=readonly -trimpath -ldflags $ldflags -o $out ./cmd/ludworks
      if ($LASTEXITCODE -ne 0) { throw "go build failed for $t" }
    }
  } finally {
    $env:CGO_ENABLED = $saved.CGO_ENABLED; $env:GOOS = $saved.GOOS; $env:GOARCH = $saved.GOARCH
  }

  $lines = Get-ChildItem dist -Filter 'ludworks-*' | Sort-Object Name | ForEach-Object {
    '{0}  {1}' -f (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLower(), $_.Name
  }
  # LF line endings so `sha256sum -c` works on Linux too.
  [System.IO.File]::WriteAllText((Join-Path $root 'dist/SHA256SUMS'), (($lines -join "`n") + "`n"))
  Write-Host "==> dist/SHA256SUMS"
  Get-Content dist/SHA256SUMS
} finally {
  Pop-Location
}
