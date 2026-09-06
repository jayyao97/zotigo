"""Installer transaction tests; the compiler is stubbed, filesystem operations are real."""
import os
import json
from pathlib import Path
import shutil
import signal
import sys
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
        self.env = {**os.environ, "PATH": f"{self.tools}:{os.environ['PATH']}"}

    def install(self, **env):
        return subprocess.run(["sh", str(self.source / "install.sh"), "--source", str(self.source),
                               "--prefix", str(self.prefix), "--no-service"],
                              env={**self.env, **env}, text=True, capture_output=True)

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
        shutil.copy("/bin/sleep", current / "zotigo")
        if sys.platform == "darwin":
            # Copied Apple system binaries need a fresh signature to run as a fixture.
            subprocess.run(["codesign", "--force", "--sign", "-", str(current / "zotigo")],
                           check=True, capture_output=True)
        process = subprocess.Popen(["zotigo", "30"], env={**self.env, "PATH": f"{self.prefix}/bin:{self.env['PATH']}"})
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
                                     "test", str(self.source / "scripts/install-common.sh"), str(binary), str(self.source)])
            self.assertEqual(result.returncode, expected, metadata)
        (self.source / "local-change").write_text("modified source")
        result = subprocess.run(["sh", "-c", '. "$1"; installed_daemon_matches "$2" "$3"',
                                 "test", str(self.source / "scripts/install-common.sh"), str(binary), str(self.source)])
        self.assertEqual(result.returncode, 1)

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


if __name__ == "__main__":
    unittest.main()
