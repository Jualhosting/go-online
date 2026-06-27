$url = "https://goinstant.my.id/downloads/goinstant-windows.exe"
$destDir = "$env:USERPROFILE\.goinstant"
$destFile = "$destDir\goinstant.exe"

if (!(Test-Path $destDir)) {
    New-Item -ItemType Directory -Force -Path $destDir | Out-Null
}

Write-Host "Downloading goinstant from $url..."
Invoke-WebRequest -Uri $url -OutFile $destFile

# Add to user PATH if not already present
$path = [Environment]::GetEnvironmentVariable("Path", "User")
if ($path -notlike "*$destDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$path;$destDir", "User")
    $env:Path = "$env:Path;$destDir"
    Write-Host "goinstant added to User PATH."
}

Write-Host "`ngoinstant installed successfully!"
Write-Host "Please RESTART your terminal (PowerShell/CMD) and you can now run:"
Write-Host "  goinstant expose --port 8080"
Write-Host "  goinstant deploy --dir ./folder-kamu"
