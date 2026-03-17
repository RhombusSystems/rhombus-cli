# Rhombus CLI installer for Windows
# Usage: irm https://raw.githubusercontent.com/RhombusSystems/rhombus-cli/main/install.ps1 | iex

$ErrorActionPreference = "Stop"

$repo = "RhombusSystems/rhombus-cli"
$installDir = "$env:LOCALAPPDATA\rhombus\bin"

# Detect architecture
$arch = if ([Environment]::Is64BitOperatingSystem) {
    if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
} else {
    Write-Error "32-bit systems are not supported."
    return
}

# Get latest release tag
Write-Host "Fetching latest release..." -ForegroundColor Cyan
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
$version = $release.tag_name -replace '^v', ''
Write-Host "Latest version: $version" -ForegroundColor Green

# Find the right asset
$assetName = "rhombus-cli_${version}_windows_${arch}.zip"
$asset = $release.assets | Where-Object { $_.name -eq $assetName }
if (-not $asset) {
    Write-Error "Could not find release asset: $assetName"
    return
}

# Download
$tempDir = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "rhombus-install-$(Get-Random)")
$zipPath = Join-Path $tempDir $assetName

Write-Host "Downloading $assetName..." -ForegroundColor Cyan
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zipPath

# Extract
Write-Host "Extracting..." -ForegroundColor Cyan
Expand-Archive -Path $zipPath -DestinationPath $tempDir -Force

# Install
if (-not (Test-Path $installDir)) {
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}
Copy-Item -Path (Join-Path $tempDir "rhombus.exe") -Destination $installDir -Force

# Clean up
Remove-Item -Path $tempDir -Recurse -Force

# Add to PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
    Write-Host "Adding $installDir to PATH..." -ForegroundColor Cyan
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
    $env:Path = "$env:Path;$installDir"
}

Write-Host ""
Write-Host "Rhombus CLI v$version installed to $installDir\rhombus.exe" -ForegroundColor Green
Write-Host "Run 'rhombus --help' to get started." -ForegroundColor Green
Write-Host "You may need to restart your terminal for PATH changes to take effect." -ForegroundColor Yellow
