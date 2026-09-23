#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

# A dedicated compiler: never modify the machine's Go installation.
case "$(uname -s)" in
  Linux) host_os=linux; extension=tar.gz ;;
  MINGW*|MSYS*) host_os=windows; extension=zip ;;
  *) echo 'Build on Linux or Windows (Git Bash).' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) host_arch=amd64 ;;
  aarch64|arm64) host_arch=arm64 ;;
  *) echo 'Unsupported host architecture.' >&2; exit 1 ;;
esac
case "$host_os-$host_arch" in
  linux-amd64) checksum=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445 ;;
  linux-arm64) checksum=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec ;;
  windows-amd64) checksum=a3911b5e0e1b1053f25ed0675f4c1c6aad1e2bfcf253df2b9be4caabd2edd95d ;;
  windows-arm64) checksum=13b69b87bb0e83f96bc68560a8cace7f0343b1e03469f1110ea18d17e3234069 ;;
esac
archive="go1.27.1.$host_os-$host_arch.$extension"
directory="$PWD/build/$host_os-$host_arch"
mkdir -p "$directory" bin
printf 'module ooth-build-cache\n\ngo 1.27.0\n' > build/go.mod
if [[ ! -f "$directory/.extracted" ]]; then
  curl --fail --location --retry 3 "https://go.dev/dl/$archive" -o "$directory/$archive"
  printf '%s  %s\n' "$checksum" "$directory/$archive" | sha256sum --check
  if [[ $extension == zip ]]; then
    unzip -oq "$directory/$archive" -d "$directory"
  else
    tar -xzf "$directory/$archive" -C "$directory"
  fi
  touch "$directory/.extracted"
fi
export GOROOT="$directory/go"
if [[ $host_os == windows ]]; then GOROOT=$(cygpath -w "$GOROOT"); fi
export CGO_ENABLED=0 GOTOOLCHAIN=local
unset GOOS GOARCH
compiler="$directory/go/bin/go"
"$compiler" run ./tools/patchgo -goroot "$GOROOT"
"$compiler" mod verify
"$compiler" test -timeout 60s ./...
"$compiler" vet ./...

version=${VERSION:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo dev)}
cp THIRD_PARTY_NOTICES.md bin/THIRD_PARTY_NOTICES.md
for target_arch in amd64 arm64; do
  suffix=''
  if [[ $host_os == windows ]]; then suffix=.exe; fi
  GOOS=$host_os GOARCH=$target_arch "$compiler" build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "bin/ooth-$host_os-$target_arch$suffix" .
done
