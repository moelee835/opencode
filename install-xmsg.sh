#!/usr/bin/env bash
set -euo pipefail

tag=xmsg-v0.1.0
release_url="https://github.com/moelee835/opencode/releases/download/${tag}"
install_dir="${OPENCODE_XMSG_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'OpenCode XMSG supports macOS and Linux.\n' >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) printf 'OpenCode XMSG supports ARM64 and x86-64.\n' >&2; exit 1 ;;
esac

archive="opencode-xmsg-${os}-${arch}.tar.gz"
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "$tmp_dir"' EXIT

curl -fsSL --retry 3 --output "$tmp_dir/$archive" "$release_url/$archive"
curl -fsSL --retry 3 --output "$tmp_dir/checksums.txt" "$release_url/checksums.txt"

(
  cd "$tmp_dir"
  awk -v archive="$archive" '
    $2 == archive && length($1) == 64 && $1 ~ /^[0-9a-fA-F]+$/ {
      print $1 "  " archive
      matches++
    }
    END { if (matches != 1) exit 1 }
  ' checksums.txt > checksum
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c checksum
  else
    shasum -a 256 -c checksum
  fi
)

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" opencode-xmsg
mkdir -p "$install_dir"
install -m 755 "$tmp_dir/opencode-xmsg" "$install_dir/opencode-xmsg"
printf 'Installed OpenCode XMSG %s. Run %q from your project directory.\n' \
  "$tag" "$install_dir/opencode-xmsg"
