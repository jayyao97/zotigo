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
  trap 'rm -rf "$stage"; rmdir "$root/install.lock"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
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
  assert_stopped
  if [ -e "$root/current" ] && [ ! -L "$root/current" ]; then fail "$root/current is not an installer-owned symlink."; fi
  release="$root/releases/$version-$(date +%s)-$$"
  printf 'version=%s\ncommit=%s\ndirty=%s\n' "$version" "$commit" "$dirty" > "$stage/output/INSTALLATION"
  mv "$stage/output" "$release"
  if [ -L "$root/current" ]; then atomic_link "$(readlink "$root/current")" "$root/previous"; fi
  atomic_link "$release" "$root/current"
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
ExecStart="$root/current/run"
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
<key>ProgramArguments</key><array><string>$root/current/run</string></array>
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
             grep -F "\"process_id\":\"$expected_pid\"" "$stage/health" >/dev/null; then return 0; fi ;;
      esac
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  fail "$component was installed but did not pass startup verification at $check_url. Inspect the service logs; no data has been rolled back."
}
