#!/bin/sh
# Invoked by the independent updater, never by a renderer or a session tool.
set -eu
prefix=$1 component=$2
root="$prefix/$component"
. "$(dirname "$0")/install-common.sh"
platform=$(uname -s)
service=yes
stage=$(mktemp -d "$prefix/updates/$component/apply.XXXXXX")
switched=no stopped=no
desktop_unit=
mkdir "$root/install.lock" || fail 'Another installation is running.'
printf '%s\n' "$stage" > "$root/install.lock/update-owner"
printf '%s\n' "$stage" > "$prefix/updates/$component/journal.new"
mv "$prefix/updates/$component/journal.new" "$prefix/updates/$component/journal"
check_host=
cleanup() {
  code=$?
  trap - EXIT
  if [ ! -e "$root/current" ] && [ ! -L "$root/current" ] && { [ -e "$stage/old-current" ] || [ -L "$stage/old-current" ]; }; then
    mv "$stage/old-current" "$root/current" || exit 1
  fi
  if [ "$code" != 0 ] && [ "$switched" = yes ]; then
    # Do not replace a running binary if the service cannot be stopped.
    stop_service || exit 1
    if [ -L "$stage/old-current" ]; then
      atomic_link "$(readlink "$stage/old-current")" "$root/current"
    else
      mv "$root/current" "$stage/failed-current"
      mv "$stage/old-current" "$root/current"
    fi
  fi
  if [ "$code" != 0 ] && [ "$stopped" = yes ]; then start_service || :; fi
  if [ "$component" = daemon ]; then rm -f "$prefix/updates/daemon/draining"; fi
  rm -f "$root/install.lock/update-owner" "$prefix/updates/$component/journal"
  rmdir "$root/install.lock"
  # Preserve failed binaries and diagnostics for recovery; no user data lives here.
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
stop_service() {
  if [ "$component" = desktop ]; then
    if [ "$platform" = Linux ] && [ -n "$desktop_unit" ]; then
      systemctl --user stop "$desktop_unit"
    elif [ -f "$prefix/updates/desktop/started.json" ]; then
      desktop_pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$prefix/updates/desktop/started.json")
      [ -z "$desktop_pid" ] || kill "$desktop_pid" 2>/dev/null || :
      sleep 2
    fi
    (assert_stopped)
    return
  fi
  case "$platform" in
    Linux) systemctl --user stop "zotigo-$component.service" ;;
    Darwin) launchctl bootout "gui/$(id -u)/com.zotigo.$component" ;;
  esac
}
start_service() {
  if [ "$component" = desktop ]; then
    case "$platform" in
      Darwin) open "$root/current/Zotigo.app" ;;
      Linux)
        desktop_unit="zotigo-desktop-$(date +%s)-$$"
        systemd-run --user --collect --unit="$desktop_unit" "$root/current/zotigo-desktop" ;;
    esac
  else
    case "$platform" in
      Linux) systemctl --user start "zotigo-$component.service" ;;
      Darwin) launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.zotigo.$component.plist" ;;
    esac
  fi
}
version=$(sed -n 's/^version=//p' "$root/prepared/INSTALLATION")
[ -n "$version" ] || fail 'Prepared installation metadata is missing.'
if [ "$component" = daemon ]; then
  . "$prefix/config/daemon.env"
  daemon_check_url
  [ -n "${ZOTIGOD_AUTH_TOKEN_FILE:-}" ] || fail 'Application updates require daemon authentication.'
  printf 'Authorization: Bearer %s\n' "$(cat "$ZOTIGOD_AUTH_TOKEN_FILE")" > "$stage/auth"
  # The daemon closes admission before testing for active turns, including
  # queued input, Channel submissions and requests waiting for human input.
  while :; do
    [ ! -f "$prefix/updates/$component/cancel" ] || exit 10
    curl --noproxy '*' -fsS --max-time 10 -H "@$stage/auth" -X POST "${check_url%/health}/updates/drain" > "$stage/drain"
    if grep -Eq '"data"[[:space:]]*:[[:space:]]*true' "$stage/drain"; then break; fi
    sleep 2
  done
elif [ "$component" = web ]; then
  . "$prefix/config/web.env"
  check_bind=${ZOTIGO_WEB_HOST:-127.0.0.1}
  case "$check_bind" in 0.0.0.0) check_bind=127.0.0.1 ;; ::) check_bind='[::1]' ;; *:*) check_bind="[$check_bind]" ;; esac
  check_url="http://$check_bind:${ZOTIGO_WEB_PORT:-8080}/"
  origin=${ZOTIGO_WEB_ORIGIN:-http://127.0.0.1:${ZOTIGO_WEB_PORT:-8080}}
  check_host=${origin#*://}
  check_host=${check_host%%/*}
  case "$origin" in http://*) check_host=${check_host%:80} ;; https://*) check_host=${check_host%:443} ;; esac
fi
service_check
printf 'applying\n' > "$prefix/updates/$component/progress"
[ ! -f "$prefix/updates/$component/cancel" ] || exit 10
if [ "$component" = desktop ]; then
  attempts=0
  until (assert_stopped) >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 120 ] || fail 'Quit Desktop to apply its prepared update.'
    sleep 1
  done
else
  stop_service
fi
stopped=yes
assert_stopped
# Keep the old directory intact until verification succeeds. On macOS the
# daemon's fixed current path and signing identity are preserved.
release="$root/releases/$version-update-$(date +%s)-$$"
mv "$root/prepared" "$release"
if [ "$platform" = Darwin ] && [ "$component" = daemon ]; then
  cp -R "$release" "$stage/new-current"
  printf '%s\n' "$release" > "$stage/new-current/RELEASE"
else
  ln -s "$release" "$stage/new-current"
fi
if [ -L "$root/current" ] && [ -L "$stage/new-current" ]; then
  ln -s "$(readlink "$root/current")" "$stage/old-current"
  atomic_link "$(readlink "$stage/new-current")" "$root/current"
else
  mv "$root/current" "$stage/old-current"
  if ! mv "$stage/new-current" "$root/current"; then
    mv "$stage/old-current" "$root/current"
    fail 'Could not activate the prepared release.'
  fi
fi
switched=yes
if [ "$component" = desktop ]; then rm -f "$prefix/updates/desktop/started.json"; fi
start_service
printf 'verifying\n' > "$prefix/updates/$component/progress"
if [ "$component" != desktop ]; then
  service_verify
else
  attempts=0
  while :; do
    receipt="$prefix/updates/desktop/started.json"
    if [ -f "$receipt" ] && grep -F "\"version\":\"$version\"" "$receipt" >/dev/null; then
      desktop_pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$receipt")
      if [ -n "$desktop_pid" ] && kill -0 "$desktop_pid" 2>/dev/null; then break; fi
    fi
    attempts=$((attempts + 1))
    [ "$attempts" -lt 30 ] || fail 'Desktop did not acknowledge successful startup.'
    sleep 1
  done
fi
stopped=no
if [ -L "$stage/old-current" ]; then
  atomic_link "$(readlink "$stage/old-current")" "$root/previous"
elif [ -f "$stage/old-current/RELEASE" ]; then
  atomic_link "$(cat "$stage/old-current/RELEASE")" "$root/previous"
fi
printf 'Update activated: %s %s\n' "$component" "$version"
