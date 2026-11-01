# Shared by the CLI/daemon installer and the UI installer that consumes this revision.
# Callers set prefix and component; no daemon needs to be running to use this file.
fail() { printf '%s\n' "Error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "Install $1 and retry (see docs/installation.md)."; }
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

install_init() {
  platform=$(uname -s)
  case "$platform" in Linux|Darwin) ;; *) fail 'Only Linux and macOS are supported.' ;; esac
  [ "$(id -u)" != 0 ] || fail 'Run as your normal user, not root or sudo.'
  # Service configuration formats interpret these characters. Reject them at the boundary.
  case "$prefix:$HOME" in *[!a-zA-Z0-9_./:\ -]*) fail 'Install prefix and HOME may contain only letters, digits, spaces, _, ., /, and -.' ;; esac
  case "$prefix" in /*) ;; *) fail '--prefix must be absolute.' ;; esac
  mkdir -p "$prefix/$component/releases" "$prefix/bin" "$prefix/config" "$prefix/logs"
  prefix=$(cd "$prefix" && pwd -P)
  root="$prefix/$component"
  mkdir "$root/install.lock" 2>/dev/null || fail "Another $component installation holds $root/install.lock; remove it only if that installer is no longer running."
  stage=$(mktemp -d "$root/releases/build.XXXXXX")
  if [ "${ZOTIGO_PREPARE_UPDATE:-}" = 1 ]; then printf 'prepare\n' > "$root/install.lock/update-owner"; fi
  trap 'install_cleanup' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

install_cleanup() {
  # Restore the active directory if publication was interrupted between renames.
  if [ -e "$stage/old-current" ] || [ -L "$stage/old-current" ]; then
    if [ ! -e "$root/current" ] && [ ! -L "$root/current" ]; then
      mv "$stage/old-current" "$root/current" || return 1
    fi
  fi
  rm -rf "$stage"
  if [ "${ZOTIGO_PREPARE_UPDATE:-}" = 1 ]; then rm -f "$root/install.lock/update-owner"; fi
  rmdir "$root/install.lock"
}

source_identity() {
  source_dir=$(cd "$source_dir" && pwd -P)
  commit=$(git -C "$source_dir" rev-parse HEAD)
  dirty=false
  if [ -n "$(git -C "$source_dir" status --porcelain)" ]; then dirty=true; fi
}

# Provenance is installation metadata, independent of the code-owned release version.
installed_daemon_matches() (
  [ -x "$1" ] || return 1
  record="$(dirname "$1")/INSTALLATION"
  [ -f "$record" ] || return 1
  if [ "$(uname -s)" = Darwin ]; then
    # Older symlink-based installs need migrating even at the same source commit.
    [ ! -L "$(dirname "$1")" ] && [ -f "$(dirname "$1")/RELEASE" ] || return 1
    sh "$2/scripts/sign-macos-daemon.sh" --verify "$1" >/dev/null 2>&1 || return 1
  fi
  source_dir=$2
  source_identity
  [ "$dirty" = false ] || return 1
  grep -Fx "commit=$commit" "$record" >/dev/null || return 1
  grep -Fx 'dirty=false' "$record" >/dev/null || return 1
  version=$(sed -n 's/^version=//p' "$record")
  [ -n "$version" ] && [ "$("$1" --version)" = "zotigod $version" ]
)

validate_version() {
  case "$version" in ''|*[!a-zA-Z0-9.+_-]*|-*) fail 'Invalid release version in source.' ;; esac
}

atomic_link() {
  target=$1 destination=$2
  ln -s "$target" "$destination.new.$$"
  case "$platform" in
    Darwin) mv -fh "$destination.new.$$" "$destination" ;;
    Linux) mv -fT "$destination.new.$$" "$destination" ;;
  esac
}

assert_stopped() {
  # argv covers Node/scripts; executable paths cover binary aliases and PATH launches.
  case "$platform" in
    Darwin)
      need lsof
      executables=$(lsof -n -a -u "$(id -u)" -d txt -Fn) ;;
    Linux)
      [ -d /proc/self ] || fail 'Process checks require /proc.'
      executables=$(for executable in /proc/[0-9]*/exe; do readlink "$executable" 2>/dev/null || :; done) ;;
  esac
  processes=$(ps ax -o command=)
  if printf '%s\n' "$executables" "$processes" | awk -v root="$root/" 'index($0, root) { found=1 } END { exit !found }'; then
    fail "Stop $component before replacing it; the old installation is unchanged."
  fi
  [ "$component" != daemon ] && [ "$component" != web ] && return 0
  case "$platform" in
    Linux)
      if command -v systemctl >/dev/null && systemctl --user is-active --quiet "zotigo-$component.service"; then
        fail "Run: systemctl --user stop zotigo-$component.service; then retry."
      fi ;;
    Darwin)
      if launchctl print "gui/$(id -u)/com.zotigo.$component" >/dev/null 2>&1; then
        fail "Unload the service before upgrading: launchctl bootout gui/$(id -u)/com.zotigo.$component"
      fi ;;
  esac
}

