"""Offline toolchain tests: real archives/checksums/filesystem, stubbed downloads."""
import hashlib
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]


class ToolchainsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="zotigo-toolchains-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.prefix = self.base / "private install"
        self.bin = self.base / "bin"
        self.bin.mkdir()
        self.source = self.base / "source"
        self.source.mkdir()
        (self.source / "go.mod").write_text("module fixture\n\ngo 1.25.3\n")
        (self.source / "package.json").write_text('{"packageManager":"pnpm@10.30.3"}')
        self.manifest = self.base / "toolchains.tsv"
        self.downloads = self.base / "downloads"
        self.downloads.mkdir()
        self.calls = self.base / "calls"
        self.executable(self.bin / "curl", '''#!/bin/sh
while [ "$#" -gt 0 ]; do
 case "$1" in https://*) name=${1##*/} ;; -o) shift; target=$1 ;; esac
 shift
done
printf '%s\\n' "$name" >> "$CALLS"
[ "${FAIL_DOWNLOAD:-0}" = 0 ] || exit 22
cp "$DOWNLOADS/$name" "$target"
''')
        self.executable(self.bin / "uname", '#!/bin/sh\necho "${TEST_ARCH:-x86_64}"\n')
        for name in ["go", "node", "pnpm"]:
            self.executable(self.bin / name, "#!/bin/sh\nexit 127\n")
        rows = []
        for tool, version in [("go", "1.25.3"), ("node", "22.23.2"), ("pnpm", "10.30.3")]:
            payload = self.base / f"payload-{tool}" / "bin"
            payload.mkdir(parents=True)
            if tool == "go":
                script = '#!/bin/sh\necho "go version go1.25.3 fixture/arch"\n'
                self.executable(payload / "go", script)
            elif tool == "node":
                script = '''#!/bin/sh
case "$2" in process.versions.node) echo 22.23.2 ;; process.execPath) echo "$0" ;;
 *packageManager*) echo 10.30.3 ;;
 *) case "$1" in */pnpm.cjs) exec sh "$@" ;; *) exit 1 ;; esac ;;
esac
'''
                self.executable(payload / "node", script)
            else:
                self.executable(payload / "pnpm.cjs", '#!/bin/sh\necho 10.30.3\n')
            archive = self.downloads / f"{tool}.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                bundle.add(payload.parent, arcname="package")
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            for system in ["linux", "darwin"]:
                for arch in ["amd64", "arm64"]:
                    rows.append(f"{tool} {version} {system} {arch} {digest} https://example.invalid/{tool}.tar.gz")
        self.manifest.write_text("\n".join(rows) + "\n")
        self.env = {**os.environ, "PATH": f"{self.bin}:/usr/bin:/bin", "HOME": str(self.base / "home"),
                    "DOWNLOADS": str(self.downloads), "CALLS": str(self.calls)}

    def executable(self, path, script):
        path.write_text(script)
        path.chmod(0o755)

    def run_tools(self, body, **env):
        command = '''set -eu
. "$1/scripts/install-common.sh"
. "$1/scripts/toolchains.sh"
prefix=$2 toolchain_manifest=$3 source=$4 platform=${TEST_PLATFORM:-Linux}
''' + body
        return subprocess.run(["sh", "-c", command, "test", str(REPO), str(self.prefix),
                               str(self.manifest), str(self.source)],
                              env={**self.env, **env}, text=True, capture_output=True)

    def test_local_sufficient_go_and_node_are_reused_without_download(self):
        self.executable(self.bin / "go", '#!/bin/sh\necho "go version go1.26.1 fixture/arch"\n')
        self.executable(self.bin / "node", '#!/bin/sh\ncase "$2" in process.versions.node) echo 24.0.0 ;; process.execPath) echo "$0" ;; esac\n')
        result = self.run_tools('ensure_go "$source"; ensure_node')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.calls.exists())
        self.assertFalse(self.prefix.exists())

    def test_missing_tools_download_and_reuse_private_cache_on_both_platforms(self):
        for platform, arch in [("Linux", "x86_64"), ("Linux", "aarch64"), ("Darwin", "x86_64"), ("Darwin", "arm64")]:
            with self.subTest(platform=platform, arch=arch):
                self.prefix = self.base / f"install-{platform}-{arch}"
                body = 'ensure_go "$source"; ensure_node; install_tool pnpm 10.30.3; test -x "$prefix/toolchains/pnpm-10.30.3/bin/pnpm"'
                result = self.run_tools(body, TEST_PLATFORM=platform, TEST_ARCH=arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = self.calls.read_text()
                result = self.run_tools(body, TEST_PLATFORM=platform, TEST_ARCH=arch, FAIL_DOWNLOAD="1")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.calls.read_text(), calls)
                self.assertFalse(list(self.prefix.glob("toolchains/*.lock")))

    def test_old_or_prerelease_versions_use_private_tools(self):
        self.executable(self.bin / "go", '#!/bin/sh\necho "go version go1.24.9 fixture/arch"\n')
        self.executable(self.bin / "node", '#!/bin/sh\necho 26.0.0-rc.1\n')
        result = self.run_tools('ensure_go "$source"; ensure_node')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(), ["go.tar.gz", "node.tar.gz"])
        self.assertIn("go1.24.9", (self.bin / "go").read_text())
        self.assertFalse((self.base / "home/.profile").exists())

    def test_pnpm_exact_pin_replaces_only_installer_path_selection(self):
        self.executable(self.bin / "pnpm", '#!/bin/sh\necho 9.0.0\n')
        result = self.run_tools('ensure_node; ensure_pnpm "$source"; pnpm --version')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.rstrip().endswith("10.30.3"))
        self.assertIn("9.0.0", (self.bin / "pnpm").read_text())
        calls = self.calls.read_text()
        self.executable(self.bin / "pnpm", '#!/bin/sh\necho 10.30.3\n')
        result = self.run_tools('ensure_node; ensure_pnpm "$source"', FAIL_DOWNLOAD="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text(), calls)

    def test_bad_checksum_never_publishes_and_releases_lock(self):
        with (self.downloads / "go.tar.gz").open("ab") as archive:
            archive.write(b"tampered")
        result = self.run_tools('ensure_go "$source"')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Checksum mismatch", result.stderr)
        self.assertFalse((self.prefix / "toolchains/go-1.25.3").exists())
        self.assertEqual(list((self.prefix / "toolchains").iterdir()), [])

    def test_download_failure_preserves_other_toolchains(self):
        marker = self.prefix / "toolchains/node-existing/keep"
        marker.parent.mkdir(parents=True)
        marker.write_text("in-use runtime")
        result = self.run_tools('ensure_go "$source"', FAIL_DOWNLOAD="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(marker.read_text(), "in-use runtime")
        self.assertFalse((self.prefix / "toolchains/go-1.25.3.lock").exists())

    def test_shared_tool_lock_blocks_concurrent_publication(self):
        lock = self.prefix / "toolchains/go-1.25.3.lock"
        lock.mkdir(parents=True)
        result = self.run_tools('ensure_go "$source"')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Another installer", result.stderr)
        self.assertTrue(lock.is_dir())
        self.assertFalse(self.calls.exists())

    def test_newer_source_requires_an_explicit_new_pin(self):
        (self.source / "go.mod").write_text("module fixture\ngo 9.0.0\n")
        result = self.run_tools('ensure_go "$source"')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("No pinned go 9.0.0", result.stderr)
        self.assertFalse(self.calls.exists())


if __name__ == "__main__":
    unittest.main()
