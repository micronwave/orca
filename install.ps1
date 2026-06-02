#Requires -Version 5.1
# install.ps1 — installs orca CLI on Windows.
# Usage: iwr https://raw.githubusercontent.com/micronwave/orca/main/install.ps1 | iex
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Respect NO_COLOR (https://no-color.org/) and dumb terminals.
$script:UseColor = (-not $env:NO_COLOR) -and ($env:TERM -ne 'dumb')

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

$AssetName   = "orca-windows-$GoArch.exe"
$DownloadUrl = "https://github.com/$Repo/releases/latest/download/$AssetName"
$ChecksumUrl = "$DownloadUrl.sha256"

function Write-Col {
    param([string]$Text, [ConsoleColor]$Color = 'Cyan')
    if ($script:UseColor) { Write-Host $Text -ForegroundColor $Color }
    else { Write-Host $Text }
}

function Show-Banner {
    Write-Col "   .            _"
    Write-Col "  - _ _  _  ( ) _"
    Write-Col "- ( _ )( _ )( _ )| |( _ )"
    Write-Col "-  (_ ) | | |  _/| || _ |"
    Write-Col " - ( _ ) |_| | _ | |_|( _ )"
    Write-Col "  -  -   -   -   -   -"
    Write-Host ""
}

function Write-Status {
    param([string]$Message, [ConsoleColor]$Color = "Cyan")
    if ($script:UseColor) {
        Write-Host "[orca] " -NoNewline -ForegroundColor Cyan
        Write-Host $Message -ForegroundColor $Color
    } else {
        Write-Host "[orca] $Message"
    }
}

Show-Banner

Write-Status "Downloading $AssetName ..."

# Download to a temp file so a failed/partial download never corrupts an
# existing install. Move to the final path only after checksum passes.
$TmpFile     = [System.IO.Path]::GetTempFileName()
$TmpChecksum = [System.IO.Path]::GetTempFileName()

try {
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $TmpFile -UseBasicParsing

    Write-Status "Verifying checksum ..."
    Invoke-WebRequest -Uri $ChecksumUrl -OutFile $TmpChecksum -UseBasicParsing
    $Expected = (Get-Content $TmpChecksum -Raw).Trim().ToLowerInvariant()
    $Actual   = (Get-FileHash -Path $TmpFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) {
        Write-Error "✗ Checksum mismatch.`n  Expected: $Expected`n  Got:      $Actual"
        exit 1
    }
    Write-Status "✓ Checksum verified." "Green"

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir | Out-Null
    }
    Move-Item -Path $TmpFile -Destination $BinaryPath -Force
    Write-Status "✓ Installed: $BinaryPath" "Green"
} finally {
    Remove-Item -Path $TmpFile, $TmpChecksum -ErrorAction SilentlyContinue
}

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
    # Expand %VAR% references before comparing so that equivalent paths with
    # different representations (e.g. %LOCALAPPDATA% vs the literal expansion)
    # are treated as equal.
    $Expanded = [Environment]::ExpandEnvironmentVariables($PathValue)
    $Trimmed  = $Expanded.Trim().Trim('"')
    while ($Trimmed.EndsWith('\')) {
        $Trimmed = $Trimmed.Substring(0, $Trimmed.Length - 1)
    }
    return $Trimmed.ToLowerInvariant()
}

$NormalizedInstallDir = & $NormalizePath $InstallDir
$PathParts            = $UserPath -split ';' | Where-Object { $_ -ne '' }
$NormalizedPathParts  = @($PathParts | ForEach-Object { & $NormalizePath $_ })

if ($NormalizedPathParts -notcontains $NormalizedInstallDir) {
    $NewPath = ($PathParts + $InstallDir) -join ';'
    [Environment]::SetEnvironmentVariable('Path', $NewPath, 'User')
    Write-Status "Added $InstallDir to your user PATH."
    Write-Col "Open a new terminal for the change to take effect." Yellow
}

# Verify the binary actually executes before declaring success.
Write-Host ''
Write-Status "Verifying installation ..."
& $BinaryPath --help

Write-Host ''
Write-Status "✦ Installation complete!" "Green"
Write-Host "Open a new terminal to use orca from your PATH."
