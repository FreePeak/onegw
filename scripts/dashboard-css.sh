#!/bin/sh
# Rebuild internal/server/dashboard/static/admin.css from admin.src.css
# with the Tailwind v4 standalone CLI — a single static binary, no Node,
# no npm. Resolves the CLI from $TW_BIN, $PATH, or downloads the pinned
# release for this platform into /tmp. The compiled output is committed;
# regular builds never invoke this script.
set -e
ver=v4.3.3
root=$(cd "$(dirname "$0")/.." && pwd)
dir=$root/internal/server/dashboard

tw=${TW_BIN:-$(command -v tailwindcss || true)}
if [ -z "$tw" ]; then
  case "$(uname -s)/$(uname -m)" in
    Darwin/arm64)  asset=tailwindcss-macos-arm64 ;;
    Darwin/x86_64) asset=tailwindcss-macos-x64 ;;
    Linux/aarch64) asset=tailwindcss-linux-arm64 ;;
    Linux/x86_64)  asset=tailwindcss-linux-x64 ;;
    *) echo "unsupported platform: set TW_BIN to the tailwindcss CLI" >&2; exit 1 ;;
  esac
  tw=/tmp/tailwindcss-$ver
  [ -x "$tw" ] || curl -fsSL -o "$tw" \
    "https://github.com/tailwindlabs/tailwindcss/releases/download/$ver/$asset"
  chmod +x "$tw"
fi

"$tw" --cwd "$dir" -i admin.src.css -o static/admin.css --minify
echo "wrote $dir/static/admin.css ($(wc -c < "$dir/static/admin.css" | tr -d ' ') bytes)"
