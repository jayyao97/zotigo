#!/bin/sh
# Shared by source installation and the Desktop development daemon builder.
set -eu
verify_only=no
if [ "${1:-}" = --verify ]; then verify_only=yes; shift; fi
[ "$#" = 1 ] || { echo 'Usage: sign-macos-daemon.sh [--verify] BINARY' >&2; exit 2; }
[ "$(uname -s)" = Darwin ] || exit 0
identity_file="${ZOTIGO_MACOS_SIGNING_IDENTITY_FILE:-$HOME/.config/zotigo/macos-signing-identity}"
identity=${ZOTIGO_MACOS_SIGNING_IDENTITY:-}
if [ -z "$identity" ] && [ -f "$identity_file" ]; then identity=$(cat "$identity_file"); fi
# Require an unambiguous certificate fingerprint, never the ad-hoc '-' identity.
if ! printf '%s\n' "$identity" | grep -Eq '^[0-9a-fA-F]{40}$'; then
  echo "macOS daemon signing requires a persistent code-signing certificate. Set ZOTIGO_MACOS_SIGNING_IDENTITY to its 40-character SHA-1 fingerprint, or save that fingerprint in $identity_file. See docs/installation.md." >&2
  exit 1
fi
if [ "$verify_only" = no ]; then
  codesign --force --sign "$identity" --identifier com.zotigo.daemon "$1"
fi
codesign --verify --strict -R "=identifier \"com.zotigo.daemon\" and certificate leaf = H\"$identity\"" "$1"
# Signing must be certificate-backed. An unavailable/locked identity is fatal.
details=$(codesign --display --verbose=2 "$1" 2>&1)
if ! printf '%s\n' "$details" | grep -Fx 'Identifier=com.zotigo.daemon' >/dev/null ||
   ! printf '%s\n' "$details" | grep '^Authority=' >/dev/null; then
  echo 'Daemon signature must use com.zotigo.daemon and a certificate-backed identity.' >&2
  exit 1
fi
if [ "$verify_only" = no ] && [ -n "${ZOTIGO_MACOS_SIGNING_IDENTITY:-}" ]; then
  umask 077
  mkdir -p "$(dirname "$identity_file")"
  saved_identity=$(mktemp "${identity_file}.XXXXXX")
  trap 'rm -f "$saved_identity"' EXIT
  printf '%s\n' "$identity" > "$saved_identity"
  mv -f "$saved_identity" "$identity_file"
fi
