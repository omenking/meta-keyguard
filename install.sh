#!/bin/sh
# Installs the latest keyguard release into ~/.local/bin (or $KEYGUARD_INSTALL_DIR):
#   curl -fsSL https://raw.githubusercontent.com/omenking/meta-keyguard/main/install.sh | sh
set -eu

repo=omenking/meta-keyguard
dir=${KEYGUARD_INSTALL_DIR:-$HOME/.local/bin}

case "$(uname -s)" in
  Linux) ;;
  *) echo "keyguard runs inside WSL on Linux; this system is $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "No keyguard release for $(uname -m)" >&2; exit 1 ;;
esac

archive=keyguard_linux_$arch.tar.gz
base=https://github.com/$repo/releases/latest/download
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c --quiet -)
tar -xzf "$tmp/$archive" -C "$tmp" keyguard

mkdir -p "$dir"
install -m 0755 "$tmp/keyguard" "$dir/keyguard"
echo "Installed $("$dir/keyguard" --version) to $dir/keyguard"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc" ;;
esac
echo "Next: run 'keyguard install-bridge', then load the extension it prints in chrome://extensions."
