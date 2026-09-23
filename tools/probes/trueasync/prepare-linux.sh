#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
directory=build/runtime-trueasync
archive=php-trueasync-0.10.0-php8.6-linux-x86_64.tar.gz
mkdir -p "$directory/linux"
if [[ ! -f "$directory/$archive" ]]; then
  curl --fail --location --retry 3 "https://github.com/true-async/releases/releases/download/v0.10.0/$archive" -o "$directory/$archive"
fi
printf '%s  %s\n' 1657eda2a086d6d9ace779979d53d161f1ad2df4df9db2cb3ec3dd90b280a8aa "$directory/$archive" | sha256sum --check
tar -xzf "$directory/$archive" -C "$directory/linux"
