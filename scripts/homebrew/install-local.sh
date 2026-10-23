#!/bin/bash
# Build a standalone binary/UI archive and install it into a local Homebrew tap.
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$project_root"
mode="${1:-all}"
case "$mode" in all|--package-only|--install-only) ;; *) echo "Usage: $0 [--package-only|--install-only]" >&2; exit 2 ;; esac
[ "$(uname -s)" = Darwin ] || { echo 'This local package targets macOS.' >&2; exit 1; }
output_dir="$project_root/tmp/homebrew"
formula="$output_dir/bifrost-local.rb"

if [ "$mode" != --install-only ]; then
  version="0.0.0.$(date -u +%Y%m%d%H%M%S)"
  package_dir="$output_dir/$version"
  mkdir -p "$package_dir"
  make build-ui
  revision="$(git rev-parse --short HEAD)"
  go build -trimpath -ldflags "-s -w -X main.Version=local-$revision-$version" -o "$package_dir/bifrost-http" ./transports/bifrost-http
  cp scripts/homebrew/bifrost-local.sh "$package_dir/bifrost-local"
  chmod 755 "$package_dir/bifrost-http" "$package_dir/bifrost-local"
  archive="$output_dir/bifrost-local-$version-$(uname -m).tar.gz"
  COPYFILE_DISABLE=1 tar -czf "$archive" -C "$package_dir" bifrost-http bifrost-local
  digest="$(shasum -a 256 "$archive" | awk '{print $1}')"
  # Only the local artifact URL is generated; nothing is uploaded or pushed.
  python3 - "$archive" "$version" "$digest" "$formula" <<'PY'
import pathlib, sys
archive, version, digest, target = sys.argv[1:]
source = pathlib.Path('scripts/homebrew/bifrost-local.rb.in').read_text()
source = source.replace('@ARCHIVE_URL@', pathlib.Path(archive).as_uri()).replace('@VERSION@', version).replace('@SHA256@', digest)
pathlib.Path(target).write_text(source)
PY
  echo "Local formula ready: $formula"
fi
[ "$mode" != --package-only ] || exit 0
[ -f "$formula" ] || { echo 'No package found. Run without --install-only first.' >&2; exit 1; }

tap_name=maple/bifrost-local
tap_dir="$(brew --repository)/Library/Taps/maple/homebrew-bifrost-local"
if [ ! -d "$tap_dir" ]; then
  HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$tap_name"
fi
mkdir -p "$tap_dir/Formula"
cp "$formula" "$tap_dir/Formula/bifrost-local.rb"
if brew list --versions "$tap_name/bifrost-local" >/dev/null 2>&1; then
  HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 brew reinstall "$tap_name/bifrost-local"
else
  HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 brew install "$tap_name/bifrost-local"
fi
printf '\nInstalled. Start with: brew services start bifrost-local\n'
