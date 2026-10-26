<#
.SYNOPSIS
  Build the Ludworks release packages into dist\ on a Windows machine with Go 1.27+.

.DESCRIPTION
  The Windows counterpart of scripts/package.sh, with the same outputs:

    dist\ludworks_<version>_amd64.deb
    dist\ludworks_<version>_arm64.deb
    dist\ludworks_<version>_windows_amd64.zip   (ludworks.exe, README-windows.txt, LICENSE,
                                                config.example.json, install/uninstall scripts)
    dist\SHA256SUMS

  dist\stage\ keeps the unpacked payloads. With -Installer the Inno Setup script
  packaging\windows\ludworks.iss is compiled too (dist\ludworks_<version>_windows_amd64_setup.exe,
  added to SHA256SUMS). The .deb files are assembled by the pure-Go tools/mkdeb, so no Linux
  tools are needed; the maintainer scripts must keep LF line endings (.gitattributes does that,
  and mkdeb refuses CRLF).

.PARAMETER Version
  Semantic version. Default: the VERSION file, else `git describe`.

.PARAMETER Targets
  What to build: 'deb', 'windows' or both (default).

.PARAMETER Installer
  Also compile the Inno Setup installer (needs Inno Setup 6.3+; see -Iscc).

.PARAMETER Iscc
  Path to ISCC.exe. Default: ISCC.exe on PATH or the standard Inno Setup 6 install folders.

.EXAMPLE
  .\scripts\package.ps1
  .\scripts\package.ps1 -Targets windows -Installer
  $env:SOURCE_DATE_EPOCH = '1767225600'; .\scripts\package.ps1 -Version 0.2.0-rc.1

.NOTES
  Set $env:GO to use a Go binary that is not on PATH.
#>
[CmdletBinding()]
param(
  [string]$Version = $env:VERSION,
  [ValidateSet('deb', 'windows')]
  [string[]]$Targets = @('deb', 'windows'),
  [switch]$Installer,
  [string]$Iscc = ''
)

$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'package.ps1 is for Windows hosts; use scripts/package.sh elsewhere.' }

$root = Split-Path -Parent $PSScriptRoot
$go = 'go'
if ($env:GO) { $go = $env:GO }
$utf8 = New-Object System.Text.UTF8Encoding($false)

function Invoke-Checked {
  param([string]$What, [scriptblock]$Block)
  & $Block
  if ($LASTEXITCODE -ne 0) { throw "$What failed (exit code $LASTEXITCODE)" }
}

