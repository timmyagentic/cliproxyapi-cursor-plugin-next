"""Check the boundaries that could produce an uninstallable store release."""
import importlib.util
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


release = load("release", "release.py")
submit = load("submit", "submit-store-pr.py")


class StoreReleaseTests(unittest.TestCase):
    def setUp(self):
        self.plugin = release.metadata()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        for (goos, goarch), (ext, machine) in release.TARGETS.items():
            # Synthetic headers exercise artifact validation; CI separately
            # builds and loads each real library through its C ABI.
            data = bytearray(256)
            if goos == "linux":
                data[:6] = b"\x7fELF\x02\x01"
                struct.pack_into("<HH", data, 16, 3, machine)
            elif goos == "darwin":
                struct.pack_into("<IIII", data, 0, 0xfeedfacf, machine, 0, 6)
            else:
                data[:2] = b"MZ"
                struct.pack_into("<I", data, 0x3c, 128)
                data[128:132] = b"PE\0\0"
                struct.pack_into("<H", data, 132, machine)
                struct.pack_into("<H", data, 150, 0x2000)
            path = self.directory / release.asset_name(self.plugin, goos, goarch)
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr(self.plugin["id"] + "." + ext, data)

    def linux_archive(self):
        return self.directory / release.asset_name(self.plugin, "linux", "amd64")

    def test_complete_release_and_tampered_checksum(self):
        expected = release.checksums(self.directory, self.plugin, complete=True)
        self.assertEqual(len(expected.splitlines()), 5)
        with zipfile.ZipFile(self.linux_archive(), "a") as archive:
            archive.comment = b"modified after checksumming"
        self.assertNotEqual(release.checksums(self.directory, self.plugin, complete=True), expected)

    def test_missing_platform_is_rejected(self):
        for target in release.TARGETS:
            path = self.directory / release.asset_name(self.plugin, *target)
            data = path.read_bytes()
            with self.subTest(target=target):
                path.unlink()
                with self.assertRaises(AssertionError):
                    release.checksums(self.directory, self.plugin, complete=True)
                path.write_bytes(data)

    def test_darwin_binary_architecture_and_library_type(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                path = self.directory / release.asset_name(self.plugin, "darwin", arch)
                with zipfile.ZipFile(path) as archive:
                    data = bytearray(archive.read(archive.namelist()[0]))
                other = "arm64" if arch == "amd64" else "amd64"
                with self.assertRaisesRegex(AssertionError, "architecture mismatch"):
                    release.check_binary(data, "darwin", other)
                struct.pack_into("<I", data, 12, 2)
                with self.assertRaisesRegex(AssertionError, "dynamic library"):
                    release.check_binary(data, "darwin", arch)
                with self.assertRaisesRegex(AssertionError, "Mach-O"):
                    release.check_binary(b"invalid header", "darwin", arch)

    def test_unsafe_and_nested_zip_paths_are_rejected(self):
        for name in ("../spx-cursor.so", "dist/spx-cursor.so", "/spx-cursor.so"):
            with self.subTest(name=name):
                with zipfile.ZipFile(self.linux_archive(), "w") as archive:
                    archive.writestr(name, b"untrusted")
                with self.assertRaises(AssertionError):
                    release.verify_archive(self.linux_archive(), self.plugin, "linux", "amd64")

    def test_wrong_binary_architecture_is_rejected(self):
        with zipfile.ZipFile(self.linux_archive()) as archive:
            data = archive.read(archive.namelist()[0])
        with self.assertRaisesRegex(AssertionError, "architecture mismatch"):
            release.check_binary(data, "linux", "arm64")

    def test_registry_append_preserves_existing_entries_and_rejects_duplicates(self):
        other = {"id": "other", "repository": "https://github.com/other/plugin"}
        original = json.dumps({"schema_version": 1, "plugins": [other]}, indent=2) + "\n"
        result = submit.append_entry(original, self.plugin)
        self.assertEqual(json.loads(result)["plugins"], [other, self.plugin])
        self.assertTrue(result.startswith(original[:original.rfind("]")].rstrip()))
        with self.assertRaises(AssertionError):
            submit.append_entry(result, self.plugin)

    def test_store_submission_stops_before_writing_when_release_is_missing(self):
        with patch.object(submit.sys, "argv", ["submit-store-pr.py", "--submit"]), \
                patch.dict(submit.os.environ, {"GH_TOKEN": "synthetic-test-token"}), \
                patch.object(submit, "api", return_value={
                    "tag_name": "v" + self.plugin["version"], "draft": False, "prerelease": False, "assets": [],
                }) as api:
            with self.assertRaisesRegex(AssertionError, "missing release asset"):
                submit.main()
            self.assertEqual(api.call_count, 1)
            self.assertTrue(api.call_args.args[0].endswith("/releases/latest"))

    def test_store_submission_rejects_release_without_darwin(self):
        assets = [{"name": release.asset_name(self.plugin, *target), "size": 256}
                  for target in release.TARGETS if target[0] != "darwin"]
        assets.append({"name": "checksums.txt", "size": 256})
        with patch.object(submit.sys, "argv", ["submit-store-pr.py", "--submit"]), \
                patch.dict(submit.os.environ, {"GH_TOKEN": "synthetic-test-token"}), \
                patch.object(submit, "api", return_value={
                    "tag_name": "v" + self.plugin["version"], "draft": False,
                    "prerelease": False, "assets": assets,
                }) as api:
            with self.assertRaisesRegex(AssertionError, "missing release asset: .*darwin"):
                submit.main()
            self.assertEqual(api.call_count, 1)

    @unittest.skipIf(os.name == "nt", "Linux build script requires Bash")
    def test_linux_build_uses_explicit_python_without_python_on_path(self):
        bin_dir = self.directory / "bin"
        bin_dir.mkdir()
        readelf = bin_dir / "readelf"
        readelf.write_text("#!/bin/sh\nprintf 'Name: GLIBC_2.17\\n'\n")
        readelf.chmod(0o755)
        env = dict(os.environ, PATH=str(bin_dir), BUILD_PYTHON=sys.executable)
        # Stub only the Go compilation; exercise the actual shell script and
        # Python/readelf glibc check in an environment with no python3 on PATH.
        command = ["/bin/bash", "-c", 'go() { return 0; }; export -f go; exec /bin/bash "$1"',
                   "test", str(ROOT / "scripts/build-linux.sh")]
        result = subprocess.run(command, env=env, cwd=self.directory, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("glibc <= 2.17", result.stdout)

        readelf.write_text("#!/bin/sh\nprintf 'Name: GLIBC_2.28\\n'\n")
        result = subprocess.run(command, env=env, cwd=self.directory, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("glibc baseline exceeded", result.stderr)


if __name__ == "__main__":
    unittest.main()
