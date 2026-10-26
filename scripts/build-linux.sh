#!/usr/bin/env bash
set -euo pipefail

# Run inside the native-architecture manylinux2014 container with GOROOT mounted.
export PATH="/usr/local/go/bin:$PATH"
# manylinux installs Python under /opt/python, outside the default PATH.
BUILD_PYTHON="${BUILD_PYTHON:-/opt/python/cp312-cp312/bin/python3}"
if [[ ! -x "$BUILD_PYTHON" ]]; then
  echo "Build Python not found: $BUILD_PYTHON" >&2
  exit 1
fi

CGO_ENABLED=1 go build -buildvcs=false -trimpath -buildmode=c-shared \
  -o dist/cursor-next.so .

"$BUILD_PYTHON" - <<'PY'
import re
import subprocess

output = subprocess.check_output(
    ["readelf", "--version-info", "dist/cursor-next.so"], text=True
)
versions = [tuple(map(int, version)) for version in re.findall(r"GLIBC_(\d+)\.(\d+)", output)]
assert versions and max(versions) <= (2, 17), "glibc baseline exceeded"
print("Linux library requires glibc <= 2.17")
PY
