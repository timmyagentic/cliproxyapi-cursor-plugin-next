"""Package CPA store ZIPs and verify their embedded-source provenance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import struct
import subprocess
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = {("linux", "amd64"): ("so", 62), ("linux", "arm64"): ("so", 183),
           ("darwin", "amd64"): ("dylib", 0x01000007),
           ("darwin", "arm64"): ("dylib", 0x0100000c),
           ("windows", "amd64"): ("dll", 0x8664)}


def metadata():
    registry = json.loads((ROOT / "registry.json").read_text())
    assert registry["schema_version"] == 1 and len(registry["plugins"]) == 1
    plugin = registry["plugins"][0]
    assert re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", plugin["id"])
    assert re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", plugin["version"])
    assert re.fullmatch(r"https://github.com/[^/]+/[^/]+", plugin["repository"])
    for field in ("name", "description", "author"):
        assert plugin[field]
    version = re.search(r'const version = "([^"]+)"', (ROOT / "runtime.go").read_text())
    assert version and version[1] == plugin["version"], "runtime and registry versions differ"
    return plugin


def check_binary(data, goos, goarch):
    _, machine = TARGETS[goos, goarch]
    if goos == "linux":
        assert data[:6] == b"\x7fELF\x02\x01", "expected a little-endian 64-bit ELF library"
        assert struct.unpack_from("<H", data, 16)[0] == 3, "expected an ELF shared library"
        assert struct.unpack_from("<H", data, 18)[0] == machine, "ELF architecture mismatch"
    elif goos == "darwin":
        assert data[:4] == b"\xcf\xfa\xed\xfe", "expected a little-endian 64-bit Mach-O library"
        assert struct.unpack_from("<I", data, 4)[0] == machine, "Mach-O architecture mismatch"
        assert struct.unpack_from("<I", data, 12)[0] == 6, "expected a Mach-O dynamic library"
    else:
        assert data[:2] == b"MZ", "expected a Windows DLL"
        offset = struct.unpack_from("<I", data, 0x3c)[0]
        assert data[offset:offset + 4] == b"PE\0\0", "invalid PE signature"
        assert struct.unpack_from("<H", data, offset + 4)[0] == machine, "PE architecture mismatch"
        assert struct.unpack_from("<H", data, offset + 22)[0] & 0x2000, "expected a DLL"


def asset_name(plugin, goos, goarch):
    return f'{plugin["id"]}_{plugin["version"]}_{goos}_{goarch}.zip'


def provenance_path(archive):
    return archive.with_suffix(".provenance.json")


def requires_provenance(plugin):
    return tuple(map(int, plugin["version"].split("."))) >= (0, 2, 2)


def release_asset_names(plugin):
    names = [asset_name(plugin, *target) for target in TARGETS]
    if requires_provenance(plugin):
        names += [provenance_path(Path(name)).name for name in names]
    return names + ["checksums.txt"]


def source_state():
    # Go 1.26's VCS finder skips linked-worktree .git files and can stamp an
    # enclosing repository. Git itself resolves them correctly with -C.
    env = dict(os.environ)
    for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
        env.pop(key, None)
    def git(*args):
        return subprocess.check_output(["git", "-C", str(ROOT), *args], env=env, text=True).strip()
    assert Path(git("rev-parse", "--show-toplevel")).resolve() == ROOT.resolve(), "build source must be the plugin's Git root"
    commit = git("rev-parse", "HEAD")
    assert re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", commit), "invalid source commit"
    return commit, bool(git("status", "--porcelain=v1", "--untracked-files=normal"))


def build_library(library, go="go"):
    commit, dirty = source_state()
    library = library.resolve()
    library.parent.mkdir(parents=True, exist_ok=True)
    flags = "-X main.buildStamp=" + build_stamp(commit, dirty).decode("ascii")
    subprocess.run([go, "build", "-buildvcs=false", "-trimpath", "-buildmode=c-shared",
                    "-ldflags", flags, "-o", str(library), "."], cwd=ROOT,
                   env=dict(os.environ, CGO_ENABLED="1", GOWORK="off"), check=True)


def build_stamp(commit, dirty):
    return f"CURSOR_NEXT_BUILD_V1|{commit}|{int(dirty)}|END".encode("ascii")


def mapped_ranges(data, goos):
    # Search bytes belonging to the loaded image, not arbitrary text appended
    # after an old library. The supported targets are all little-endian 64-bit.
    def number(fmt, offset):
        assert 0 <= offset <= len(data) - struct.calcsize(fmt), "truncated library header"
        return struct.unpack_from(fmt, data, offset)[0]
    ranges = []
    if goos == "linux":
        offset, size, count = number("<Q", 32), number("<H", 54), number("<H", 56)
        assert 0 < count <= 4096 and size >= 56 and offset + size * count <= len(data), "invalid ELF program headers"
        for index in range(count):
            at = offset + size * index
            if number("<I", at) == 1 and number("<I", at + 4) & 4:  # readable PT_LOAD
                ranges.append((number("<Q", at + 8), number("<Q", at + 32)))
    elif goos == "darwin":
        count, end, at = number("<I", 16), 32 + number("<I", 20), 32
        assert 0 < count <= 4096 and end <= len(data), "invalid Mach-O load commands"
        for _ in range(count):
            assert at + 8 <= end, "truncated Mach-O load command"
            command, size = number("<I", at), number("<I", at + 4)
            assert size >= 8 and at + size <= end, "invalid Mach-O load command size"
            if command == 0x19:  # LC_SEGMENT_64
                assert size >= 72, "invalid Mach-O segment"
                if number("<I", at + 60) & 1:  # VM_PROT_READ
                    ranges.append((number("<Q", at + 40), number("<Q", at + 48)))
            at += size
        assert at == end, "incomplete Mach-O load commands"
    else:
        pe = number("<I", 0x3c)
        count = number("<H", pe + 6)
        offset = pe + 24 + number("<H", pe + 20)
        assert 0 < count <= 4096 and offset + count * 40 <= len(data), "invalid PE sections"
        for index in range(count):
            at = offset + index * 40
            if number("<I", at + 36) & 0x40000000:  # IMAGE_SCN_MEM_READ
                ranges.append((number("<I", at + 20), number("<I", at + 16)))
    for offset, size in ranges:
        assert offset + size <= len(data), "mapped library data is truncated"
    return [(offset, size) for offset, size in ranges if size]


def library_source_stamp(data, goos, goarch):
    check_binary(data, goos, goarch)
    pattern = rb"CURSOR_NEXT_BUILD_V1\|([0-9a-f]{40}|[0-9a-f]{64})\|([01])\|END"
    found = {stamp for offset, size in mapped_ranges(data, goos)
             for stamp in re.findall(pattern, data[offset:offset + size])}
    assert found, "library has no explicit source stamp; rebuild with make build"
    assert len(found) == 1, "library contains conflicting source stamps"
    commit, dirty = found.pop()
    return commit.decode("ascii"), dirty == b"1"


def binary_provenance(library, plugin, goos, goarch):
    # Ask Go about the actual library, not the current checkout. Packaging an
    # older binary after switching branches must retain the binary's source.
    info = json.loads(subprocess.check_output(
        ["go", "version", "-m", "-json", str(library.resolve())], text=True))
    module = plugin["repository"].removeprefix("https://")
    assert info.get("Main", {}).get("Path") == module, "library module is not this plugin"
    settings = {entry["Key"]: entry["Value"] for entry in info.get("Settings", [])}
    data = library.read_bytes()
    commit, dirty = library_source_stamp(data, goos, goarch)
    assert settings.get("GOOS") == goos and settings.get("GOARCH") == goarch, "embedded build target mismatch"
    assert info.get("GoVersion"), "library has no compiler version"
    return {"schema_version": 1, "plugin_id": plugin["id"], "version": plugin["version"],
            "module_path": module, "source_commit": commit, "source_dirty": dirty, "go_version": info["GoVersion"],
            "goos": goos, "goarch": goarch, "library_sha256": hashlib.sha256(data).hexdigest()}


def verify_provenance(path, plugin, goos, goarch, library):
    data = json.loads(path.read_text())
    assert data.get("schema_version") == 1 and data.get("plugin_id") == plugin["id"] and data.get("version") == plugin["version"], "provenance metadata mismatch"
    assert data.get("module_path") == plugin["repository"].removeprefix("https://"), "provenance module mismatch"
    assert data.get("goos") == goos and data.get("goarch") == goarch, "provenance target mismatch"
    assert re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", data.get("source_commit", "")), "invalid provenance source commit"
    assert type(data.get("source_dirty")) is bool and isinstance(data.get("go_version"), str) and data["go_version"], "incomplete build provenance"
    assert data.get("library_sha256") == hashlib.sha256(library).hexdigest(), "provenance library digest mismatch"
    assert library_source_stamp(library, goos, goarch) == (data["source_commit"], data["source_dirty"]), "provenance source stamp mismatch"
    return data


def verify_archive(path, plugin, goos, goarch):
    ext, _ = TARGETS[goos, goarch]
    with zipfile.ZipFile(path) as archive:
        assert archive.namelist() == [f'{plugin["id"]}.{ext}'], "ZIP must contain only the root-level plugin library"
        assert archive.testzip() is None, "ZIP CRC check failed"
        library = archive.read(archive.namelist()[0])
        check_binary(library, goos, goarch)
        return library


def checksums(directory, plugin, complete=False, expected_commit=None):
    expected = {asset_name(plugin, *target): target for target in TARGETS}
    archives = sorted(directory.glob("*.zip"))
    assert archives, "no release ZIPs found"
    if complete:
        assert {p.name for p in archives} == set(expected), "release platform assets are missing or unexpected"
        if requires_provenance(plugin):
            assert {p.name for p in directory.glob("*.provenance.json")} == {provenance_path(p).name for p in archives}, "release provenance files are missing or unexpected"
    lines = []
    commits = set()
    for path in archives:
        assert path.name in expected, f"unexpected release asset: {path.name}"
        library = verify_archive(path, plugin, *expected[path.name])
        artifacts = [path]
        if requires_provenance(plugin):
            proof = provenance_path(path)
            data = verify_provenance(proof, plugin, *expected[path.name], library)
            commits.add(data["source_commit"])
            if expected_commit is not None:
                assert data["source_commit"] == expected_commit and not data["source_dirty"], "tag release requires a clean library built from the tagged source"
            artifacts.append(proof)
        for artifact in artifacts:
            lines.append(f"{hashlib.sha256(artifact.read_bytes()).hexdigest()}  {artifact.name}\n")
    assert len(commits) <= 1, "release libraries were built from different source commits"
    return "".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["build", "package", "checksums", "verify", "metadata"])
    parser.add_argument("--go", default="go", help="Go compiler executable for build")
    parser.add_argument("--goos")
    parser.add_argument("--goarch")
    parser.add_argument("--library", type=Path)
    parser.add_argument("--directory", type=Path, default=ROOT / "dist" / "release")
    parser.add_argument("--tag")
    args = parser.parse_args()
    plugin = metadata()
    if args.tag:
        assert args.tag == f'v{plugin["version"]}', "tag must match the runtime and registry version"
    if args.command == "metadata":
        print(json.dumps(plugin, indent=2))
        return
    if args.command == "build":
        assert args.library, "build requires --library"
        build_library(args.library, args.go)
        print(args.library.resolve())
        return
    directory = args.directory
    if args.command == "package":
        target = (args.goos, args.goarch)
        assert target in TARGETS and args.library, "provide a supported target and --library"
        data = args.library.read_bytes()
        check_binary(data, *target)
        provenance = binary_provenance(args.library, plugin, *target) if requires_provenance(plugin) else None
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / asset_name(plugin, *target)
        info = zipfile.ZipInfo(f'{plugin["id"]}.{TARGETS[target][0]}', (2026, 1, 1, 0, 0, 0))
        info.external_attr = 0o100755 << 16
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            archive.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED)
        verify_archive(path, plugin, *target)
        if provenance is not None:
            provenance_path(path).write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
            verify_provenance(provenance_path(path), plugin, *target, data)
        print(path)
        if provenance is not None:
            print(provenance_path(path))
        return
    expected_commit = None
    if args.tag:
        expected_commit = subprocess.check_output(["git", "rev-parse", "--verify", args.tag + "^{commit}"], cwd=ROOT, text=True).strip()
    content = checksums(directory, plugin, complete=True, expected_commit=expected_commit)
    checksum_file = directory / "checksums.txt"
    if args.command == "checksums":
        checksum_file.write_text(content, encoding="ascii")
    else:
        assert checksum_file.read_text(encoding="ascii") == content, "checksum list or archive digest mismatch"
    proof = ", provenance files" if requires_provenance(plugin) else " (legacy format)"
    print(f"Validated {len(TARGETS)} platform ZIPs{proof} and checksums.txt")


if __name__ == "__main__":
    main()
