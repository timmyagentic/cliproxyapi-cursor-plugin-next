"""Build and verify CPA store ZIPs using only the Python standard library."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import struct
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


def verify_archive(path, plugin, goos, goarch):
    ext, _ = TARGETS[goos, goarch]
    with zipfile.ZipFile(path) as archive:
        assert archive.namelist() == [f'{plugin["id"]}.{ext}'], "ZIP must contain only the root-level plugin library"
        assert archive.testzip() is None, "ZIP CRC check failed"
        check_binary(archive.read(archive.namelist()[0]), goos, goarch)


def checksums(directory, plugin, complete=False):
    expected = {asset_name(plugin, *target): target for target in TARGETS}
    archives = sorted(directory.glob("*.zip"))
    assert archives, "no release ZIPs found"
    if complete:
        assert {p.name for p in archives} == set(expected), "release platform assets are missing or unexpected"
    lines = []
    for path in archives:
        assert path.name in expected, f"unexpected release asset: {path.name}"
        verify_archive(path, plugin, *expected[path.name])
        lines.append(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n")
    return "".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["package", "checksums", "verify", "metadata"])
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
    directory = args.directory
    if args.command == "package":
        target = (args.goos, args.goarch)
        assert target in TARGETS and args.library, "provide a supported target and --library"
        data = args.library.read_bytes()
        check_binary(data, *target)
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / asset_name(plugin, *target)
        info = zipfile.ZipInfo(f'{plugin["id"]}.{TARGETS[target][0]}', (2026, 1, 1, 0, 0, 0))
        info.external_attr = 0o100755 << 16
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            archive.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED)
        verify_archive(path, plugin, *target)
        print(path)
        return
    content = checksums(directory, plugin, complete=True)
    checksum_file = directory / "checksums.txt"
    if args.command == "checksums":
        checksum_file.write_text(content, encoding="ascii")
    else:
        assert checksum_file.read_text(encoding="ascii") == content, "checksum list or archive digest mismatch"
    print(f"Validated {len(TARGETS)} platform ZIPs and checksums.txt")


if __name__ == "__main__":
    main()