publish_install() {
  if [ "${ZOTIGO_PREPARE_UPDATE:-}" = 1 ]; then
    printf 'version=%s\ncommit=%s\ndirty=%s\n' "$version" "$commit" "$dirty" > "$stage/output/INSTALLATION"
    [ ! -e "$root/prepared" ] || fail 'A prepared update already exists.'
    mv "$stage/output" "$root/prepared"
    exit 0
  fi
  assert_stopped
  if [ "$platform" = Darwin ] && [ "$component" = daemon ]; then
    publish_macos_daemon
    return
  fi
  if [ -e "$root/current" ] && [ ! -L "$root/current" ]; then fail "$root/current is not an installer-owned symlink."; fi
  release="$root/releases/$version-$(date +%s)-$$"
  printf 'version=%s\ncommit=%s\ndirty=%s\n' "$version" "$commit" "$dirty" > "$stage/output/INSTALLATION"
  mv "$stage/output" "$release"
  if [ -L "$root/current" ]; then atomic_link "$(readlink "$root/current")" "$root/previous"; fi
  atomic_link "$release" "$root/current"
}

publish_macos_daemon() {
  previous_release=
  if [ -L "$root/current" ]; then
    previous_release=$(readlink "$root/current")
  elif [ -e "$root/current" ]; then
    [ -d "$root/current" ] && [ -f "$root/current/RELEASE" ] || fail 'Refusing to replace an unrecognized daemon/current directory.'
    previous_release=$(cat "$root/current/RELEASE")
  fi
  if [ -n "$previous_release" ]; then
    case "$previous_release" in "$root/releases/"*) ;; *) fail 'Unrecognized previous daemon release.' ;; esac
    [ -f "$previous_release/INSTALLATION" ] || fail 'Previous daemon release metadata is missing.'
  fi
  release="$root/releases/$version-$(date +%s)-$$"
  printf 'version=%s\ncommit=%s\ndirty=%s\n' "$version" "$commit" "$dirty" > "$stage/output/INSTALLATION"
  mv "$stage/output" "$release"
  # The archive remains immutable; current is a real, fixed-path directory.
  cp -R "$release" "$stage/active"
  printf '%s\n' "$release" > "$stage/active/RELEASE"
  if [ -n "$previous_release" ]; then atomic_link "$previous_release" "$root/previous"; fi
  # Services and processes were checked above. Never overwrite a live Mach-O.
  if [ -e "$root/current" ] || [ -L "$root/current" ]; then mv "$root/current" "$stage/old-current"; fi
  mv "$stage/active" "$root/current"
}

service_check() {
  [ "$service" = yes ] || return 0
  [ "$component" = daemon ] || [ "$component" = web ] || return 0
  case "$platform" in
    Linux) service_file="$HOME/.config/systemd/user/zotigo-$component.service"; owner="# Zotigo installer: $prefix" ;;
    Darwin) service_file="$HOME/Library/LaunchAgents/com.zotigo.$component.plist"; owner="<!-- Zotigo installer: $prefix -->" ;;
  esac
  if [ -e "$service_file" ] || [ -L "$service_file" ]; then
    [ ! -L "$service_file" ] && grep -Fx "$owner" "$service_file" >/dev/null || fail "Refusing to overwrite $service_file."
  fi
  case "$platform" in
    Linux)
      need systemctl
      systemctl --user show-environment >/dev/null 2>&1 || fail 'No systemd user manager. Use --no-service and run in the foreground.' ;;
    Darwin)
      launchctl print "gui/$(id -u)" >/dev/null 2>&1 || fail 'No macOS login session. Use --no-service and run in the foreground.' ;;
  esac
}

service_install() {
  [ "$service" = yes ] || { printf 'Start manually: %s/current/run\n' "$root"; return 0; }
  # The recovery entrypoint survives a failed swap of current, including the
  # fixed-path macOS daemon directory. Upgrade it only while the service is down.
  if [ -x "$root/current/zotigo-update" ]; then
    cp "$root/current/zotigo-update" "$root/updater.new"
    mv "$root/updater.new" "$root/updater"
    {
      echo '#!/bin/sh'
      printf '%s recover --prefix %s --component %s || exit 1\n' "$(quote "$root/updater")" "$(quote "$prefix")" "$(quote "$component")"
      printf 'exec %s\n' "$(quote "$root/current/run")"
    } > "$root/launch.new"
    chmod +x "$root/launch.new"
    mv "$root/launch.new" "$root/launch"
    service_launcher="$root/launch"
  else
    service_launcher="$root/current/run"
  fi
  case "$platform" in
    Linux)
      service_file="$HOME/.config/systemd/user/zotigo-$component.service"
      mkdir -p "$(dirname "$service_file")"
      if [ -e "$service_file" ] && ! grep -F "# Zotigo installer: $prefix" "$service_file" >/dev/null; then fail "Refusing to overwrite $service_file."; fi
      cat > "$service_file" <<EOF
