#!/bin/sh
set -eu

root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
server_pid=""
trap 'test -z "$server_pid" || kill "$server_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT HUP INT TERM

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "unsupported test OS" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) echo "unsupported test architecture" >&2; exit 1 ;;
esac

version="v0.0.0"
archive="agentline_0.0.0_${os}_${arch}.tar.gz"
mkdir -p "$tmp/release/$version" "$tmp/payload" "$tmp/install"
go build -o "$tmp/payload/agentline" "$root/cmd/agentline"
tar -czf "$tmp/release/$version/$archive" -C "$tmp/payload" agentline
if command -v sha256sum >/dev/null 2>&1; then
  checksum="$(sha256sum "$tmp/release/$version/$archive" | awk '{ print $1 }')"
else
  checksum="$(shasum -a 256 "$tmp/release/$version/$archive" | awk '{ print $1 }')"
fi
printf '%s  %s\n' "$checksum" "$archive" > "$tmp/release/$version/checksums.txt"

python3 - "$tmp/release" "$tmp/port" <<'PY' &
import http.server
import os
import socketserver
import sys

os.chdir(sys.argv[1])
with socketserver.TCPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler) as server:
    with open(sys.argv[2], "w") as port_file:
        port_file.write(str(server.server_address[1]))
    server.serve_forever()
PY
server_pid=$!

while [ ! -s "$tmp/port" ]; do sleep 0.05; done
base="http://127.0.0.1:$(cat "$tmp/port")"
AGENTLINE_VERSION="$version" \
AGENTLINE_RELEASE_BASE_URL="$base" \
AGENTLINE_INSTALL_DIR="$tmp/install" \
  sh "$root/website/install.sh"
test -x "$tmp/install/agentline"

printf 'corrupt' >> "$tmp/release/$version/$archive"
if AGENTLINE_VERSION="$version" \
   AGENTLINE_RELEASE_BASE_URL="$base" \
   AGENTLINE_INSTALL_DIR="$tmp/install-bad" \
     sh "$root/website/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a corrupt archive" >&2
  exit 1
fi

echo "installer smoke test passed"
