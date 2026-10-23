#!/bin/bash
# Installed by the local Homebrew formula; no development tools are needed at runtime.
set -euo pipefail

case "${1:-}" in
  -h|--help|-help) exec "@BIFROST_BINARY@" "$@" ;;
esac

app_dir="${BIFROST_APP_DIR:-$HOME/Library/Application Support/BifrostLocal}"
umask 077
mkdir -p "$app_dir"
chmod 700 "$app_dir"

# Link an already-written private file into place without overwriting an existing
# profile, even if two launch attempts race during initial setup.
if [ ! -f "$app_dir/setup-token" ]; then
  token_tmp="$(mktemp "$app_dir/.setup-token.XXXXXX")"
  /usr/bin/openssl rand -hex 32 > "$token_tmp"
  chmod 600 "$token_tmp"
  ln "$token_tmp" "$app_dir/setup-token" 2>/dev/null || test -f "$app_dir/setup-token"
  rm -f "$token_tmp"
fi
export BIFROST_SETUP_TOKEN="${BIFROST_SETUP_TOKEN:-$(cat "$app_dir/setup-token")}"

if [ "${1:-}" = "setup-token" ]; then
  printf '%s\n' "$BIFROST_SETUP_TOKEN"
  exit 0
fi

if [ ! -e "$app_dir/config.json" ]; then
  config_tmp="$(mktemp "$app_dir/.config.XXXXXX")"
  cat > "$config_tmp" <<'JSON'
{
  "setup_token": "env.BIFROST_SETUP_TOKEN",
  "client": {
    "enable_logging": true,
    "enforce_auth_on_inference": true
  },
  "providers": {}
}
JSON
  chmod 600 "$config_tmp"
  ln "$config_tmp" "$app_dir/config.json" 2>/dev/null || test -f "$app_dir/config.json"
  rm -f "$config_tmp"
fi

cd "$app_dir"
exec "@BIFROST_BINARY@" -host "${BIFROST_HOST:-127.0.0.1}" -port "${BIFROST_PORT:-8080}" -app-dir "$app_dir" "$@"