# Zotigo installer: $prefix
[Unit]
Description=Zotigo $component
[Service]
ExecStart="$service_launcher"
WorkingDirectory=%h
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
KillMode=control-group
UMask=0077
StandardOutput=null
StandardError=null
[Install]
WantedBy=default.target
EOF
      systemctl --user daemon-reload
      systemctl --user enable --now "zotigo-$component.service"
      printf 'Manage: systemctl --user {status,start,stop,restart} zotigo-%s.service\nLogs: %s/.zotigo/logs/%s/\nFor boot/SSH logout persistence, configure loginctl enable-linger for your user.\n' "$component" "$HOME" "$component"
      ;;
    Darwin)
      service_file="$HOME/Library/LaunchAgents/com.zotigo.$component.plist"
      mkdir -p "$(dirname "$service_file")"
      if [ -e "$service_file" ] && ! grep -F "<!-- Zotigo installer: $prefix -->" "$service_file" >/dev/null; then fail "Refusing to overwrite $service_file."; fi
      cat > "$service_file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<!-- Zotigo installer: $prefix -->
<key>Label</key><string>com.zotigo.$component</string>
<key>ProgramArguments</key><array><string>$service_launcher</string></array>
<key>WorkingDirectory</key><string>$HOME</string>
<key>Umask</key><integer>63</integer>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>/dev/null</string>
</dict></plist>
EOF
      launchctl bootstrap "gui/$(id -u)" "$service_file"
      printf 'Status: launchctl print gui/%s/com.zotigo.%s\nStop: launchctl bootout gui/%s/com.zotigo.%s\nStart: launchctl bootstrap gui/%s "%s"\nLogs: %s/.zotigo/logs/%s/\n' "$(id -u)" "$component" "$(id -u)" "$component" "$(id -u)" "$service_file" "$HOME" "$component"
      ;;
  esac
}

service_running() {
  case "$platform" in
    Linux) systemctl --user is-active --quiet "zotigo-$component.service" ;;
    Darwin) launchctl print "gui/$(id -u)/com.zotigo.$component" 2>/dev/null | grep -q 'state = running' ;;
  esac
}

service_pid() {
  case "$platform" in
    Linux) systemctl --user show "zotigo-$component.service" --property=MainPID --value ;;
    Darwin) launchctl print "gui/$(id -u)/com.zotigo.$component" | awk '$1 == "pid" && $2 == "=" { print $3; exit }' ;;
  esac
}

daemon_check_url() {
  case "$ZOTIGOD_ADDR" in
    :*) check_url="http://127.0.0.1$ZOTIGOD_ADDR/health" ;;
    0.0.0.0:*) check_url="http://127.0.0.1:${ZOTIGOD_ADDR##*:}/health" ;;
    '[::]:'*) check_url="http://[::1]:${ZOTIGOD_ADDR##*:}/health" ;;
    *) check_url="http://$ZOTIGOD_ADDR/health" ;;
  esac
}

service_verify() {
  [ "$service" = yes ] || return 0
  set -- --noproxy '*' -fsS --max-time 2
  [ -z "${check_host:-}" ] || set -- "$@" -H "Host: $check_host"
  attempts=0
  while [ "$attempts" -lt 15 ]; do
    if service_running && curl "$@" "$check_url" > "$stage/health" 2>/dev/null; then
      [ "$component" != web ] || return 0
      # A different daemon on the same port may have the same release version.
      expected_pid=$(service_pid) || expected_pid=
      case "$expected_pid" in
        ''|0|*[!0-9]*) ;;
        *)
          if grep -F "\"version\":\"$version\"" "$stage/health" >/dev/null &&
             grep -F "\"process_id\":\"$expected_pid\"" "$stage/health" >/dev/null; then
            if [ -n "${ZOTIGOD_AUTH_TOKEN_FILE:-}" ]; then
              # Keep the credential out of process arguments and diagnostics.
              (umask 077; printf 'Authorization: Bearer %s\n' "$(cat "$ZOTIGOD_AUTH_TOKEN_FILE")" > "$stage/daemon-auth-header")
              curl "$@" -H "@$stage/daemon-auth-header" "${check_url%/health}/projects" >/dev/null 2>&1 || fail 'Daemon started but authenticated API verification failed; check its token configuration.'
            fi
            return 0
          fi ;;
      esac
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  fail "$component was installed but did not pass startup verification at $check_url. Inspect the service logs; no data has been rolled back."
}
