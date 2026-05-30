#Requires -Version 5.1
# install.ps1 — installs orca CLI on Windows.
# Usage: iwr https://raw.githubusercontent.com/micronwave/orca/main/install.ps1 | iex
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$Repo       = 'micronwave/orca'
$InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\orca'
$BinaryPath = Join-Path $InstallDir 'orca.exe'

# Detect architecture.
$ProcessorArch = $env:PROCESSOR_ARCHITECTURE
if (-not $ProcessorArch) {
    # Fallback for environments where PROCESSOR_ARCHITECTURE is unset.
    $ProcessorArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
}

$GoArch = switch ($ProcessorArch.ToUpper()) {
    'AMD64' { 'amd64' }
    'X86_64' { 'amd64' }
    default {
        Write-Error (
            "Unsupported architecture: $ProcessorArch. " +
            "Only AMD64 is supported. " +
            "Visit https://github.com/$Repo/releases for a manual download."
        )
        exit 1
    }
}

$AssetName  = "orca-windows-$GoArch.exe"
$DownloadUrl = "https://github.com/$Repo/releases/latest/download/$AssetName"

Write-Host "Downloading $DownloadUrl ..."

if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir | Out-Null
}

Invoke-WebRequest -Uri $DownloadUrl -OutFile $BinaryPath -UseBasicParsing

Write-Host "Installed: $BinaryPath"

# Add install directory to the user PATH if not already present.
$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $UserPath) {
    $UserPath = ''
}

$NormalizePath = {
    param([string]$PathValue)
    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return ''
    }

    $Trimmed = $PathValue.Trim().Trim('"')
    while ($Trimmed.EndsWith('\')) {
        $Trimmed = $Trimmed.Substring(0, $Trimmed.Length - 1)
    }
    return $Trimmed.ToLowerInvariant()
}

$NormalizedInstallDir = & $NormalizePath $InstallDir
$PathParts = $UserPath -split ';' | Where-Object { $_ -ne '' }
$NormalizedPathParts = @($PathParts | ForEach-Object { & $NormalizePath $_ })

if ($NormalizedPathParts -notcontains $NormalizedInstallDir) {
    $NewPath = ($PathParts + $InstallDir) -join ';'
    [Environment]::SetEnvironmentVariable('Path', $NewPath, 'User')
    Write-Host "Added $InstallDir to your user PATH."
    Write-Host "Open a new terminal for the change to take effect."
}

Write-Host ''
Write-Host 'Installation complete.'
Write-Host 'Next steps:'
Write-Host '  1. Open a new terminal to pick up the updated PATH.'
Write-Host '  2. Run: orca --help'
