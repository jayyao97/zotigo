# Sourced after install_init. All PATH changes are local to the installer and
# its children; published tool directories are immutable while services use them.
version_at_least() {
  awk -v actual="$1" -v required="$2" 'BEGIN {
    if (actual !~ /^[0-9]+\.[0-9]+\.[0-9]+$/ || required !~ /^[0-9]+\.[0-9]+\.[0-9]+$/) exit 1
    split(actual,a,"."); split(required,r,".")
    for (i=1;i<=3;i++) { if (a[i]+0 > r[i]+0) exit 0; if (a[i]+0 < r[i]+0) exit 1 }
  }'
}

tool_digest() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else fail 'SHA-256 verification requires sha256sum or shasum.'; fi
}

install_tool() (
  tool=$1 tool_version=$2
  system=$(printf '%s' "$platform" | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail 'Private toolchains support AMD64 and ARM64 only.' ;; esac
  row=$(awk -v tool="$tool" -v version="$tool_version" -v os="$system" -v arch="$arch" \
    '$1==tool && $2==version && (($3==os && $4==arch) || ($3=="any" && $4=="any")) {print $5, $6}' "$toolchain_manifest")
  [ -n "$row" ] || fail "No pinned $tool $tool_version for $system/$arch; update scripts/toolchains.tsv."
  checksum=${row%% *} url=${row#* }
  destination="$prefix/toolchains/$tool-$tool_version"
  [ ! -e "$destination" ] || exit 0
  mkdir -p "$prefix/toolchains"
  lock="$destination.lock"
  mkdir "$lock" 2>/dev/null || fail "Another installer holds $lock; retry after it finishes. Remove a stale lock only after checking its owner has stopped."
  temporary=
  trap '[ -z "$temporary" ] || rm -rf "$temporary"; rmdir "$lock"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  [ ! -e "$destination" ] || exit 0
  temporary=$(mktemp -d "$prefix/toolchains/.download.XXXXXX")
  printf 'Downloading private %s %s (%s/%s)...\n' "$tool" "$tool_version" "$system" "$arch"
  curl --proto '=https' --tlsv1.2 -fL --retry 2 "$url" -o "$temporary/archive.tar.gz"
  [ "$(tool_digest "$temporary/archive.tar.gz")" = "$checksum" ] || fail "Checksum mismatch for $tool $tool_version; nothing installed."
  mkdir "$temporary/unpacked"
  tar -xzf "$temporary/archive.tar.gz" --strip-components=1 -C "$temporary/unpacked"
  case "$tool" in
    go) actual=$(GOROOT="$temporary/unpacked" GOTOOLCHAIN=local "$temporary/unpacked/bin/go" version | awk '{sub(/^go/,"",$3); print $3}') ;;
    node) actual=$("$temporary/unpacked/bin/node" -p 'process.versions.node') ;;
    pnpm)
      actual=$("$node_executable" "$temporary/unpacked/bin/pnpm.cjs" --version)
      ln -s pnpm.cjs "$temporary/unpacked/bin/pnpm" ;;
  esac
  [ "$actual" = "$tool_version" ] || fail "Downloaded $tool cannot run or reports unexpected version: $actual"
  mv "$temporary/unpacked" "$destination"
)

ensure_go() {
  required_go=$(awk '$1=="go" {print $2; exit}' "$1/go.mod")
  version_at_least "$required_go" "$required_go" || fail 'Invalid Go version in go.mod.'
  actual_go=$(GOTOOLCHAIN=local go version 2>/dev/null | awk '{sub(/^go/,"",$3); print $3}')
  if ! version_at_least "$actual_go" "$required_go"; then
    install_tool go "$required_go"
    PATH="$prefix/toolchains/go-$required_go/bin:$PATH"
    unset GOROOT
  fi
  # Keep Go from silently downloading a different compiler outside our tool directory.
  export PATH GOTOOLCHAIN=local
  actual_go=$(go version | awk '{sub(/^go/,"",$3); print $3}')
  version_at_least "$actual_go" "$required_go" || fail 'Private Go is unusable; inspect the toolchain directory.'
  printf 'Using Go %s: %s\n' "$actual_go" "$(command -v go)"
}

ensure_node() {
  actual_node=$(node -p 'process.versions.node' 2>/dev/null || :)
  if ! version_at_least "$actual_node" 22.12.0; then
    pinned_node=$(awk '$1=="node" {print $2; exit}' "$toolchain_manifest")
    install_tool node "$pinned_node"
    PATH="$prefix/toolchains/node-$pinned_node/bin:$PATH"
    export PATH
  fi
  actual_node=$(node -p 'process.versions.node')
  version_at_least "$actual_node" 22.12.0 || fail 'Private Node is unusable; inspect the toolchain directory.'
  node_executable=$(node -p 'process.execPath')
  # Also make child build commands use this exact Node when PATH contained a shim.
  PATH="$(dirname "$node_executable"):$PATH"
  export PATH
  printf 'Using Node %s: %s\n' "$actual_node" "$node_executable"
}

ensure_pnpm() {
  expected_pnpm=$("$node_executable" -p "require(process.argv[1]).packageManager.split('@')[1]" "$1/package.json")
  actual_pnpm=$(COREPACK_ENABLE_NETWORK=0 pnpm --version 2>/dev/null || :)
  if [ "$actual_pnpm" != "$expected_pnpm" ]; then
    install_tool pnpm "$expected_pnpm"
    PATH="$prefix/toolchains/pnpm-$expected_pnpm/bin:$PATH"
    export PATH
  fi
  [ "$(pnpm --version)" = "$expected_pnpm" ] || fail 'Private pnpm is unusable; inspect the toolchain directory.'
  printf 'Using pnpm %s: %s\n' "$expected_pnpm" "$(command -v pnpm)"
}
