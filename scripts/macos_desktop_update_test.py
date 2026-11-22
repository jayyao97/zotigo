"""Two Desktop activations in a private prefix and GUI app, using the real launchd."""
import json
import os
from pathlib import Path
import platform
import plistlib
import shlex
import shutil
import subprocess
import tempfile
import time
import unittest
import uuid

REPO = Path(__file__).resolve().parents[1]


def wait_for(description, predicate, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.15)
    raise AssertionError(f"Timed out waiting for {description}")


@unittest.skipUnless(platform.system() == "Darwin", "requires macOS LaunchServices")
class MacDesktopUpdateTest(unittest.TestCase):
    def test_two_updates_collect_launch_agents(self):
        domain = f"gui/{os.getuid()}"
        if subprocess.run(["launchctl", "print", domain], stdout=subprocess.DEVNULL,
                          stderr=subprocess.DEVNULL).returncode:
            self.skipTest("requires a logged-in GUI user")
        temp = tempfile.TemporaryDirectory(prefix="zotigo-dock-test-")
        self.addCleanup(temp.cleanup)
        prefix = Path(temp.name).resolve()
        bundle_id = f"com.zotigo.fixture.x{uuid.uuid4().hex}"
        app_name = "Zotigo Dock Fixture " + bundle_id[-8:]
        work = prefix / "updates/desktop"
        work.mkdir(parents=True)
        root = prefix / "desktop"
        initial = root / "releases/initial"
        contents = initial / "Zotigo.app/Contents"
        (contents / "MacOS").mkdir(parents=True)
        (root / "current").symlink_to(initial)
        # A foreground app must initiate both operations: an updater launched by
        # the test process would exercise launchd but not LaunchServices attribution.
        (contents / "Info.plist").write_bytes(plistlib.dumps({
            "CFBundleIdentifier": bundle_id, "CFBundleExecutable": "fixture",
            "CFBundleName": app_name, "CFBundleDisplayName": app_name, "CFBundlePackageType": "APPL",
            "CFBundleVersion": "1", "FixturePrefix": str(prefix),
        }))
        source = prefix / "fixture.m"
        source.write_text(r'''
#import <Cocoa/Cocoa.h>
#include <unistd.h>
int main(void) { @autoreleasepool {
    NSApplication *app = [NSApplication sharedApplication];
    [app setActivationPolicy:NSApplicationActivationPolicyRegular];
    [app finishLaunching];
    NSBundle *bundle = [NSBundle mainBundle];
    NSString *prefix = [bundle objectForInfoDictionaryKey:@"FixturePrefix"];
    NSString *release = bundle.bundlePath.stringByResolvingSymlinksInPath.stringByDeletingLastPathComponent;
    NSString *metadata = [NSString stringWithContentsOfFile:[release stringByAppendingPathComponent:@"INSTALLATION"]
                                                   encoding:NSUTF8StringEncoding error:nil];
    NSString *version = [[metadata componentsSeparatedByString:@"\n"][0] substringFromIndex:8];
    NSString *receipt = [NSString stringWithFormat:@"{\"version\":\"%@\",\"pid\":%d}", version, getpid()];
    [receipt writeToFile:[prefix stringByAppendingPathComponent:@"updates/desktop/started.json"]
              atomically:YES encoding:NSUTF8StringEncoding error:nil];
    NSString *request = [prefix stringByAppendingPathComponent:@"request"];
    [NSTimer scheduledTimerWithTimeInterval:0.15 repeats:YES block:^(NSTimer *timer) {
        NSString *action = [NSString stringWithContentsOfFile:request encoding:NSUTF8StringEncoding error:nil];
        if (!action) return;
        [[NSFileManager defaultManager] removeItemAtPath:request error:nil];
        if ([action isEqual:@"quit"]) { [app terminate:nil]; return; }
        NSTask *task = [NSTask new];
        task.launchPath = [release stringByAppendingPathComponent:@"zotigo-update"];
        task.arguments = @[action, @"--component", @"desktop", @"--prefix", prefix];
        [task launch]; [task waitUntilExit];
        if ([action isEqual:@"apply"] && task.terminationStatus == 0) [app terminate:nil];
    }];
    [app run];
} return 0; }
''')
        subprocess.run(["clang", "-fobjc-arc", "-framework", "Cocoa", str(source),
                        "-o", str(contents / "MacOS/fixture")], check=True)
        subprocess.run(["codesign", "-s", "-", "--force", str(contents.parent)], check=True)
        helper = prefix / "helper"
        subprocess.run(["go", "build", "-o", str(helper), "./cmd/zotigo-update"], cwd=REPO, check=True)
        repository = prefix / "repo"
        subprocess.run(["git", "init", "-q", str(repository)], check=True)
        # The detached worker inherits no test environment. Scope the local
        # source override to this fixture's updater, including its copied runner.
        shim = initial / "zotigo-update"
        legacy_labels = [f"{bundle_id}.{action}" for action in ("prepare", "apply")]
        seed = ""
        for label in legacy_labels:
            path = prefix / (label + ".plist")
            path.write_bytes(plistlib.dumps({"Label": label, "ProgramArguments": ["/usr/bin/true"], "RunAtLoad": True}))
            seed += f"launchctl bootstrap {domain} {shlex.quote(str(path))} || exit 1\n"
        shim.write_text('#!/bin/sh\nif [ "$1" = seed ]; then\n' + seed + 'exit 0\nfi\n' +
                        "export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=" +
                        shlex.quote(f"url.{repository}.insteadOf") +
                        " GIT_CONFIG_VALUE_0=https://github.com/jayyao97/zotigo-ui.git\nexec " +
                        shlex.quote(str(helper)) + ' "$@"\n')
        shim.chmod(0o755)
        support = initial / "update-support"
        support.mkdir()
        for name in ("apply-update.sh", "install-common.sh"):
            shutil.copy(REPO / "scripts" / name, support / name)
        (repository / "install.sh").write_text('''set -eu
cd "$2"  # --source checkout
while [ "$1" != --prefix ]; do shift; done
root="$2/desktop"
cp -R "$root/current/." "$root/prepared"
cp update.json "$root/prepared/UPDATE.json"
cp INSTALLATION "$root/prepared/INSTALLATION"
''')

        def metadata(version):
            return json.dumps(dict(version=version, update_protocol=1, api_epoch=1, data_epoch=1))
        (initial / "INSTALLATION").write_text("version=1.0.0\n")
        (initial / "UPDATE.json").write_text(metadata("1.0.0"))
        receipt = work / "started.json"
        request = prefix / "request"

        def live_version(version):
            if not receipt.exists():
                return False
            info = json.loads(receipt.read_text())
            if info["version"] != version:
                return False
            os.kill(info["pid"], 0)
            return True

        def app_records():
            return subprocess.check_output(["lsappinfo", "list"], text=True).count(f'bundleID="{bundle_id}"')

        def phase(expected):
            status = json.loads((work / "status.json").read_text())
            self.assertNotEqual(status["phase"], "failed", status.get("error"))
            return status["phase"] == expected

        # All cleanup targets belong to this fixture.
        labels = set(legacy_labels)

        def cleanup():
            request.write_text("quit")
            worker_plist = work / "worker.plist"
            if worker_plist.exists():
                labels.add(plistlib.loads(worker_plist.read_bytes())["Label"])
            for label in labels:
                subprocess.run(["launchctl", "bootout", domain + "/" + label], capture_output=True)
            wait_for("fixture quit", lambda: app_records() == 0, 15)
        self.addCleanup(cleanup)
        subprocess.run(["open", str(root / "current/Zotigo.app")], check=True)
        wait_for("initial app", lambda: live_version("1.0.0") and app_records() == 1)
        request.write_text("seed")
        for label in legacy_labels:
            wait_for(label, lambda: "last exit code = 0" in subprocess.run(
                ["launchctl", "print", domain + "/" + label], capture_output=True, text=True).stdout)
        for version in ("1.0.1", "1.0.2"):
            (repository / "update.json").write_text(metadata(version))
            (repository / "INSTALLATION").write_text(f"version={version}\n")
            subprocess.run(["git", "add", "."], cwd=repository, check=True)
            subprocess.run(["git", "-c", "user.email=test@example.invalid", "-c", "user.name=Fixture",
                            "commit", "-qm", version], cwd=repository, check=True)
            subprocess.run(["git", "tag", "v" + version], cwd=repository, check=True)
            subprocess.run([str(shim), "check", "--component", "desktop", "--prefix", str(prefix)], check=True)
            for action, expected in (("prepare", "ready"), ("apply", "completed")):
                request.write_text(action)
                wait_for(expected, lambda: phase(expected))
                label = plistlib.loads((work / "worker.plist").read_bytes())["Label"]
                self.assertNotIn(label, labels)
                labels.add(label)
                wait_for(f"{label} bootout", lambda: subprocess.run(
                    ["launchctl", "print", domain + "/" + label], capture_output=True).returncode != 0)
            if version == "1.0.1":
                # Observe before intervening: LaunchServices retention varies by macOS version.
                print("LaunchServices records with two completed legacy jobs:", app_records(), flush=True)
                for label in legacy_labels:
                    subprocess.run(["launchctl", "bootout", domain + "/" + label], check=True)
            wait_for(version, lambda: live_version(version) and app_records() == 1)
            # Optional in a GUI test account that already permits Dock accessibility.
            if os.environ.get("ZOTIGO_TEST_DOCK_UI") == "1":
                query = ('tell application "System Events" to tell process "Dock" to count '
                         f'(UI elements of list 1 whose name is "{app_name}" and subrole is "AXApplicationDockItem")')
                wait_for("one visible fixture Dock tile", lambda: subprocess.check_output(
                    ["osascript", "-e", query], text=True).strip() == "1")


if __name__ == "__main__":
    unittest.main()
