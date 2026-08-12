#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
project_dir="$(cd -- "$script_dir/.." && pwd)"
app_version="${APP_VERSION:-1.0.0}"

for command_name in go makensis; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Missing required command: $command_name" >&2
    if [[ "$command_name" == "makensis" ]]; then
      echo "Install it on Ubuntu with: sudo apt-get install nsis" >&2
    fi
    exit 1
  fi
done

cd "$project_dir"
mkdir -p dist

echo "Building Windows amd64 GUI executable on Ubuntu..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w -H windowsgui' \
  -o dist/frpc-web.exe .

echo "Building NSIS installer on Ubuntu..."
makensis -NOCD -DAPP_VERSION="$app_version" packaging/frpc-web.nsi

echo "Created:"
ls -lh \
  dist/frpc-web.exe \
  dist/frpc-web-for-Windows.exe
