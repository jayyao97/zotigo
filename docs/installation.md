# Source installation (Linux and macOS)

## Codex discovery in background services

When installing the daemon, the installer captures an absolute `codex` executable
from the caller's PATH before selecting build toolchains. It validates `--version`
with the launcher's PATH and records a usable result as a default in that release's
`run` script, not in the user's `daemon.env`. The launcher loads `daemon.env` after
this default, so explicit configuration wins. A new installation captures again;
if a captured path is later removed, reinstall from a terminal where Codex works
or set an explicit path. Existing configuration files are never rewritten.

For `curl ... | sh` in a logged-in terminal, the installer inherits that terminal's
PATH; it does not load shell profiles. A non-interactive SSH command may not have
the same PATH. Missing or unusable Codex does not prevent installation and leaves
runtime discovery in effect. The installer retains its toolchain-adjusted PATH in
the launcher, including interpreter lookup for script-based Codex entry points.
It does not make external Node/version-manager directories immutable.

The daemon does not load interactive shell startup files. Codex discovery checks
`ZOTIGO_CODEX_BINARY` first (an absolute executable path), then `codex` on the
daemon's `PATH`, then `~/.local/bin/codex`. On macOS it also checks the existing
user and system `Applications/ChatGPT.app/Contents/Resources/codex` locations.
The user-bin entry may be a symlink to a standalone Codex release. Discovery does
not modify `PATH`, install Codex, or choose among version-manager installations.

For a custom location, add `export ZOTIGO_CODEX_BINARY=/absolute/path/to/codex`
to `~/.local/share/zotigo/config/daemon.env` (or `PREFIX/config/daemon.env` for a
custom install prefix). Explicit invalid paths fail rather than selecting another
installation. This file is preserved during upgrades. Finish active tasks before
restarting the daemon to reload its environment; restarting Desktop or Web alone
does not reload daemon configuration. A script-based installation must also have
its interpreter, such as Node, available in the daemon's environment.

## Installation

No Homebrew tap, separate manager, GitHub Release, or Apple Developer account is required for this source-install path. Run as your normal user. Git, curl, tar, and a SHA-256 utility (`sha256sum` or `shasum`) are prerequisites. On macOS, install Command Line Tools if Git is unavailable; daemon builds also require a persistent code-signing identity as described below. Daemon services require a systemd user manager on Linux or a logged-in macOS GUI session.

After the installer is merged and an initial stable tag is published:

```sh
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh | sh
```

The installer selects the numerically highest `vX.Y.Z` tag, excludes prerelease tags, and fetches the selected Git object before executing that revision's installer. It fails clearly if no stable tag exists; it never falls back to the development branch. Tags must be immutable. To select an older tag or full commit:

```sh
curl -fsSL https://raw.githubusercontent.com/jayyao97/zotigo/master/install.sh |
  sh -s -- --version v0.0.1
```

The default component is the standalone `zotigo` CLI. UI installers use `--component daemon` to install `zotigod` instead. For a local checkout:

```sh
sh install.sh --source "$PWD" --component daemon
```

The release version is the `Version` constant in `internal/buildinfo/buildinfo.go`, shared by CLI and daemon. Ordinary `go build`, `make build`, and the installer use this same value without linker injection. To release, update the constant, commit it, then publish the matching tag (for example, `0.0.1` → `v0.0.1`). The installer records that version plus the source commit and dirty state separately, builds before switching, and preserves configuration and session data. Go module dependencies may be downloaded during the build. The compiler is selected before building, with automatic Go toolchain downloads disabled for the installer. Do not move published tags.

## Build tools

A working local Go at least as new as the `go` directive in `go.mod` is reused. Otherwise, the installer downloads that exact Go version from the official distribution, verifies the SHA-256 pinned in `scripts/toolchains.tsv`, and installs it under `PREFIX/toolchains/go-VERSION/`. Private downloads support Linux/macOS on AMD64 and ARM64. If a newer source revision requires a tool version absent from the manifest, installation fails with an instruction to update the pin.

Downloads are extracted only after verification and published only after the executable reports the expected version. Existing version directories are reused without being overwritten; a per-tool lock prevents concurrent publication. Failed downloads do not replace existing tools or installed programs. A stale lock or damaged cached tool requires manual inspection rather than automatic removal.

