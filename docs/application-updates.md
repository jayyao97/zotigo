**English** | [简体中文](zh-CN/application-updates.md)

# In-app updates

The "Versions and updates" page in settings manages the current client and the Daemon of the connected host separately. Desktop updates run on this computer, and Web updates run on the server that serves the page; Daemon updates always run on the corresponding host. Closing the browser or disconnecting a remote connection does not cancel an update task that has already been submitted to the service manager.

## First installation and releases

Older versions have no update API or standalone updater, so they must first be upgraded once manually with the install script. The Daemon's public API must be configured with a bearer token; the internal worker token has no access to the update API. Service upgrades apply to the systemd user service or macOS LaunchAgent created by the installer. Processes running from a source checkout for development are not managed by the updater.

This version still downloads source code from the official Git repository and builds it with a pinned toolchain, so the target host must be able to reach GitHub and the toolchain download source. The existing service keeps running during the build; it is stopped only during the "apply update" stage.

Publishers must maintain the following together:

- The program version and the `version` in the root `update.json`; they must match.
- `update_protocol`: the updater protocol. Only `1` is currently accepted.
- `api_epoch`: the compatibility generation between clients and the Daemon. In-app updates currently require the target and the installed component to have the same generation.
- `data_epoch`: the bidirectional compatibility generation of the persistent format. Within the same generation, older versions must be able to read data written by newer versions; only then is automatic rollback allowed.
- The UI's `daemon-version`: the matching Daemon revision used for source builds.

dev.34 keeps `data_epoch: 1` and can be upgraded to directly from the existing in-app update entry point in dev.31, with no bridge version, manual takeover of the updater, or SSH required. The base `schema_meta` and `catalog_migrations` stay at 15; completion callbacks and managed repositories are backward-compatible extensions whose progress is recorded separately in `catalog_extensions`. New structures and their progress are committed in the same transaction, so a failed extension does not mark the base schema recognized by older versions as an unreadable version.

Publish the matching Daemon tag first, then the UI tag that requires the matching changes. This release's Daemon is `v0.0.4-dev.34`, and no UI change is required. For development testing, the UI installer's `--daemon-source` can point to a local checkout. A code commit by itself does not become a detectable update; a version tag must be published. Do not reuse or move published tags.

dev.32/33 were published with epoch 2/3. This fix assumes it has been confirmed that they were never actually installed, and no automatic conversion is provided for databases from those two versions. Adding the dev.34 tag is enough for it to become the highest version on the development channel; historical tags do not need to be deleted or rewritten. If actual schema 16/17 installations are discovered later, stop releasing and handle them separately; do not just change their install manifest.

Compatibility includes the write and scheduling behavior of the old program: the callback queue uses `completion_*` states, which dev.31 does not execute as ordinary messages; the ordinary queue keeps using the old states. Managed repositories keep the Git source and workspace path usable by older versions, plans store a pinned commit as the baseline, and extension foreign keys do not prevent older versions from deleting projects. If an upgrade fails, the existing updater can restore just the old program, and the old program can still open the database; extension data is kept and can be used again after a later upgrade.

`data_epoch` is not incremented for every added table or column. A release that keeps the same epoch must pass real binary tests for "upgrade from the old program to the new program" and "roll back to the old program after the new program has written data". Consider changing it only when compatibility with old reads and writes truly cannot be kept, and first design an upgrade path that the old updater can reach.

## Channels and version detection

The updater reads tags from the official repository and compares `vX.Y.Z` and `vX.Y.Z-dev.N` numerically. The stable channel includes only stable tags; the development channel includes both development and stable tags, so a development version can upgrade to the corresponding stable version. For annotated tags the peeled commit is used, and the prepare stage fetches that pinned commit so the tag cannot change between check and download.

The UI and each Daemon store their channel independently. The channel is first inferred from the installed version, and the saved choice is used afterwards. Switching channels only checks versions; when a development version switches back to an older stable version, that target is also shown, and you still need to click prepare and apply. Custom dirty builds show a separate marker, and they install only after the user explicitly selects a published version.

The client checks in the background at startup and every four hours; the settings page rechecks stale results, and checking for updates manually rechecks immediately. Network failures are shown as errors and are not treated as "already up to date". Releases without a compatible manifest, or with a mismatched protocol, API, or data generation, can be detected, but installation is refused at prepare time and manual migration is required.

## Upgrade and recovery

1. `prepare` pins the source commit, validates the manifest, and generates `prepared` in the install directory. The existing service is not stopped.
2. `apply` starts a standalone update task. Linux uses a separate transient systemd unit and macOS uses a separate LaunchAgent, so the task does not exit with the process group of the Daemon being upgraded.
3. The Daemon first stops admitting new input and queue dispatch, and waits for the current turn, tool calls, and tasks waiting for human input to finish. The update can be canceled while it is still in this waiting stage; unexecuted messages in the persistent queue are kept. After the upgrade completes, the user may need to explicitly resume paused queues.
4. The updater acquires the same install lock as the command-line installer, stops the component, records a switch journal, activates the new version, and starts it. The previous version and configuration are kept. The macOS Daemon keeps using its fixed path and existing signing identity.
5. For the Daemon, the health endpoint, version, service PID, and an authenticated endpoint are checked; Desktop waits for a startup receipt after the new window finishes loading; Web checks the HTTP startup status. On failure, the updater tries to stop the new version, restore the previous version, and start it.

Status and logs are stored in `<prefix>/updates/<component>/`, where `status.json` is the UI status and `update.log` is the build and switch log. Sensitive tokens are not written to the command line. Version archives are stored in `<prefix>/<component>/releases/`.

The recovery record `journal` points to an isolated switch directory. The service's fixed `launch` entry point first calls the standalone updater to recover an interrupted switch, then starts `current/run`. Status queries detect leftover tasks with no active updater and clear remaining admission restrictions; if `current` is missing, it is restored from the original directory recorded in the journal. If recovery fails, the logs and directories are kept, and unknown installations are not overwritten.

## Verification

Use an isolated prefix/HOME; do not replace services the developer is currently running:

```sh
go test ./internal/update ./internal/zotigod
python3 scripts/update_test.py
python3 scripts/install_test.py
```

In transaction tests, platform service commands are replaced with isolated stubs, while directory switching and process checks use the real implementation. Real macOS signing, LaunchAgent, Desktop startup, and a full upgrade of the target remote service still require acceptance testing on real machines before release.

Automatic conversion of managed repositories runs in the background after the daemon starts, not as a synchronous scan when the catalog is opened. The original source stays usable until the managed copy and local unpublished refs have been prepared successfully; failures are persisted and retried in later maintenance cycles, and they do not prevent the service from starting. Existing caches are refreshed every ten minutes; before a workspace is created, the daemon waits up to ten seconds for a refresh, and on network failure it can use the last successful cache and indicate the cache age in the source response. Cross-epoch installation and rollback requirements are unchanged.
