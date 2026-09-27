$ErrorActionPreference = 'Stop'
$SERVER_URL = '__BOXES_PUBLIC_URL__'

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    'X64' { $arch = 'amd64' }
    'Arm64' { $arch = 'arm64' }
    default { throw "Unsupported Windows architecture: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
}

$binary = "boxes-windows-$arch.exe"
$destinationDir = Join-Path $env:LOCALAPPDATA 'boxes\bin'
$destination = Join-Path $destinationDir 'boxes.exe'
New-Item -ItemType Directory -Force -Path $destinationDir | Out-Null
Invoke-WebRequest -Uri "$($SERVER_URL.TrimEnd('/'))/install/cli/$binary" -OutFile $destination

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$pathEntries = @($userPath -split ';' | Where-Object { $_ })
if ($pathEntries -notcontains $destinationDir) {
    [Environment]::SetEnvironmentVariable('Path', (@($pathEntries) + $destinationDir) -join ';', 'User')
}
$env:Path = "$destinationDir;$env:Path"

Write-Host "Installed boxes to $destination"
Write-Host "Next steps: boxes login --server $SERVER_URL --key-stdin"
& $destination --json skill install
if ($LASTEXITCODE -ne 0) { throw "boxes skill install failed with exit code $LASTEXITCODE" }
