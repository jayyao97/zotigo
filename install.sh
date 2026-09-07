#!/bin/sh
main() {
set -eu
umask 077
component=cli version= source_dir= service=yes
prefix="${ZOTIGO_INSTALL_PREFIX:-$HOME/.local/share/zotigo}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --component|--version|--source|--prefix)
      [ "$#" -ge 2 ] || { echo "Missing value for $1" >&2; exit 2; }
      case "$1" in --component) component=$2 ;; --version) version=$2 ;; --source) source_dir=$2 ;; --prefix) prefix=$2 ;; esac
      shift 2 ;;
    --no-service) service=no; shift ;;
    --help) echo 'Usage: sh install.sh [--version TAG|COMMIT | --source CHECKOUT] [--component cli|daemon] [--prefix PATH] [--no-service]'; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done
case "$component" in cli|daemon) ;; *) echo 'Component must be cli or daemon.' >&2; exit 2 ;; esac
[ -z "$version" ] || [ -z "$source_dir" ] || { echo 'Choose --version or --source, not both.' >&2; exit 2; }

# Git is needed to resolve release tags and preserve source provenance.
for prerequisite in git curl tar; do
  command -v "$prerequisite" >/dev/null 2>&1 || { echo "Install $prerequisite and retry." >&2; exit 1; }
done
git --version >/dev/null 2>&1 || { echo 'Git is unavailable; on macOS install Command Line Tools first.' >&2; exit 1; }
# A piped installer has no sibling files. Bootstrap the helpers from the exact requested revision.
if [ -z "$source_dir" ]; then
  if [ -z "$version" ]; then
    tags=$(git ls-remote --tags --refs https://github.com/jayyao97/zotigo.git)
    selected=$(printf '%s\n' "$tags" | awk '
      $2 ~ /^refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+$/ {
        split(substr($2,12),v,".")
        if (!found || v[1]+0 > major || (v[1]+0 == major && (v[2]+0 > minor || (v[2]+0 == minor && v[3]+0 > patch)))) {
          found=1; major=v[1]+0; minor=v[2]+0; patch=v[3]+0; chosen=$1 " " $2
        }
      }
      END {if (found) print chosen}')
    [ -n "$selected" ] || { echo 'No stable vX.Y.Z release tag is published yet. Use --source for development.' >&2; exit 1; }
    ref=${selected%% *}
    version=${selected#* }; version=${version#refs/tags/}
    printf 'Selected stable version %s (%s)\n' "$version" "$ref"
  else
    case "$version" in *[!a-zA-Z0-9._-]*|-*) echo 'Invalid --version; use a tag or full commit.' >&2; exit 2 ;; esac
    if printf '%s\n' "$version" | grep -Eq '^[0-9a-fA-F]{40}$'; then ref=$version; else ref="refs/tags/$version"; fi
  fi
  bootstrap=$(mktemp -d)
  trap 'rm -rf "$bootstrap"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  git init -q "$bootstrap"
  git -C "$bootstrap" fetch -q --depth=1 https://github.com/jayyao97/zotigo.git "$ref"
  git -C "$bootstrap" checkout -q --detach FETCH_HEAD
  [ -f "$bootstrap/install.sh" ] || { echo "Release $version predates source installation; select a newer tag." >&2; exit 1; }
  # Execute the versioned installer, not a potentially different bootstrap revision.
  set -- --source "$bootstrap" --component "$component" --prefix "$prefix"
  [ "$service" = yes ] || set -- "$@" --no-service
  sh "$bootstrap/install.sh" "$@"
  exit
fi

. "$source_dir/scripts/install-common.sh"
install_init
# Capture the caller's choice before build toolchains can prepend to PATH.
detected_codex=
if [ "$component" = daemon ]; then
  candidate=$(command -v codex 2>/dev/null || :)
  case "$candidate" in /*) [ ! -f "$candidate" ] || [ ! -x "$candidate" ] || detected_codex=$candidate ;; esac
fi
need git
need tar
toolchain_manifest="$source_dir/scripts/toolchains.tsv"
. "$source_dir/scripts/toolchains.sh"
ensure_go "$source_dir"
source_identity
service_check
binary=zotigo
[ "$component" = cli ] || binary=zotigod
mkdir "$stage/output"
(cd "$source_dir" && go build -trimpath -o "$stage/output/$binary" "./cmd/$binary")
version=$("$stage/output/$binary" --version | sed -n "s/^$binary //p")
validate_version
"$stage/output/$binary" --version
if [ "$component" = daemon ]; then
  need curl
  # Validate with the PATH the generated launcher will actually use (including
  # interpreters needed by npm wrappers), not just the interactive environment.
  if [ -n "$detected_codex" ] && ! "$detected_codex" --version >/dev/null 2>&1; then
    echo 'Codex found on PATH but cannot run in the service environment; configure ZOTIGO_CODEX_BINARY in daemon.env.' >&2
    detected_codex=
  fi
  config="$prefix/config/daemon.env"
  [ -e "$config" ] || printf '%s\n' '# Shell assignments; preserved on upgrade.' 'ZOTIGOD_ADDR=127.0.0.1:8766' '# ZOTIGOD_AUTH_TOKEN_FILE=/absolute/path/to/token' > "$config"
  {
    echo '#!/bin/sh'
    printf 'PATH=%s; export PATH\n' "$(quote "$PATH")"
    if [ -n "$detected_codex" ]; then
      printf '[ -n "${ZOTIGO_CODEX_BINARY:-}" ] || ZOTIGO_CODEX_BINARY=%s\n' "$(quote "$detected_codex")"
      echo 'export ZOTIGO_CODEX_BINARY'
    fi
    printf '. %s\n' "$(quote "$config")"
    printf 'set -- %s --addr "$ZOTIGOD_ADDR"\n' "$(quote "$root/current/zotigod")"
    echo '[ -z "${ZOTIGOD_AUTH_TOKEN_FILE:-}" ] || set -- "$@" --auth-token-file "$ZOTIGOD_AUTH_TOKEN_FILE"'
    echo 'exec "$@"'
  } > "$stage/output/run"
  chmod +x "$stage/output/run"
fi
if [ -e "$prefix/bin/$binary" ] || [ -L "$prefix/bin/$binary" ]; then
  [ "$(readlink "$prefix/bin/$binary" || true)" = "$root/current/$binary" ] || fail "Refusing to overwrite $prefix/bin/$binary."
fi
publish_install
atomic_link "$root/current/$binary" "$prefix/bin/$binary"
if [ "$component" = daemon ]; then
  service_install
  . "$config"
  daemon_check_url
  service_verify
fi
printf 'Installed %s: %s\nAdd to PATH: %s/bin\nVersion record: %s/current/INSTALLATION\n' "$binary" "$version" "$prefix" "$root"

}
main "$@"
