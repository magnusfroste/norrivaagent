#!/bin/sh
# Installs the norriva command.
#
#   curl -fsSL https://norriva.example/install.sh | sh
#
# One binary, no runtime. It goes to /usr/local/bin when that is writable,
# otherwise to ~/.local/bin, and the script says which and whether that is on
# your PATH - the two things an install script most often leaves you to find
# out for yourself.
set -eu

REPO="${NORRIVA_REPO:-magnusfroste/norrivaagent}"
VERSION="${NORRIVA_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "norriva: no build for $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "norriva: no build for $os" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  # github.com/.../releases/latest/download/<asset> redirects to the newest
  # release without touching the API, so it never hits the 60-an-hour limit an
  # office full of laptops would. The API is only the fallback, for the rare
  # moment right after a release when the redirect has not caught up.
  url="https://github.com/$REPO/releases/latest/download/norriva-$os-$arch"
  if ! curl -fsSLI -o /dev/null "$url" 2>/dev/null; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" 2>/dev/null \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$VERSION" ] || { echo "norriva: could not find a release of $REPO (is github.com reachable?)" >&2; exit 1; }
    url="https://github.com/$REPO/releases/download/$VERSION/norriva-$os-$arch"
  fi
else
  url="https://github.com/$REPO/releases/download/$VERSION/norriva-$os-$arch"
fi
# A local build server can stand in for GitHub during development.
[ -n "${NORRIVA_DOWNLOAD_BASE:-}" ] && url="$NORRIVA_DOWNLOAD_BASE/norriva-$os-$arch"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
echo "Downloading norriva for $os/$arch..."
if ! curl -fsSL "$url" -o "$tmp"; then
  echo "norriva: download failed: $url" >&2
  exit 1
fi
chmod +x "$tmp"

if [ -w /usr/local/bin ]; then
  dest=/usr/local/bin/norriva
else
  mkdir -p "$HOME/.local/bin"
  dest="$HOME/.local/bin/norriva"
fi
mv "$tmp" "$dest"
trap - EXIT

echo "Installed $("$dest" version) -> $dest"
case ":$PATH:" in
  *":$(dirname "$dest"):"*) ;;
  *)
    echo ""
    echo "$(dirname "$dest") is not on your PATH. Add it:"
    echo "  echo 'export PATH=\"$(dirname "$dest"):\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
    ;;
esac
echo ""
echo "Next:"
echo "  norriva login"
echo "  norriva link ./some-folder"
echo "  norriva \"what is in this folder, and what is in Norriva?\""
