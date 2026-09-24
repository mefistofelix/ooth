#!/usr/bin/env bash
# Ubuntu 24.04 amd64 test preparation: no package installation or global PATH edits.
set -euo pipefail
cd "$(dirname "$0")/../../.."
mkdir -p tmp/runtime-proxy
cd tmp/runtime-proxy
fetch() {
  local url=$1 checksum=$2 archive=${1##*/}
  if [[ ! -f $archive ]]; then curl --fail --location "$url" -o "$archive"; fi
  printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check
}
fetch https://github.com/caddyserver/caddy/releases/download/v2.11.4/caddy_2.11.4_linux_amd64.tar.gz 527fbf917c39189a1e3b31d34fa955601680b2d5c8055d2a87b8b9588dec7bb9
mkdir -p caddy-linux nginx-linux php-linux
tar -xzf caddy_2.11.4_linux_amd64.tar.gz -C caddy-linux
fetch https://nginx.org/download/nginx-1.30.5.tar.gz 6c20565aa2325cb82216ae804f4a4ff1875179014759a381c42ddc8e11c4906d
tar -xzf nginx-1.30.5.tar.gz -C nginx-linux --strip-components=1
cd nginx-linux
./configure --prefix="$PWD/local" --with-http_v2_module --without-http_rewrite_module --without-http_gzip_module > configure.log
make -j2 > make.log
cd ../php-linux
# apt-get download verifies against the configured signed repository metadata.
apt-get download php8.3-cgi=8.3.6-0ubuntu0.24.04.11
dpkg-deb -x php8.3-cgi_8.3.6-0ubuntu0.24.04.11_amd64.deb .
./usr/bin/php-cgi8.3 -n -v
../caddy-linux/caddy version
../nginx-linux/objs/nginx -V
