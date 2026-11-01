"""Run the real activation shell in an isolated installation; never stop host services."""
import os
import platform
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
        self.base = Path(self.temp.name).resolve()
        self.prefix = self.base / "installation with spaces"
        self.root = self.prefix / "daemon"
        self.component = "daemon"
        self.old = self.root / "releases" / "old"
        self.old.mkdir(parents=True)
        (self.old / "INSTALLATION").write_text("version=1.0.0\n")
        if platform.system() == "Darwin":
            shutil.copytree(self.old, self.root / "current")
            (self.root / "current" / "RELEASE").write_text(str(self.old) + "\n")
        else:
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
        self.tool("systemctl", '''
case "$*" in
  *stop*) rm -f "$FIXTURE_RUNNING" ;;
  *start*) touch "$FIXTURE_RUNNING" ;;
  *is-active*) test -f "$FIXTURE_RUNNING" ;;
  *MainPID*) echo 999999 ;;
esac
''')
        self.tool("launchctl", '''
case "$1" in
  bootout) rm -f "$FIXTURE_RUNNING" ;;
  bootstrap) touch "$FIXTURE_RUNNING" ;;
  print)
    case "$2" in
      */com.zotigo.*) test -f "$FIXTURE_RUNNING"; printf 'state = running\\npid = 999999\\n' ;;
    esac ;;
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
        # Keep retry tests fast; process checks use native ps and /proc or lsof.
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

    def test_activate_preserves_platform_layout_and_releases_lock(self):
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.root / "current").is_symlink(), platform.system() != "Darwin")
        if platform.system() == "Darwin":
            release = Path((self.root / "current" / "RELEASE").read_text().strip())
            self.assertEqual((release / "INSTALLATION").read_text(), "version=1.0.1\n")
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.1\n")
        self.assertFalse((self.root / "install.lock").exists())
        self.assertFalse((self.work / "journal").exists())
        self.assertEqual((self.root / "previous").resolve(), self.old)

    def test_symlink_activation_never_moves_current_out_of_the_way(self):
        # Web uses atomic symlink replacement on both Linux and macOS.
        self.use_web()
        real_mv = shutil.which("mv")
        self.tool("mv", f'''[ "$1" != "$FIXTURE_ROOT/current" ] || exit 97
exec '{real_mv}' "$@"
''')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / "current").exists())

    def use_web(self):
        self.component = "web"
        old_root = self.root
        self.root = self.prefix / "web"
        old_root.rename(self.root)
        if (self.root / "current").is_symlink():
            (self.root / "current").unlink()
        else:
            shutil.rmtree(self.root / "current")
        (self.root / "current").symlink_to(self.root / "releases" / "old")
        new_work = self.prefix / "updates" / "web"
        self.work.rename(new_work)
        self.work = new_work
        self.env["FIXTURE_ROOT"] = str(self.root)
        (self.prefix / "config" / "web.env").write_text("ZOTIGO_WEB_HOST=127.0.0.1\nZOTIGO_WEB_PORT=9999\nZOTIGO_WEB_ORIGIN=https://app.example.test\n")
        self.tool("curl", '''case "$*" in *"Host: app.example.test"*) echo '<html>ok</html>' ;; *) exit 22 ;; esac\n''')

    def test_web_custom_origin_is_used_for_health_check(self):
        self.use_web()
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_cancel_waiting_keeps_prepared_release_and_service(self):
        (self.work / "cancel").write_text("cancel")
        result = self.apply()
        self.assertEqual(result.returncode, 10)
        self.assert_old_current()
        self.assertTrue((self.root / "prepared").exists())
        self.assertTrue(self.running.exists())

    def test_failed_health_restores_old_installation(self):
        result = self.apply(FAIL_VERIFY="1")
        self.assertNotEqual(result.returncode, 0)
        self.assert_old_current()
        self.assertTrue(self.running.exists())
        self.assertFalse((self.work / "draining").exists())
        self.assertTrue((self.work / "journal").exists(), "failed recovery must retain its journal")
        self.assertTrue((self.root / "install.lock").exists())
        self.assertIn("did not recover", result.stderr)

    def test_concurrent_installer_is_not_interrupted(self):
        (self.root / "install.lock").mkdir()
        result = self.apply()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.running.exists())
        self.assert_old_current()
        self.assertTrue((self.root / "install.lock").exists())

    def delayed_macos(self):
        # Simulate launchd independently of the host OS; never invoke real services.
        if (self.root / "current").is_symlink():
            (self.root / "current").unlink()
            shutil.copytree(self.old, self.root / "current")
            (self.root / "current" / "RELEASE").write_text(str(self.old) + "\n")
        self.tool("uname", "echo Darwin\n")
        self.tool("lsof", '''[ ! -f "$FIXTURE_RUNNING" ] || echo "n$FIXTURE_ROOT/current/zotigod"
