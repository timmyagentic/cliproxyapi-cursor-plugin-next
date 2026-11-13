"""Check the boundaries that could produce an uninstallable store release."""
import importlib.util
import hashlib
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
            data = bytearray(512)
            if goos == "linux":
                data[:6] = b"\x7fELF\x02\x01"
                struct.pack_into("<HH", data, 16, 3, machine)
                struct.pack_into("<Q", data, 32, 64)
                struct.pack_into("<HH", data, 54, 56, 1)
                struct.pack_into("<IIQQQQQQ", data, 64, 1, 4, 256, 256, 256, 256, 256, 1)
            elif goos == "darwin":
                struct.pack_into("<IIII", data, 0, 0xfeedfacf, machine, 0, 6)
                struct.pack_into("<II", data, 16, 1, 72)
                struct.pack_into("<II", data, 32, 0x19, 72)
                struct.pack_into("<QQ", data, 72, 256, 256)
                struct.pack_into("<I", data, 92, 1)
            else:
                data[:2] = b"MZ"
                struct.pack_into("<I", data, 0x3c, 128)
                data[128:132] = b"PE\0\0"
                struct.pack_into("<H", data, 132, machine)
                struct.pack_into("<H", data, 134, 1)
                struct.pack_into("<H", data, 150, 0x2000)
                struct.pack_into("<II", data, 168, 256, 256)
                struct.pack_into("<I", data, 188, 0x40000040)
            stamp = release.build_stamp("a" * 40, False)
            data[256:256 + len(stamp)] = stamp
            path = self.directory / release.asset_name(self.plugin, goos, goarch)
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr(self.plugin["id"] + "." + ext, data)
            release.provenance_path(path).write_text(json.dumps({
                "schema_version": 1, "plugin_id": self.plugin["id"], "version": self.plugin["version"],
                "module_path": self.plugin["repository"].removeprefix("https://"),
                "source_commit": "a" * 40, "source_dirty": False, "go_version": "go1.26.10",
                "goos": goos, "goarch": goarch, "library_sha256": hashlib.sha256(data).hexdigest(),
            }))

    def linux_archive(self):
        return self.directory / release.asset_name(self.plugin, "linux", "amd64")

    def test_complete_release_and_tampered_checksum(self):
        expected = release.checksums(self.directory, self.plugin, complete=True)
        self.assertEqual(len(expected.splitlines()), 10)
        with zipfile.ZipFile(self.linux_archive(), "a") as archive:
            archive.comment = b"modified after checksumming"
        self.assertNotEqual(release.checksums(self.directory, self.plugin, complete=True), expected)

    def test_provenance_is_required_and_binds_the_library_bytes(self):
        proof = release.provenance_path(self.linux_archive())
        saved = proof.read_text()
        proof.unlink()
        with self.assertRaisesRegex(AssertionError, "provenance files"):
            release.checksums(self.directory, self.plugin, complete=True)
        data = json.loads(saved)
        data["library_sha256"] = "0" * 64
        proof.write_text(json.dumps(data))
        with self.assertRaisesRegex(AssertionError, "library digest"):
            release.checksums(self.directory, self.plugin, complete=True)

    def test_tagged_release_rejects_dirty_or_different_source(self):
        release.checksums(self.directory, self.plugin, complete=True, expected_commit="a" * 40)
        proof = release.provenance_path(self.linux_archive())
        data = json.loads(proof.read_text())
        with zipfile.ZipFile(self.linux_archive()) as archive:
            original = archive.read(archive.namelist()[0])
        for change in ({"source_dirty": True}, {"source_commit": "b" * 40}):
            with self.subTest(change=change):
                changed = dict(data, **change)
                library = original.replace(release.build_stamp("a" * 40, False), release.build_stamp(changed["source_commit"], changed["source_dirty"]))
                with zipfile.ZipFile(self.linux_archive(), "w") as archive:
                    archive.writestr(self.plugin["id"] + ".so", library)
                changed["library_sha256"] = hashlib.sha256(library).hexdigest()
                proof.write_text(json.dumps(changed))
                with self.assertRaisesRegex(AssertionError, "clean library built from the tagged source"):
                    release.checksums(self.directory, self.plugin, complete=True, expected_commit="a" * 40)

    def test_packaging_reads_the_binary_stamp_not_current_git_head(self):
        library = self.directory / "cursor-next.so"
        with zipfile.ZipFile(self.linux_archive()) as archive:
            image = archive.read(archive.namelist()[0])
        library.write_bytes(image.replace(release.build_stamp("a" * 40, False), release.build_stamp("c" * 40, True)))
        info = {"GoVersion": "go1.26.10", "Main": {"Path": self.plugin["repository"].removeprefix("https://")}, "Settings": [
            {"Key": "vcs.revision", "Value": "must-not-use-enclosing-repository"},
            {"Key": "GOOS", "Value": "linux"}, {"Key": "GOARCH", "Value": "amd64"},
        ]}
        with patch.object(release.subprocess, "check_output", return_value=json.dumps(info)) as read:
            proof = release.binary_provenance(library, self.plugin, "linux", "amd64")
        self.assertEqual(proof["source_commit"], "c" * 40)
        self.assertTrue(proof["source_dirty"])
        self.assertEqual(proof["library_sha256"], hashlib.sha256(library.read_bytes()).hexdigest())
        self.assertEqual(read.call_args.args[0], ["go", "version", "-m", "-json", str(library.resolve())])
        library.write_bytes(image.replace(release.build_stamp("a" * 40, False), b"\0" * len(release.build_stamp("a" * 40, False))))
        with patch.object(release.subprocess, "check_output", return_value=json.dumps(info)), \
                self.assertRaisesRegex(AssertionError, "no explicit source stamp"):
            release.binary_provenance(library, self.plugin, "linux", "amd64")

    def test_source_stamp_ignores_appended_text_and_rejects_missing_or_conflicting_image_data(self):
        original_stamp = release.build_stamp("a" * 40, False)
        other_stamp = release.build_stamp("b" * 40, True)
        for target in release.TARGETS:
            with self.subTest(target=target):
                with zipfile.ZipFile(self.directory / release.asset_name(self.plugin, *target)) as archive:
                    data = archive.read(archive.namelist()[0])
                self.assertEqual(release.library_source_stamp(data + other_stamp, *target), ("a" * 40, False))
                empty = data.replace(original_stamp, b"\0" * len(original_stamp))
                for candidate in (empty, empty + b"random trailer " + other_stamp):
                    with self.assertRaisesRegex(AssertionError, "no explicit source stamp"):
                        release.library_source_stamp(candidate, *target)
                conflicting = bytearray(data)
                conflicting[400:400 + len(other_stamp)] = other_stamp
                with self.assertRaisesRegex(AssertionError, "conflicting source stamps"):
                    release.library_source_stamp(conflicting, *target)
                with self.assertRaisesRegex(AssertionError, "truncated"):
                    release.library_source_stamp(data[:-1], *target)

    def test_legacy_five_zip_checksums_remain_readable(self):
        legacy = dict(self.plugin, version="0.2.0")
        directory = self.directory / "legacy"
        directory.mkdir()
        for target in release.TARGETS:
            original = self.directory / release.asset_name(self.plugin, *target)
            (directory / release.asset_name(legacy, *target)).write_bytes(original.read_bytes())
        self.assertEqual(len(release.checksums(directory, legacy, complete=True).splitlines()), 5)
        self.assertEqual(len(release.release_asset_names(legacy)), 6)
        self.assertEqual(len(release.release_asset_names(self.plugin)), 11)

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

    def test_store_submission_requires_new_version_provenance(self):
        assets = [{"name": release.asset_name(self.plugin, *target), "size": 256}
                  for target in release.TARGETS]
        assets.append({"name": "checksums.txt", "size": 256})
        with patch.object(submit.sys, "argv", ["submit-store-pr.py", "--submit"]), \
                patch.dict(submit.os.environ, {"GH_TOKEN": "synthetic-test-token"}), \
                patch.object(submit, "api", return_value={
                    "tag_name": "v" + self.plugin["version"], "draft": False,
                    "prerelease": False, "assets": assets,
                }) as api:
            with self.assertRaisesRegex(AssertionError, "missing release asset: .*provenance"):
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
        # The build entry has its own linked-worktree regression below. Stub
        # only that step and exercise the real Python/readelf glibc check in an
        # environment with no python3 on PATH.
        (self.directory / "scripts").mkdir()
        (self.directory / "scripts/release.py").write_text("raise SystemExit(0)\n")
        command = ["/bin/bash", str(ROOT / "scripts/build-linux.sh")]
        result = subprocess.run(command, env=env, cwd=self.directory, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("glibc <= 2.17", result.stdout)

        readelf.write_text("#!/bin/sh\nprintf 'Name: GLIBC_2.28\\n'\n")
        result = subprocess.run(command, env=env, cwd=self.directory, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("glibc baseline exceeded", result.stderr)


class BuildSourceTests(unittest.TestCase):
    def test_nested_linked_worktree_build_from_external_cwd_stamps_its_own_source(self):
        plugin = release.metadata()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
            def git(path, *args):
                return subprocess.check_output(["git", "-C", str(path), *args], env=env, text=True, stderr=subprocess.STDOUT).strip()
            def commit(path):
                git(path, "add", ".")
                git(path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                    "-c", "commit.gpgsign=false", "commit", "-qm", "synthetic build source")
                return git(path, "rev-parse", "HEAD")

            outer, source, caller = (root / name for name in ("outer", "source", "caller"))
            for path in (outer, source, caller):
                path.mkdir()
            git(outer, "init", "-q")
            (outer / "outer.txt").write_text("This is not the plugin source.\n")
            outer_commit = commit(outer)
            git(source, "init", "-q")
            (source / "scripts").mkdir()
            (source / "scripts/release.py").write_bytes((ROOT / "scripts/release.py").read_bytes())
            (source / "registry.json").write_text(json.dumps({"schema_version": 1, "plugins": [plugin]}))
            (source / "go.mod").write_text("module " + plugin["repository"].removeprefix("https://") + "\n\ngo 1.23\n")
            (source / "runtime.go").write_text('package main\nimport "C"\nconst version = "' + plugin["version"] + '"\nvar buildStamp = "unknown"\n//export fixtureBuildStamp\nfunc fixtureBuildStamp() *C.char { return C.CString(buildStamp) }\nfunc main() {}\n')
            expected = commit(source)
            self.assertNotEqual(expected, outer_commit)
            linked = outer / "linked"
            git(source, "worktree", "add", "--detach", str(linked), "HEAD")
            self.assertTrue((linked / ".git").is_file())
            goos, goarch = subprocess.check_output(["go", "env", "GOOS", "GOARCH"], text=True).splitlines()
            library = root / ("candidate." + release.TARGETS[goos, goarch][0])
            result = subprocess.run([sys.executable, str(linked / "scripts/release.py"), "build", "--library", str(library)],
                                    cwd=caller, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            proof = release.binary_provenance(library, plugin, goos, goarch)
            self.assertEqual(proof["source_commit"], expected)
            self.assertFalse(proof["source_dirty"])
            with (linked / "runtime.go").open("a") as changed:
                changed.write("// synthetic uncommitted change\n")
            with patch.object(release, "ROOT", linked):
                self.assertEqual(release.source_state(), (expected, True))


if __name__ == "__main__":
    unittest.main()
