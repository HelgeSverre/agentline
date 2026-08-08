#!/bin/sh
set -eu

repo="HelgeSverre/agentline"
api_url="${AGENTLINE_RELEASE_API_URL:-https://api.github.com/repos/$repo/releases/latest}"
release_base="${AGENTLINE_RELEASE_BASE_URL:-https://github.com/$repo/releases/download}"
install_dir="${AGENTLINE_INSTALL_DIR:-$HOME/.local/bin}"

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "agentline: curl or wget is required" >&2
    exit 1
  fi
}

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "agentline: unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) echo "agentline: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

version="${AGENTLINE_VERSION:-}"
if [ -z "$version" ]; then
  metadata="$(mktemp)"
  trap 'rm -rf "$metadata" "${tmp_dir:-}"' EXIT HUP INT TERM
  download "$api_url" "$metadata"
  version="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$metadata" | head -n 1)"
  if [ -z "$version" ]; then
    echo "agentline: could not determine the latest release" >&2
    exit 1
  fi
fi

release_version="${version#v}"
archive="agentline_${release_version}_${os}_${arch}.tar.gz"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${metadata:-}" "$tmp_dir"' EXIT HUP INT TERM

download "$release_base/$version/$archive" "$tmp_dir/$archive"
download "$release_base/$version/checksums.txt" "$tmp_dir/checksums.txt"

expected="$(awk -v file="$archive" '$2 == file { print $1 }' "$tmp_dir/checksums.txt")"
if [ -z "$expected" ]; then
  echo "agentline: $archive is missing from checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp_dir/$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp_dir/$archive" | awk '{ print $1 }')"
else
  echo "agentline: sha256sum or shasum is required" >&2
  exit 1
fi

if [ "$actual" != "$expected" ]; then
  echo "agentline: checksum verification failed for $archive" >&2
  exit 1
fi

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" agentline
mkdir -p "$install_dir"
install -m 0755 "$tmp_dir/agentline" "$install_dir/agentline.tmp.$$"
mv "$install_dir/agentline.tmp.$$" "$install_dir/agentline"

echo "Installed agentline $version to $install_dir/agentline"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to your PATH." ;;
esac