No `sudo`, global package-manager installation, shell profile edits, or persistent environment changes are performed. PATH changes apply only to this installer and its generated launchers. Toolchains are retained for reuse; remove old versions manually only after checking that no installed service or launcher uses them. To update supported downloads, pin the new official URL/version/checksum in `scripts/toolchains.tsv`; verify pnpm's archive against the npm registry integrity value before recording its SHA-256.

## Files and versions

Default prefix: `~/.local/share/zotigo`; override with `--prefix /absolute/path` or `ZOTIGO_INSTALL_PREFIX`. Paths may contain spaces but not shell/service metacharacters. Add `PREFIX/bin` to your shell's PATH.

- `cli/releases/`, `daemon/releases/`: separate builds, never overwritten in place.
- Each component's `current` symlink selects its installation, except the macOS daemon: its `current` is a real directory so the executable's resolved path stays fixed. `previous` retains the prior release archive.
- `current/INSTALLATION`: code-owned release version, source commit, and `dirty=true|false`.
- `config/daemon.env`: user-owned shell assignments, preserved on upgrade.
- On first configuration (no `daemon.env` and no `daemon/current/INSTALLATION`), the installer creates `config/daemon.token` with 32 random bytes encoded as hex and mode `0600`, and sets `ZOTIGOD_AUTH_TOKEN_FILE` in `daemon.env`. OpenSSL is required for this generation. Authentication is enabled even on loopback. An existing token is reused after an interrupted install; upgrades never rotate it or enable authentication on an existing configuration. If an installed daemon has lost `daemon.env`, restore it before reinstalling.
- Diagnostic logs live outside the prefix, in `~/.zotigo/logs/cli/` and `~/.zotigo/logs/daemon/`.

Run `zotigo --version` or `zotigod --version`. `GET /health` also reports the running daemon's `version` and `commit`, in addition to the existing status/protocol fields. The commit comes from optional Go VCS metadata and is `unknown` when unavailable; it is not injected or used as the release version. Installed and running versions can differ until a service is restarted.

## Services

The daemon runs as the installing user, in that user's home directory, on loopback port 8766 by default. The installer captures PATH for Git and external agent tools. Edit `config/daemon.env` for `ZOTIGOD_ADDR` and optional `ZOTIGOD_AUTH_TOKEN_FILE`; it is a trusted shell file, not downloaded configuration. See [the API authentication documentation](zotigod-api.md) before configuring remote access.

Linux:

```sh
systemctl --user status zotigo-daemon.service
systemctl --user stop zotigo-daemon.service
systemctl --user start zotigo-daemon.service
journalctl --user -u zotigo-daemon.service
```

Use `loginctl enable-linger "$USER"` when the user manager must start at boot and survive logout; this may require administrator help. Non-systemd hosts can use `--no-service` and run `PREFIX/daemon/current/run` in the foreground under an existing supervisor.

macOS:

```sh
launchctl print gui/$(id -u)/com.zotigo.daemon
launchctl bootout gui/$(id -u)/com.zotigo.daemon
launchctl bootstrap gui/$(id -u) "$HOME/Library/LaunchAgents/com.zotigo.daemon.plist"
```

LaunchAgents run within the user's login session, not before login. Closing Desktop or a terminal does not stop this service. `--no-service` skips registration/start on either platform.

## Local diagnostic logs

CLI and daemon startup, shutdown and operational errors are saved automatically in `~/.zotigo/logs/cli/` and `~/.zotigo/logs/daemon/`. Each component retains up to 100 MiB total across restarts and concurrent processes. A single file rotates at 5 MiB; a separate 128-file ceiling bounds small-file accumulation. Small logs survive restarts until either total bytes or file count requires pruning. Processes write distinct files, so workers never append to or rotate another worker's file. Cleanup tolerates files already removed by another process. These are retention limits checked after writes, not an instantaneous disk quota during concurrent writes. Old files are pruned automatically; a quiet process may lose its older logs when another process is busy. Individual entries are truncated at 64 KiB. Files are created with owner-only permissions. Logging failures do not stop application work.

Use `ls -lt ~/.zotigo/logs/daemon/` to find recent `run-*.log` files, then `tail -n 200 /path/to/run-file.log`. CLI logs use the same layout. Set `ZOTIGO_DEBUG=1` before launching for existing detailed agent diagnostics; for an installed daemon, add `export ZOTIGO_DEBUG=1` to `config/daemon.env` and restart it. Review logs before sharing: error messages and external tool output can contain private information.

