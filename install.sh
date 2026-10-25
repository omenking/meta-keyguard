#!/bin/sh
# Installs the latest meta-keyguard release into ~/.local/bin (or $KEYGUARD_INSTALL_DIR):
#   curl -fsSL https://raw.githubusercontent.com/omenking/meta-keyguard/main/install.sh | sh
set -eu

repo=omenking/meta-keyguard
dir=${KEYGUARD_INSTALL_DIR:-$HOME/.local/bin}

case "$(uname -s)" in
  Linux) ;;
  *) echo "meta-keyguard runs inside WSL on Linux; this system is $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "No meta-keyguard release for $(uname -m)" >&2; exit 1 ;;
esac

archive=meta-keyguard_linux_$arch.tar.gz
base=https://github.com/$repo/releases/latest/download
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c --quiet -)
tar -xzf "$tmp/$archive" -C "$tmp" meta-keyguard

mkdir -p "$dir"
install -m 0755 "$tmp/meta-keyguard" "$dir/meta-keyguard"
echo "Installed $("$dir/meta-keyguard" --version) to $dir/meta-keyguard"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc" ;;
esac
echo "Next: run 'meta-keyguard install-bridge', then load the extension it prints in chrome://extensions."