Push-Location $root
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED; SOURCE_DATE_EPOCH = $env:SOURCE_DATE_EPOCH }
$syso = Join-Path $root 'cmd\ludworks\rsrc_windows_amd64.syso'
try {
  # ------------------------------------------------------------------------------------
  # Version
  if (-not $Version) {
    if (Test-Path -LiteralPath VERSION) {
      $Version = ([IO.File]::ReadAllText((Join-Path $root 'VERSION'))).Trim()
    } else {
      $described = ''
      try { $described = (& git describe --tags --match 'v[0-9]*' --dirty 2>$null) } catch { }
      $Version = "$described".TrimStart('v')
      $m = [regex]::Match($Version, '^([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?)-([0-9]+)-(g[0-9a-f]+)(-dirty)?$')
      if ($m.Success) {
        $Version = '{0}+{1}.{2}' -f $m.Groups[1].Value, $m.Groups[3].Value, $m.Groups[4].Value
        if ($m.Groups[5].Success) { $Version += '.dirty' }
      } elseif ($Version.EndsWith('-dirty')) {
        $Version = $Version.Substring(0, $Version.Length - 6) + '+dirty'
      }
    }
  }
  $semver = '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
  $sv = [regex]::Match($Version, $semver)
  if (-not $sv.Success) { throw "Version '$Version' is not a semantic version (MAJOR.MINOR.PATCH[-PRE][+BUILD])" }
  $debVersion = $Version.Replace('-', '~')
  $fileVersion = '{0}.{1}.{2}.0' -f $sv.Groups[1].Value, $sv.Groups[2].Value, $sv.Groups[3].Value

  # ------------------------------------------------------------------------------------
  # Reproducible timestamps
  $epoch = $env:SOURCE_DATE_EPOCH
  if (-not $epoch) {
    try { $epoch = (& git log -1 --format=%ct 2>$null) } catch { $epoch = '' }
    if (-not $epoch) {
      $epoch = [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
      Write-Warning "not a git checkout; SOURCE_DATE_EPOCH defaults to now ($epoch)"
    }
  }
  if ($epoch -notmatch '^[0-9]+$') { throw 'SOURCE_DATE_EPOCH must be a Unix timestamp' }
  $env:SOURCE_DATE_EPOCH = $epoch
  Write-Host "==> Ludworks $Version (Debian $debVersion), SOURCE_DATE_EPOCH=$epoch"

  $stage = 'dist/stage'
  if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
  New-Item -ItemType Directory -Force -Path 'dist' | Out-Null
  Get-ChildItem -LiteralPath 'dist' -File | Where-Object {
    $_.Name -like 'ludworks_*.deb' -or $_.Name -like 'ludworks_*.zip' -or $_.Name -like 'ludworks_*_setup.exe' -or $_.Name -eq 'SHA256SUMS'
  } | Remove-Item -Force
  foreach ($d in 'tools', 'linux_amd64', 'linux_arm64', 'windows_amd64') {
    New-Item -ItemType Directory -Force -Path "$stage/$d" | Out-Null
  }

  # Host tools (pure Go) for this machine.
  $hostArch = 'amd64'
  if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $hostArch = 'arm64' }
  $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = $hostArch
  Invoke-Checked 'building tools/mkdeb' { & $go build -mod=readonly -o "$stage/tools/mkdeb.exe" ./tools/mkdeb }
  Invoke-Checked 'building tools/winres' { & $go build -mod=readonly -o "$stage/tools/winres.exe" ./tools/winres }
  $mkdeb = Join-Path $root 'dist\stage\tools\mkdeb.exe'
  $winres = Join-Path $root 'dist\stage\tools\winres.exe'

  $ldflags = "-s -w -buildid= -X ludworks/internal/version.Version=$Version"
  function Build-Ludworks([string]$os, [string]$arch, [string]$out) {
    Write-Host "==> go build $os/$arch -> $out"
    $env:CGO_ENABLED = '0'; $env:GOOS = $os; $env:GOARCH = $arch
    & $go build -mod=readonly -trimpath -buildvcs=false -ldflags $ldflags -o $out ./cmd/ludworks
    if ($LASTEXITCODE -ne 0) { throw "go build $os/$arch failed" }
  }

  $artifacts = @()

  # ------------------------------------------------------------------------------------
  if ($Targets -contains 'windows') {
    # Version/icon resources exist only for this build (never committed).
    Invoke-Checked 'winres syso' { & $winres syso -version $Version -arch amd64 -icon packaging/windows/ludworks.ico -out $syso }
    try {
      Build-Ludworks 'windows' 'amd64' "$stage/windows_amd64/ludworks.exe"
    } finally {
      Remove-Item -LiteralPath $syso -Force -ErrorAction SilentlyContinue
    }
    Invoke-Checked 'winres verify' { & $winres verify -version $Version (Join-Path $root 'dist\stage\windows_amd64\ludworks.exe') }

    # CRLF text files for Notepad and PowerShell 5.1.
    $texts = [ordered]@{
      'README-windows.txt'    = 'packaging/windows/README-windows.txt'
      'LICENSE'               = 'LICENSE'
      'config.example.json'   = 'packaging/windows/config.example.json'
      'install-service.ps1'   = 'packaging/windows/install-service.ps1'
      'uninstall-service.ps1' = 'packaging/windows/uninstall-service.ps1'
    }
    foreach ($name in $texts.Keys) {
      $text = [IO.File]::ReadAllText((Join-Path $root $texts[$name]))
      $text = ($text -replace "`r`n", "`n") -replace "`n", "`r`n"
      [IO.File]::WriteAllText((Join-Path $root "dist\stage\windows_amd64\$name"), $text, $utf8)
    }

    # Deterministic zip: fixed order and timestamps.
    $zipName = "ludworks_${Version}_windows_amd64.zip"
    $zipPath = Join-Path $root "dist\$zipName"
    Write-Host "==> dist/$zipName"
    Add-Type -AssemblyName System.IO.Compression
    $stamp = [DateTimeOffset]::FromUnixTimeSeconds([Math]::Max([long]$epoch, 315532800))
    $stamp = New-Object DateTimeOffset($stamp.UtcDateTime.Ticks, [TimeSpan]::Zero)
    $fs = [IO.File]::Open($zipPath, [IO.FileMode]::Create)
    try {
      $zip = New-Object IO.Compression.ZipArchive($fs, [IO.Compression.ZipArchiveMode]::Create)
      try {
        foreach ($name in @('ludworks.exe') + @($texts.Keys)) {
          $entry = $zip.CreateEntry($name, [IO.Compression.CompressionLevel]::Optimal)
          $entry.LastWriteTime = $stamp
          $bytes = [IO.File]::ReadAllBytes((Join-Path $root "dist\stage\windows_amd64\$name"))
          $stream = $entry.Open()
          try { $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Dispose() }
        }
      } finally { $zip.Dispose() }
    } finally { $fs.Dispose() }
    $artifacts += $zipName
  }

  # ------------------------------------------------------------------------------------
  if ($Targets -contains 'deb') {
    foreach ($arch in 'amd64', 'arm64') {
      Build-Ludworks 'linux' $arch "$stage/linux_$arch/ludworks"
      $debName = "ludworks_${Version}_$arch.deb"
      $bin = Join-Path $root "dist\stage\linux_$arch\ludworks"
      Invoke-Checked "mkdeb build $arch" {
        & $mkdeb build -spec packaging/deb/mkdeb.json -version $debVersion -arch $arch -var "BINARY=$bin" -out "dist/$debName"
      }
      Invoke-Checked "mkdeb inspect $arch" { & $mkdeb inspect "dist/$debName" | Out-Null }
      $artifacts += $debName
    }
  }

  # ------------------------------------------------------------------------------------
  if ($Installer) {
    if ($Targets -notcontains 'windows') { throw '-Installer needs the windows target' }
    if (-not $Iscc) {
      $cmd = Get-Command ISCC.exe -ErrorAction SilentlyContinue
      if ($cmd) { $Iscc = $cmd.Source }
    }
    if (-not $Iscc) {
      foreach ($candidate in @("${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe", "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe")) {
        if (Test-Path -LiteralPath $candidate) { $Iscc = $candidate; break }
      }
    }
    if (-not $Iscc) { throw 'ISCC.exe (Inno Setup 6.3+) not found; pass -Iscc <path>' }
    Write-Host "==> Inno Setup installer ($Iscc)"
    Invoke-Checked 'ISCC' {
      & $Iscc /Q "/DAppVersion=$Version" "/DFileVersion=$fileVersion" `
        "/DPayloadDir=$root\dist\stage\windows_amd64" "/DOutputDir=$root\dist" `
        (Join-Path $root 'packaging\windows\ludworks.iss')
    }
    $artifacts += "ludworks_${Version}_windows_amd64_setup.exe"
  }

  # ------------------------------------------------------------------------------------
  $lines = foreach ($name in ($artifacts | Sort-Object)) {
    '{0}  {1}' -f (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $root "dist\$name")).Hash.ToLowerInvariant(), $name
  }
  # LF line endings so `sha256sum -c` works on Linux too.
  [IO.File]::WriteAllText((Join-Path $root 'dist\SHA256SUMS'), (($lines -join "`n") + "`n"), $utf8)
  Write-Host '==> dist/SHA256SUMS'
  Get-Content -LiteralPath 'dist/SHA256SUMS'
} finally {
  Remove-Item -LiteralPath $syso -Force -ErrorAction SilentlyContinue
  foreach ($k in @($saved.Keys)) {
    if ($null -eq $saved[$k]) { Remove-Item -Path "env:$k" -ErrorAction SilentlyContinue }
    else { Set-Item -Path "env:$k" -Value $saved[$k] }
  }
  Pop-Location
}