These are operational diagnostics, not a full terminal recording. Session/project history remains separate and is never removed by log retention. Foreground daemon stderr is still available. CLI and daemon launch a small child monitor to save Go runtime crash traces through the same bounded writer, including panics outside the main goroutine. Installed services discard duplicate stdout/stderr streams; use the files above for application diagnostics and `systemctl`/`launchctl` for service-manager failures. Reinstalling updates old service definitions; any legacy `PREFIX/logs/daemon.log` remains on disk and can be removed manually after stopping the old service.

## Upgrade and recovery

Run the same installer for the desired immutable tag. It builds the new version first, but refuses activation while a process from the existing installation is running. Finish tasks and stop/unload the service before retrying. Do not independently restart it while an installer is running. The installer never silently interrupts active tasks or takes over a Homebrew/manual installation.

A failed build or signing step leaves `current` unchanged. Previous build directories remain available. If startup fails after activation, inspect logs and the `previous` link; there is no automatic data rollback. Downgrade only when the old program can read the current data format. Configuration and `~/.zotigo` project/session data are never deleted by installation. Old release directories can be removed manually once no process or `current`/`previous` link uses them. For the macOS daemon, `current/RELEASE` identifies the active archive; keep that archive too.

## macOS daemon identity and privacy permissions

The daemon uses signing identifier `com.zotigo.daemon`. Unlike Go's ad-hoc
signature, a persistent certificate lets successive builds satisfy the same
code identity requirement. The installer and Desktop development builder both
use `scripts/sign-macos-daemon.sh`. They fail if the identity is missing,
unavailable, or cannot sign; they never fall back to ad-hoc signing.

For personal source builds, create a persistent **Code Signing** certificate in
your login keychain using Keychain Access's Certificate Assistant. A local
self-signed identity is sufficient to test code identity continuity; it is not
Developer ID distribution signing or notarization. Keep its private key across
upgrades. Do not generate a new certificate for each build. If it is not listed
as a valid identity, inspect its code-signing trust settings in Keychain Access.
For distribution, select your existing Developer ID Application identity instead.

List available identities, then select the certificate's 40-character SHA-1
fingerprint (not its display name):

```sh
security find-identity -v -p codesigning
ZOTIGO_MACOS_SIGNING_IDENTITY=YOUR_40_CHARACTER_CERTIFICATE_FINGERPRINT \
  sh install.sh --source "$PWD" --component daemon
```

After successful signing the fingerprint is saved to
`~/.config/zotigo/macos-signing-identity` with owner-only permissions. Subsequent
installs and Desktop development builds reuse it without an environment override.
`ZOTIGO_MACOS_SIGNING_IDENTITY_FILE` selects an alternate file. The file contains
only the public certificate fingerprint; private keys stay in the keychain.
The keychain may require unlocking or interactive authorization. Headless builds
must arrange keychain access explicitly. Changing identities can require renewed
privacy authorization. Certificate creation/trust setup is not automated.

The macOS daemon runs at `PREFIX/daemon/current/zotigod`, with no version symlink
in that path. Stop the service before upgrading as usual. The installer archives
the build in `releases/`, stages a complete active directory, then renames it into
place. It restores the old directory if the activation rename fails. This is a
stopped-service replacement with a brief missing-directory window, not a live
atomic directory exchange. After a forced kill/power loss, inspect any retained
`releases/build.*/old-current` and the install lock before recovery; do not delete
that staging directory while `current` is absent. Normal signals run cleanup.
Existing symlink-based installations migrate on the next successful install.
For rollback, reinstall a compatible source revision that includes this fixed-path
installer layout. Older installers cannot publish into the new real `current`
directory. Do not replace `current` with a release symlink or launch the archived binary.

Migration from ad-hoc signing may require one new macOS permission grant. Old
permission entries are not removed automatically. Validate on a real Mac: grant
the needed permission to build A, upgrade to a different build B with the same
identity, confirm file access and no additional entry/prompt, then validate a
rollback. Compare `codesign -d -r- PATH` between builds as supporting evidence.
Different prefixes and Desktop's separate development daemon path are distinct
installations and may still need separate authorization. No TCC database or
system-wide privacy setting is modified by the installer.

To uninstall, stop and disable/unload the service, remove its service file and installer-created binary link, then remove the component's installation directory. Keep `~/.zotigo` and `config/` unless you explicitly want to delete user data.
