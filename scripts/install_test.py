"""Installer transaction tests; the compiler is stubbed, filesystem operations are real."""
import os
import json
from pathlib import Path
import shutil
import signal
import time
import plistlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import threading
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="zotigo-install-test-")
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.source = self.base / "source"
        self.source.mkdir()
        shutil.copy(REPO / "install.sh", self.source)
        shutil.copy(REPO / "go.mod", self.source)
        shutil.copy(REPO / "update.json", self.source)
        shutil.copytree(REPO / "scripts", self.source / "scripts")
        for args in [["init", "-q"], ["add", "."],
                     ["-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"],
                     ["tag", "v1.0.0"]]:
            subprocess.run(["git", "-C", str(self.source), *args], check=True)
        self.prefix = self.base / "install with spaces"
        self.tools = self.base / "tools"
        self.tools.mkdir()
        compiler = self.tools / "go"
        compiler.write_text('''#!/bin/sh
[ "$1" != version ] || { echo "go version go1.25.3 test/arch"; exit 0; }
[ "${FAIL_BUILD:-0}" = 0 ] || exit 1
while [ "$1" != -o ]; do shift; done
printf '#!/bin/sh\\necho "zotigo 1.0.0"\\n' > "$2"
chmod +x "$2"
''')
        compiler.chmod(0o755)
        self.env = {**os.environ, "PATH": f"{self.tools}:{os.environ['PATH']}",
                    "ZOTIGO_MACOS_SIGNING_IDENTITY": "1" * 40,
                    "ZOTIGO_MACOS_SIGNING_IDENTITY_FILE": str(self.base / "signing-identity")}
        # Compiler output is a shell fixture, never sign with the host's keychain.
        codesign = self.tools / "codesign"
        codesign.write_text('''#!/bin/sh
[ "${FAIL_SIGN:-0}" = 0 ] || exit 1
if [ "$1" = --display ]; then
  echo 'Identifier=com.zotigo.daemon'
  echo 'Authority=Zotigo Test Signing'
fi
''')
        codesign.chmod(0o755)

    def publish_fixed_daemon(self, **env):
        script = '''set -eu
. "$1"
install_init
mkdir "$stage/output"
printf 'new binary' > "$stage/output/zotigod"
version=2.0.0 commit=test dirty=false
publish_macos_daemon
'''
        return subprocess.run(["sh", "-c", script, "test", str(self.source / "scripts/install-common.sh")],
                              env={**self.env, "prefix": str(self.prefix), "component": "daemon", **env},
                              text=True, capture_output=True)

    def test_macos_publication_keeps_fixed_real_path_and_archives(self):
        current = self.prefix / "daemon/current"
        for _ in range(2):
            result = self.publish_fixed_daemon()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(current.is_dir())
            self.assertFalse(current.is_symlink())
            self.assertEqual((current / "zotigod").resolve(), current / "zotigod")
            release = Path((current / "RELEASE").read_text().strip())
            self.assertEqual((release / "zotigod").read_text(), "new binary")
        self.assertTrue((current.parent / "previous").is_symlink())

    def test_macos_publication_migrates_symlink_and_restores_on_failure(self):
        root = self.prefix / "daemon"
        release = root / "releases/old"
        release.mkdir(parents=True)
        (release / "INSTALLATION").write_text("version=1.0.0\n")
        (release / "zotigod").write_text("old binary")
        (root / "current").symlink_to(release)
        real_mv = shutil.which("mv")
        stub = self.tools / "mv"
        stub.write_text(f'''#!/bin/sh
case "$1" in */active) exit 1 ;; esac
exec '{real_mv}' "$@"
''')
        stub.chmod(0o755)
        result = self.publish_fixed_daemon()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((root / "current").resolve(), release)
        self.assertEqual((root / "current/zotigod").read_text(), "old binary")
        stub.unlink()
        result = self.publish_fixed_daemon()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((root / "current").is_symlink())
        self.assertEqual((root / "previous").resolve(), release)

    def test_macos_publication_refuses_unmanaged_directory(self):
        current = self.prefix / "daemon/current"
        current.mkdir(parents=True)
        (current / "user-file").write_text("keep")
        result = self.publish_fixed_daemon()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unrecognized", result.stderr)
        self.assertEqual((current / "user-file").read_text(), "keep")

    def test_macos_signing_requires_and_reuses_persistent_identity(self):
        uname = self.tools / "uname"
        uname.write_text("#!/bin/sh\necho Darwin\n")
        uname.chmod(0o755)
        script = self.source / "scripts/sign-macos-daemon.sh"
        binary = self.base / "zotigod"
        binary.write_text("fixture")
        def sign(**env):
            return subprocess.run(["sh", str(script), str(binary)], env={**self.env, **env},
                                  text=True, capture_output=True)
        self.assertNotEqual(sign(ZOTIGO_MACOS_SIGNING_IDENTITY="").returncode, 0)
        self.assertNotEqual(sign(ZOTIGO_MACOS_SIGNING_IDENTITY="-").returncode, 0)
        self.assertEqual(sign().returncode, 0)
        self.assertEqual(sign(ZOTIGO_MACOS_SIGNING_IDENTITY="").returncode, 0)
        saved = Path(self.env["ZOTIGO_MACOS_SIGNING_IDENTITY_FILE"])
        self.assertEqual(saved.read_text().strip(), "1" * 40)
        self.assertNotEqual(sign(FAIL_SIGN="1", ZOTIGO_MACOS_SIGNING_IDENTITY="2" * 40).returncode, 0)
        self.assertEqual(saved.read_text().strip(), "1" * 40)

    def install(self, component="cli", **env):
        return subprocess.run(["sh", str(self.source / "install.sh"), "--source", str(self.source),
                               "--prefix", str(self.prefix), "--component", component, "--no-service"],
                              env={**self.env, **env}, text=True, capture_output=True)

    def prepare_daemon(self):
        compiler = self.tools / "go"
        compiler.write_text(compiler.read_text().replace(
            'echo "zotigo 1.0.0"',
            'if [ "$1" = --version ]; then echo "zotigod 1.0.0"; else printenv ZOTIGO_CODEX_BINARY; fi'))
        # The temporary install has no service; do not query the host's daemon.
        for name in ("launchctl", "systemctl"):
            tool = self.tools / name
            tool.write_text("#!/bin/sh\nexit 1\n")
            tool.chmod(0o755)

    def test_daemon_captures_custom_codex_and_preserves_overrides(self):
        self.prepare_daemon()
        custom = self.base / "custom tools' directory"
        custom.mkdir()
        codex = custom / "codex"
        codex.write_text('#!/usr/bin/env node\n')
        codex.chmod(0o755)
        node = custom / "node"
        node.write_text('#!/bin/sh\necho codex-cli test\n')
        node.chmod(0o755)
        # Toolchain PATH changes must not replace the captured entry point.
        decoy = self.tools / "codex"
        decoy.write_text('#!/bin/sh\necho decoy-version\n')
        decoy.chmod(0o755)
        toolchains = self.source / "scripts/toolchains.sh"
        toolchains.write_text(toolchains.read_text() + '\nensure_go() { PATH="$CAPTURE_TEST_TOOLS:$PATH"; export PATH; }\n')
        self.env["CAPTURE_TEST_TOOLS"] = str(self.tools)
        result = self.install(component="daemon", PATH=f"{custom}:{self.env['PATH']}")
        self.assertEqual(result.returncode, 0, result.stderr)
        launcher = self.prefix / "daemon/current/run"
        clean_env = {**self.env, "ZOTIGO_CODEX_BINARY": ""}
        self.assertEqual(subprocess.check_output([str(launcher)], env=clean_env, text=True).strip(), str(codex))
        explicit = {**clean_env, "ZOTIGO_CODEX_BINARY": "/explicit/codex"}
        self.assertEqual(subprocess.check_output([str(launcher)], env=explicit, text=True).strip(), "/explicit/codex")
        config = self.prefix / "config/daemon.env"
        content = config.read_text() + '\nexport ZOTIGO_CODEX_BINARY=/configured/codex\n'
        config.write_text(content)
        result = self.install(component="daemon", PATH=f"{custom}:{self.env['PATH']}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_text(), content)
        self.assertEqual(subprocess.check_output([str(launcher)], env=clean_env, text=True).strip(), "/configured/codex")

    def test_daemon_first_install_generates_and_reuses_private_token(self):
        self.prepare_daemon()
        result = self.install(component="daemon")
        self.assertEqual(result.returncode, 0, result.stderr)
        token = self.prefix / "config/daemon.token"
        original = token.read_text()
        self.assertRegex(original, r"^[0-9a-f]{64}\n$")
        self.assertEqual(token.stat().st_mode & 0o777, 0o600)
        config = self.prefix / "config/daemon.env"
        content = config.read_text()
        self.assertIn(str(token), content)
        self.assertNotIn(original.strip(), result.stdout + result.stderr)
        # Exercise the generated launcher, including its quoted token path.
        binary = self.prefix / "daemon/current/zotigod"
        binary.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
        args = subprocess.check_output([str(self.prefix / "daemon/current/run")], env=self.env, text=True).splitlines()
        self.assertEqual(args, ["--addr", "127.0.0.1:8766", "--auth-token-file", str(token)])
        result = self.install(component="daemon")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_text(), content)
        self.assertEqual(token.read_text(), original)

    def test_daemon_existing_config_does_not_enable_auth(self):
        self.prepare_daemon()
        config = self.prefix / "config/daemon.env"
        config.parent.mkdir(parents=True)
        original = "ZOTIGOD_ADDR=127.0.0.1:9876\n# no authentication\n"
        config.write_text(original)
        result = self.install(component="daemon")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_text(), original)
        self.assertFalse((config.parent / "daemon.token").exists())

    def test_daemon_retry_reuses_token_but_missing_upgrade_config_fails(self):
        self.prepare_daemon()
        token = self.prefix / "config/daemon.token"
        token.parent.mkdir(parents=True)
        token.write_text("interrupted-install-token\n")
        result = self.install(component="daemon")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(token.read_text(), "interrupted-install-token\n")
        config = token.parent / "daemon.env"
        config.unlink()
        result = self.install(component="daemon")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Existing daemon configuration is missing", result.stderr)
        self.assertFalse(config.exists())
        self.assertEqual(token.read_text(), "interrupted-install-token\n")

    def test_daemon_without_usable_codex_still_installs(self):
        self.prepare_daemon()
        clean_path = f"{self.tools}:/usr/bin:/bin:/usr/sbin:/sbin"
        for script in (None, "#!/bin/sh\nexit 1\n"):
            if script:
                codex = self.tools / "codex"
                codex.write_text(script)
                codex.chmod(0o755)
            result = self.install(component="daemon", PATH=clean_path)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn("ZOTIGO_CODEX_BINARY", (self.prefix / "daemon/current/run").read_text())
            if script:
                self.assertIn("cannot run", result.stderr)

    def test_piped_installer_selects_numeric_stable_tag(self):
        for args in [["tag", "v1.9.0"], ["-c", "user.name=Test", "-c", "user.email=test@example.com", "tag", "-a", "v1.10.0", "-m", "stable"], ["tag", "v2.0.0-rc.1"]]:
            subprocess.run(["git", "-C", str(self.source), *args], check=True)
        env = {**self.env, "GIT_CONFIG_COUNT": "1",
               "GIT_CONFIG_KEY_0": f"url.{self.source.as_uri()}.insteadOf",
               "GIT_CONFIG_VALUE_0": "https://github.com/jayyao97/zotigo.git"}
        result = subprocess.run(["sh", "-s", "--", "--prefix", str(self.prefix), "--no-service"],
                                input=(REPO / "install.sh").read_text(), env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Selected stable version v1.10.0", result.stdout)
        expected = subprocess.check_output(["git", "-C", str(self.source), "rev-parse", "HEAD"], text=True).strip()
        self.assertIn(f"commit={expected}\n", (self.prefix / "cli/current/INSTALLATION").read_text())

    def test_default_install_refuses_to_fall_back_to_development(self):
        subprocess.run(["git", "-C", str(self.source), "tag", "-d", "v1.0.0"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(self.source), "tag", "v2.0.0-rc.1"], check=True)
        env = {**self.env, "GIT_CONFIG_COUNT": "1",
               "GIT_CONFIG_KEY_0": f"url.{self.source.as_uri()}.insteadOf",
               "GIT_CONFIG_VALUE_0": "https://github.com/jayyao97/zotigo.git"}
        result = subprocess.run(["sh", str(REPO / "install.sh"), "--prefix", str(self.prefix), "--no-service"],
                                env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("No stable", result.stderr)
        self.assertFalse((self.prefix / "cli/current").exists())

    def test_install_upgrade_and_failed_build(self):
        first = self.install()
        self.assertEqual(first.returncode, 0, first.stderr)
        current = self.prefix / "cli/current"
        original = current.resolve()
        config = self.prefix / "config/keep-me"
        config.write_text("user configuration")
        failed = self.install(FAIL_BUILD="1")
        self.assertNotEqual(failed.returncode, 0)
        self.assertEqual(current.resolve(), original)
        second = self.install()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertNotEqual(current.resolve(), original)
        self.assertEqual((self.prefix / "cli/previous").resolve(), original)
        self.assertEqual(config.read_text(), "user configuration")
        result = subprocess.check_output([str(self.prefix / "bin/zotigo"), "--version"], text=True)
        self.assertEqual("zotigo 1.0.0\n", result)

    def test_lock_prevents_concurrent_install(self):
        lock = self.prefix / "cli/install.lock"
        lock.mkdir(parents=True)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Another cli installation", result.stderr)
        self.assertTrue(lock.exists())

    def test_unmanaged_binary_is_not_overwritten(self):
        binary = self.prefix / "bin/zotigo"
        binary.parent.mkdir(parents=True)
        binary.write_text("unmanaged")
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary.read_text(), "unmanaged")
        self.assertFalse((self.prefix / "cli/current").exists())

    def test_running_program_prevents_switch(self):
        self.assertEqual(self.install().returncode, 0)
        current = self.prefix / "cli/current"
        original = current.resolve()
        program = original / "running"
        program.write_text("#!/bin/sh\nsleep 30\n")
        program.chmod(0o755)
        process = subprocess.Popen([str(program)], start_new_session=True)
        try:
            result = self.install()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Stop cli", result.stderr)
            self.assertEqual(current.resolve(), original)
        finally:
            os.killpg(process.pid, signal.SIGTERM)
            process.wait()

    def test_binary_started_through_path_prevents_switch(self):
        self.assertEqual(self.install().returncode, 0)
        current = self.prefix / "cli/current"
        original = current.resolve()
        # A real executable exercises PATH/symlink process detection. Renaming
        # system sleep is unsafe: multicall coreutils dispatches by argv[0],
        # and copied Apple system binaries may need to be signed again.
        subprocess.run(["cc", "-x", "c", "-", "-o", str(current / "zotigo")],
                       input="#include <unistd.h>\nint main(void) { sleep(30); return 0; }\n",
                       text=True, check=True, capture_output=True)
        process = subprocess.Popen(["zotigo"], env={**self.env, "PATH": f"{self.prefix}/bin:{self.env['PATH']}"})
        try:
            time.sleep(0.2)
            self.assertIsNone(process.poll())
            result = self.install()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Stop cli", result.stderr)
            self.assertEqual(current.resolve(), original)
        finally:
            process.terminate()
            process.wait()

    def test_pairing_uses_clean_installation_provenance(self):
        commit = subprocess.check_output(["git", "-C", str(self.source), "rev-parse", "HEAD"], text=True).strip()
        binary = self.tools / "zotigod"
        binary.write_text('#!/bin/sh\necho "zotigod 1.0.0"\n')
        binary.chmod(0o755)
        record = self.tools / "INSTALLATION"
        (self.tools / "RELEASE").write_text("fixture archive\n")
        cases = [
            ("", 1),  # Unmanaged/old installations have no clean-source provenance.
            (f"version=1.0.0\ncommit={commit}\ndirty=true\n", 1),
            ("version=1.0.0\ncommit=another-commit\ndirty=false\n", 1),
            (f"version=0.9.0\ncommit={commit}\ndirty=false\n", 1),
            (f"version=1.0.0\ncommit={commit}\ndirty=false\n", 0),
        ]
        for metadata, expected in cases:
            record.write_text(metadata)
            result = subprocess.run(["sh", "-c", '. "$1"; installed_daemon_matches "$2" "$3"',
                                     "test", str(self.source / "scripts/install-common.sh"), str(binary), str(self.source)], env=self.env)
            self.assertEqual(result.returncode, expected, metadata)
        (self.source / "local-change").write_text("modified source")
        result = subprocess.run(["sh", "-c", '. "$1"; installed_daemon_matches "$2" "$3"',
                                 "test", str(self.source / "scripts/install-common.sh"), str(binary), str(self.source)], env=self.env)
        self.assertEqual(result.returncode, 1)

    def test_macos_pairing_rejects_old_layout_or_invalid_signature(self):
        uname = self.tools / "uname"
        uname.write_text("#!/bin/sh\necho Darwin\n")
        uname.chmod(0o755)
        commit = subprocess.check_output(["git", "-C", str(self.source), "rev-parse", "HEAD"], text=True).strip()
        binary = self.tools / "zotigod"
        binary.write_text('#!/bin/sh\necho "zotigod 1.0.0"\n')
        binary.chmod(0o755)
        (self.tools / "INSTALLATION").write_text(f"version=1.0.0\ncommit={commit}\ndirty=false\n")
        def matches(**env):
            return subprocess.run(["sh", "-c", '. "$1"; installed_daemon_matches "$2" "$3"',
                                   "test", str(self.source / "scripts/install-common.sh"), str(binary), str(self.source)],
                                  env={**self.env, **env}).returncode
        self.assertNotEqual(matches(), 0)
        (self.tools / "RELEASE").write_text("fixture\n")
        self.assertEqual(matches(), 0)
        self.assertNotEqual(matches(FAIL_SIGN="1"), 0)
        self.assertFalse(Path(self.env["ZOTIGO_MACOS_SIGNING_IDENTITY_FILE"]).exists(),
                         "read-only signature verification must not save configuration")

    def test_source_changes_do_not_redefine_release_version(self):
        subprocess.run(["git", "-C", str(self.source), "tag", "v9.0.0"], check=True)
        self.assertEqual(self.install().returncode, 0)
        record = self.prefix / "cli/current/INSTALLATION"
        self.assertIn("version=1.0.0\n", record.read_text())
        self.assertIn("dirty=false\n", record.read_text())
        (self.source / "local-change").write_text("modified source")
        self.assertEqual(self.install().returncode, 0)
        self.assertIn("version=1.0.0\n", record.read_text())
        self.assertIn("dirty=true\n", record.read_text())

    def test_service_templates(self):
        home = self.base / "home"
        home.mkdir()
        for command in ["launchctl", "systemctl"]:
            stub = self.tools / command
            stub.write_text("#!/bin/sh\nexit 0\n")
            stub.chmod(0o755)
        for platform in ["Darwin", "Linux"]:
            subprocess.run(["sh", "-c", '. "$1"; service_install', "test",
                            str(self.source / "scripts/install-common.sh")], check=True, capture_output=True,
                           env={**self.env, "HOME": str(home), "prefix": str(self.prefix),
                                "root": str(self.prefix / "daemon"), "platform": platform,
                                "component": "daemon", "service": "yes"})
        with (home / "Library/LaunchAgents/com.zotigo.daemon.plist").open("rb") as stream:
            plist = plistlib.load(stream)
        self.assertEqual(plist["ProgramArguments"], [str(self.prefix / "daemon/current/run")])
        self.assertEqual(plist["Umask"], 0o77)
        self.assertEqual(plist["StandardOutPath"], "/dev/null")
        self.assertEqual(plist["StandardErrorPath"], "/dev/null")
        unit = (home / ".config/systemd/user/zotigo-daemon.service").read_text()
        self.assertIn(f'ExecStart="{self.prefix}/daemon/current/run"', unit)
        self.assertIn("WorkingDirectory=%h\n", unit)
        self.assertIn("KillMode=control-group", unit)
        self.assertIn("StandardOutput=null", unit)
        self.assertIn("StandardError=null", unit)

    def test_daemon_probe_rejects_unrelated_same_version_process(self):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                pid = "999" if len(requests) == 1 else "77"
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps({"data": {"version": "1.0.0", "process_id": pid}},
                                            separators=(",", ":")).encode())

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            result = subprocess.run(["sh", "-c", '. "$1"; service_running() { return 0; }; service_pid() { echo 77; }; service_verify',
                                     "test", str(self.source / "scripts/install-common.sh")],
                                    env={**self.env, "service": "yes", "component": "daemon", "version": "1.0.0",
                                         "stage": str(self.base), "check_url": f"http://127.0.0.1:{server.server_port}/health"},
                                    capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(len(requests), 2, "The unrelated same-version daemon must not pass verification")
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_startup_probe_uses_configured_host(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200 if self.headers["Host"] == "web.example.internal" else 403)
                self.end_headers()

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            result = subprocess.run(["sh", "-c", '. "$1"; service_running() { return 0; }; service_verify',
                                     "test", str(self.source / "scripts/install-common.sh")],
                                    env={**self.env, "service": "yes", "component": "web", "stage": str(self.base),
                                         "check_url": f"http://127.0.0.1:{server.server_port}/",
                                         "check_host": "web.example.internal"}, capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_daemon_probe_checks_authenticated_api(self):
        headers = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/health":
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b'{"data":{"version":"1.0.0","process_id":"77"}}')
                else:
                    headers.append(self.headers.get("Authorization"))
                    self.send_response(200 if headers[-1] == "Bearer expected-token" else 401)
                    self.end_headers()

            def log_message(self, *_args):
                pass

        token = self.base / "probe.token"
        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            for value, code in [("expected-token", 0), ("wrong-token", 1)]:
                token.write_text(value + "\n")
                result = subprocess.run(["sh", "-c", '. "$1"; service_running() { return 0; }; service_pid() { echo 77; }; service_verify',
                                         "test", str(self.source / "scripts/install-common.sh")],
                                        env={**self.env, "service": "yes", "component": "daemon", "version": "1.0.0",
                                             "stage": str(self.base), "ZOTIGOD_AUTH_TOKEN_FILE": str(token),
                                             "check_url": f"http://127.0.0.1:{server.server_port}/health"},
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode, code, result.stderr)
                self.assertNotIn(value.encode(), result.stdout + result.stderr)
            self.assertEqual(headers, ["Bearer expected-token", "Bearer wrong-token"])
        finally:
            server.shutdown()
            thread.join()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