''')
        real_mv = shutil.which("mv")
        self.tool("mv", f'''if [ "$1" = -fh ] && [ "{platform.system()}" = Linux ]; then shift; exec '{real_mv}' -fT "$@"; fi
exec '{real_mv}' "$@"
''')
        self.tool("launchctl", '''
case "$1" in
  bootout) echo 5 > "$FIXTURE_RUNNING.stopping" ;;
  bootstrap)
    [ ! -f "$FIXTURE_RUNNING.stopping" ] || exit 5
    n=0; [ ! -f "$FIXTURE_RUNNING.starts" ] || n=$(cat "$FIXTURE_RUNNING.starts")
    n=$((n + 1)); echo "$n" > "$FIXTURE_RUNNING.starts"
    [ "$n" -gt 2 ] || exit 5
    touch "$FIXTURE_RUNNING" ;;
  print)
    case "$2" in
      */com.zotigo.*) [ -f "$FIXTURE_RUNNING" ] || exit 113
        printf 'state = running\\npid = 999999\\n' ;;
    esac ;;
esac
''')
        self.tool("sleep", '''
if [ -f "$FIXTURE_RUNNING.stopping" ]; then
  n=$(cat "$FIXTURE_RUNNING.stopping")
  n=$((n - 1))
  if [ "$n" -le 0 ] && [ "${STUCK_STOP:-0}" != 1 ]; then
    rm -f "$FIXTURE_RUNNING" "$FIXTURE_RUNNING.stopping"
  else echo "$n" > "$FIXTURE_RUNNING.stopping"; fi
fi
''')
        self.tool("curl", '''
case "$*" in
  *updates/drain*) echo '{"data":true}' ;;
  */health*)
    version=$(sed -n 's/^version=//p' "$FIXTURE_ROOT/current/INSTALLATION")
    [ "${FAIL_NEW_VERIFY:-0}:$version" != "1:1.0.1" ] || exit 1
    printf '{"version":"%s","process_id":"999999"}' "$version"
    echo "$version" >> "$FIXTURE_RUNNING.verified" ;;
  */projects*) echo '{}' ;;
esac
''')

    def test_macos_waits_for_exit_and_retries_bootstrap(self):
        self.delayed_macos()
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.1\n")
        self.assertTrue(self.running.exists())
        self.assertIn("1.0.1", Path(str(self.running) + ".verified").read_text())

    def test_macos_failed_activation_waits_and_verifies_rollback(self):
        self.delayed_macos()
        result = self.apply(FAIL_NEW_VERIFY="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.0\n")
        self.assertTrue(self.running.exists())
        self.assertIn("1.0.0", Path(str(self.running) + ".verified").read_text())
        self.assertFalse((self.root / "install.lock").exists())

    def test_macos_failure_before_swap_restarts_and_verifies_old_service(self):
        self.delayed_macos()
        real_mv = shutil.which("mv")
        self.tool("mv", f'''[ "$1" != "$FIXTURE_ROOT/prepared" ] || exit 1
exec '{real_mv}' "$@"
''')
        result = self.apply()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.0\n")
        self.assertTrue(self.running.exists())
        self.assertIn("1.0.0", Path(str(self.running) + ".verified").read_text())
        self.assertFalse((self.root / "install.lock").exists())

    def test_macos_stop_timeout_never_replaces_running_installation(self):
        self.delayed_macos()
        result = self.apply(STUCK_STOP="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.0\n")
        self.assertTrue((self.root / "prepared").exists())
        self.assertTrue(self.running.exists())

    def assert_old_current(self):
        self.assertEqual((self.root / "current" / "INSTALLATION").read_text(), "version=1.0.0\n")
        if platform.system() == "Darwin":
            self.assertFalse((self.root / "current").is_symlink())
            self.assertEqual(Path((self.root / "current" / "RELEASE").read_text().strip()), self.old)
        else:
            self.assertEqual((self.root / "current").resolve(), self.old)


if __name__ == "__main__":
    unittest.main()
