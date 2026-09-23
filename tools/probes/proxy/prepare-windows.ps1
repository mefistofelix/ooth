# Test runtimes only. No installation and no change to the global PATH.
$ErrorActionPreference = 'Stop'
$repository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$destination = Join-Path $repository 'build/runtime-proxy'
New-Item -ItemType Directory -Force -Path $destination | Out-Null
$downloads = @(
    @('https://github.com/caddyserver/caddy/releases/download/v2.11.4/caddy_2.11.4_windows_amd64.zip', '1708333f79e274c7697285afe6d592ab39314e0b131e9ec6bea08ad27df62ebf', 'caddy-windows'),
    @('https://nginx.org/download/nginx-1.30.5.zip', 'e5afe28b6a50bec92c478bfe1a4d3758206b80fb77159277bc5c4e88955c2a35', '.'),
    @('https://windows.php.net/downloads/releases/php-8.4.26-nts-Win32-vs17-x64.zip', 'da68394f9193b7f6b89d0c76861a4034ae10efee7fd55a7255d8118c2acf70d7', 'php-windows')
)
foreach ($download in $downloads) {
    $archive = Join-Path $destination ([IO.Path]::GetFileName($download[0]))
    if (-not (Test-Path -LiteralPath $archive)) { Invoke-WebRequest $download[0] -OutFile $archive }
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLower() -ne $download[1]) { throw "Checksum mismatch: $archive" }
    Expand-Archive -LiteralPath $archive -DestinationPath (Join-Path $destination $download[2]) -Force
}
$env:CGO_ENABLED = '0'
& (Join-Path $repository 'build/windows-amd64/go/bin/go.exe') build -o (Join-Path $destination 'php-worker.exe') (Join-Path $repository 'tools/probes/php_worker_windows.go')
if ($LASTEXITCODE -ne 0) { throw 'PHP launcher build failed; run build.sh first' }
