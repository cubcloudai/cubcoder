<#
.SYNOPSIS
  Updates the cubcoder.exe executable from the public GitHub raw endpoint.

.DESCRIPTION
  The Windows counterpart to cc_update.sh. Redistributable: copy to any Windows
  machine and run it from PowerShell. It downloads the right binary for the
  machine's architecture, checks it is a real PE executable (not an HTML error
  page), verifies it against the published SHA256SUMS.txt, and installs it.

  Run it:
    powershell -ExecutionPolicy Bypass -File cc_update.ps1

.PARAMETER BaseUrl
  Base raw URL. Override with -BaseUrl or the CUBCODER_BASE_URL env var.

.PARAMETER Dest
  Install path for cubcoder.exe. Override with -Dest or the CUBCODER_DEST env
  var. Defaults to %LOCALAPPDATA%\cubcoder\cubcoder.exe (no admin needed).

.PARAMETER Arch
  Override arch detection (amd64|arm64). Or set CUBCODER_ARCH.
#>
[CmdletBinding()]
param(
  [string]$BaseUrl = $(if ($env:CUBCODER_BASE_URL) { $env:CUBCODER_BASE_URL } else { 'https://raw.githubusercontent.com/cubcloudai/cubcoder/main/dist' }),
  [string]$Dest    = $(if ($env:CUBCODER_DEST)     { $env:CUBCODER_DEST }     else { Join-Path $env:LOCALAPPDATA 'cubcoder\cubcoder.exe' }),
  [string]$Arch    = $env:CUBCODER_ARCH
)

$ErrorActionPreference = 'Stop'
# TLS 1.2 for older Windows PowerShell 5.1, where it is not the default.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch {}

# Detect architecture unless overridden. PROCESSOR_ARCHITECTURE reports the
# process's view; on native arm64 it is ARM64, on x64 it is AMD64. A 32-bit
# process on 64-bit Windows sees ...W6432 instead, so consult both.
if (-not $Arch) {
  $a = $env:PROCESSOR_ARCHITECTURE
  if ($env:PROCESSOR_ARCHITEW6432) { $a = $env:PROCESSOR_ARCHITEW6432 }
  switch ($a) {
    'AMD64' { $Arch = 'amd64' }
    'ARM64' { $Arch = 'arm64' }
    'x86'   { $Arch = 'amd64' }  # 32-bit shell on 64-bit Windows: ship amd64
    default { Write-Error "unsupported architecture: $a"; exit 1 }
  }
}

$Name = "cubcoder-windows-$Arch.exe"
$Url  = "$BaseUrl/$Name"

$Tmp  = [System.IO.Path]::GetTempFileName()
$Sums = [System.IO.Path]::GetTempFileName()
try {
  Write-Host "Downloading cubcoder (windows/$Arch) from $Url"
  try {
    Invoke-WebRequest -Uri $Url -OutFile $Tmp -UseBasicParsing
  } catch {
    Write-Error "download failed from ${Url}: $($_.Exception.Message)"
    exit 1
  }

  # Sanity check: a PE executable starts with "MZ" (0x4D 0x5A). An HTML error
  # page (e.g. a wrong URL hitting GitHub's file view) would not.
  $magic = [System.IO.File]::ReadAllBytes($Tmp)
  if ($magic.Length -lt 2 -or $magic[0] -ne 0x4D -or $magic[1] -ne 0x5A) {
    Write-Error "download is not a Windows (PE) executable (bad magic). Check the URL - it must point at raw.githubusercontent.com/.../dist, not the GitHub HTML file view."
    exit 1
  }

  # Verify against the checksums written by `make release`.
  try {
    Invoke-WebRequest -Uri "$BaseUrl/SHA256SUMS.txt" -OutFile $Sums -UseBasicParsing
  } catch {
    Write-Error "could not download SHA256SUMS.txt from ${BaseUrl}: $($_.Exception.Message)"
    exit 1
  }
  $expected = $null
  foreach ($line in Get-Content $Sums) {
    # Lines look like: "<sha256>  cubcoder-windows-amd64.exe"
    $parts = $line -split '\s+', 2
    if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $Name) {
      $expected = $parts[0].Trim().ToLower()
      break
    }
  }
  if (-not $expected) {
    Write-Error "no entry for $Name in SHA256SUMS.txt"
    exit 1
  }
  $actual = (Get-FileHash -Path $Tmp -Algorithm SHA256).Hash.ToLower()
  if ($actual -ne $expected) {
    Write-Error "checksum mismatch for $Name`n  expected: $expected`n  got:      $actual"
    exit 1
  }
  Write-Host "Checksum OK"

  # Install. Create the destination directory if needed, then move into place.
  $destDir = Split-Path -Parent $Dest
  if ($destDir -and -not (Test-Path $destDir)) {
    New-Item -ItemType Directory -Path $destDir -Force | Out-Null
  }
  # A running cubcoder.exe can't be overwritten; Move-Item -Force replaces an
  # idle one. Copy then remove so the temp file is cleaned up either way.
  Copy-Item -Path $Tmp -Destination $Dest -Force
  Write-Host "Installed to $Dest"

  # PATH hint: install dir is often not on PATH on a fresh machine.
  $onPath = ($env:Path -split ';') -contains $destDir
  if (-not $onPath) {
    Write-Host "NOTE: $destDir is not on your PATH. Add it, or run cubcoder by full path:"
    Write-Host "  `"$Dest`""
  }

  & $Dest --version
}
finally {
  Remove-Item -Path $Tmp, $Sums -ErrorAction SilentlyContinue
}
