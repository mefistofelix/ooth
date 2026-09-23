# Portable test runtime only; no installation or global PATH changes.
$ErrorActionPreference = 'Stop'
$repository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$destination = Join-Path $repository 'build/runtime-trueasync'
New-Item -ItemType Directory -Force -Path $destination | Out-Null
$archive = Join-Path $destination 'php-trueasync-0.10.0-php8.6-windows-x64.zip'
if (-not (Test-Path -LiteralPath $archive)) {
    Invoke-WebRequest 'https://github.com/true-async/releases/releases/download/v0.10.0/php-trueasync-0.10.0-php8.6-windows-x64.zip' -OutFile $archive
}
if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLower() -ne 'c25923c3474c30c83d89719bf65d13b196b07e0a0774d6535ac9a33c64a1a56a') {
    throw 'TrueAsync archive checksum mismatch'
}
Expand-Archive -LiteralPath $archive -DestinationPath (Join-Path $destination 'windows') -Force
