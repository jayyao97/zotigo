"""Run the real activation shell in an isolated installation; never stop host services."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]


class ActivationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="zotigo-update-test-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.prefix = self.base / "installation with spaces"
        self.root = self.prefix / "daemon"
        self.component = "daemon"
        self.old = self.root / "releases" / "old"
        self.old.mkdir(parents=True)
        (self.old / "INSTALLATION").write_text("version=1.0.0\n")
        (self.root / "current").symlink_to(self.old)
        prepared = self.root / "prepared"
        prepared.mkdir()
        (prepared / "INSTALLATION").write_text("version=1.0.1\n")
        self.work = self.prefix / "updates" / "daemon"
        self.work.mkdir(parents=True)
        for source, target in [("apply-update.sh", "apply.sh"), ("install-common.sh", "install-common.sh")]:
            shutil.copy(REPO / "scripts" / source, self.work / target)
        config = self.prefix / "config"
        config.mkdir()
        (config / "token").write_text("fixture-token")
        (config / "daemon.env").write_text(f"ZOTIGOD_ADDR=127.0.0.1:9999\nZOTIGOD_AUTH_TOKEN_FILE='{config / 'token'}'\n")
        self.tools = self.base / "tools"
        self.tools.mkdir()
        self.tool("uname", "echo Linux\n")
        self.tool("systemctl", '''
case "$*" in
  *stop*) rm -f "$FIXTURE_RUNNING" ;;
  *start*) touch "$FIXTURE_RUNNING" ;;
  *is-active*) test -f "$FIXTURE_RUNNING" ;;
  *MainPID*) echo 999999 ;;
esac
''')
        self.tool("curl", '''
case "$*" in
  *updates/drain*) echo '{"data":true}' ;;
  */health*)
    version=$(sed -n 's/^version=//p' "$FIXTURE_ROOT/current/INSTALLATION")
    [ "${FAIL_VERIFY:-0}" != 1 ] || exit 1
    printf '{"version":"%s","process_id":"999999"}' "$version" ;;
  */projects*) echo '{}' ;;
esac
''')
        # Keep retry tests fast; process checks still use the real ps and /proc.
        self.tool("sleep", "exit 0\n")
        self.running = self.base / "running"
        self.running.touch()
        self.env = {**os.environ, "HOME": str(self.base / "home"), "PATH": f"{self.tools}:{os.environ['PATH']}",
                    "FIXTURE_RUNNING": str(self.running), "FIXTURE_ROOT": str(self.root)}

    def tool(self, name, body):
        script = self.tools / name
        script.write_text("#!/bin/sh\nset -eu\n" + body)
        script.chmod(0o755)

    def apply(self, **extra):
        return subprocess.run(["sh", str(self.work / "apply.sh"), str(self.prefix), self.component],
                              env={**self.env, **extra}, capture_output=True, text=True, timeout=20)

    def test_activate_preserves_symlink_layout_and_releases_lock(self):
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / "current").is_symlink())
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.1\n")
        self.assertFalse((self.root / "install.lock").exists())
        self.assertFalse((self.work / "journal").exists())
        self.assertEqual((self.root / "previous").resolve(), self.old)

    def test_symlink_activation_never_moves_current_out_of_the_way(self):
        real_mv = shutil.which("mv")
        self.tool("mv", f'''[ "$1" != "$FIXTURE_ROOT/current" ] || exit 97
exec '{real_mv}' "$@"
''')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / "current").exists())

    def test_web_custom_origin_is_used_for_health_check(self):
        self.component = "web"
        old_root = self.root
        self.root = self.prefix / "web"
        old_root.rename(self.root)
        (self.root / "current").unlink()
        (self.root / "current").symlink_to(self.root / "releases" / "old")
        new_work = self.prefix / "updates" / "web"
        self.work.rename(new_work)
        self.work = new_work
        self.env["FIXTURE_ROOT"] = str(self.root)
        (self.prefix / "config" / "web.env").write_text("ZOTIGO_WEB_HOST=127.0.0.1\nZOTIGO_WEB_PORT=9999\nZOTIGO_WEB_ORIGIN=https://app.example.test\n")
        self.tool("curl", '''case "$*" in *"Host: app.example.test"*) echo '<html>ok</html>' ;; *) exit 22 ;; esac\n''')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_cancel_waiting_keeps_prepared_release_and_service(self):
        (self.work / "cancel").write_text("cancel")
        result = self.apply()
        self.assertEqual(result.returncode, 10)
        self.assertEqual((self.root / "current").resolve(), self.old)
        self.assertTrue((self.root / "prepared").exists())
        self.assertTrue(self.running.exists())

    def test_failed_health_restores_old_installation(self):
        result = self.apply(FAIL_VERIFY="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "current").resolve(), self.old)
        self.assertTrue(self.running.exists())
        self.assertFalse((self.work / "draining").exists())

    def test_concurrent_installer_is_not_interrupted(self):
        (self.root / "install.lock").mkdir()
        result = self.apply()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.running.exists())
        self.assertEqual((self.root / "current").resolve(), self.old)
        self.assertTrue((self.root / "install.lock").exists())


if __name__ == "__main__":
    unittest.main()
