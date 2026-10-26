"""Prepare or submit the official store PR after the release is published."""
import argparse
import base64
import json
import os
from pathlib import Path
import re
import sys
import time
from release import TARGETS
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]
STORE = "router-for-me/CLIProxyAPI-Plugins-Store"


def api(path, token, data=None, method=None):
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "spx-cursor-release",
               "X-GitHub-Api-Version": "2022-11-28"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    raw = None if data is None else json.dumps(data).encode()
    if raw is not None:
        headers["Content-Type"] = "application/json"
    request = Request("https://api.github.com" + path, data=raw, headers=headers, method=method)
    with urlopen(request, timeout=30) as response:
        return json.load(response)


def append_entry(text, entry):
    registry = json.loads(text)
    assert registry["schema_version"] == 1
    assert not any(p["id"] == entry["id"] for p in registry["plugins"]), "plugin ID is already registered"
    assert not any(p["repository"] == entry["repository"] for p in registry["plugins"]), "repository is already registered"
    # Preserve the existing registry's formatting and append just our entry.
    end = text.rfind("]")
    before = text[:end].rstrip()
    encoded = "\n".join("    " + line for line in json.dumps(entry, indent=2).splitlines())
    updated = before + ("," if registry["plugins"] else "") + "\n" + encoded + "\n  " + text[end:]
    assert json.loads(updated)["plugins"][-1] == entry
    return updated


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--submit", action="store_true", help="fork the store and open the PR using GH_TOKEN")
    args = parser.parse_args()
    plugin = json.loads((ROOT / "registry.json").read_text())["plugins"][0]
    owner_repo = plugin["repository"].removeprefix("https://github.com/")
    tag = "v" + plugin["version"]
    names = [f'{plugin["id"]}_{plugin["version"]}_{goos}_{goarch}.zip'
             for goos, goarch in TARGETS]
    title = f'Add {plugin["name"]} provider'
    release_url = plugin["repository"] + "/releases/tag/" + tag
    body = f'''Adds **{plugin["name"]}** (`{plugin["id"]}`), a native Cursor provider for CPA.

Features:
- Browser OAuth login and API key login, including a plugin resource page for key validation and account creation.
- Streaming Chat Completions, images and caller tool calls.
- Subscription quota, reset times and on-demand spending reporting through CPA host HTTP callbacks.
- Cursor's official app icon, with source attribution in the repository.

Repository: {plugin["repository"]}
Latest release: [{tag}]({release_url})

Release assets (ZIP root contains `{plugin["id"]}.so`, `{plugin["id"]}.dylib` or `{plugin["id"]}.dll`):
''' + "".join(f"\n- `{name}`" for name in names + ["checksums.txt"]) + '''

Validation:
- The maintainer has completed self-testing on their CPA deployment.
- The release workflow runs Go race tests, go vet and C ABI smoke tests on each target runner.
- Release ZIP layouts, binary architectures and SHA256 checksums have been verified.

Linux libraries target glibc 2.17 or newer. No Node.js runtime is required.
Both login methods are provided by the plugin; no CPA source changes are required.

Supersedes the closed PR #277 by adding the missing Darwin/macOS AMD64 and
ARM64 binaries. The release covers all five required platform targets.
This PR changes only `registry.json`.
'''
    if not args.submit:
        print(title + "\n\n" + body + "\nRegistry entry:\n" + json.dumps(plugin, indent=2))
        return
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        raise ValueError("Set GH_TOKEN with access to the plugin repository and permission to fork/create a store PR")
    release = api(f"/repos/{owner_repo}/releases/latest", token)
    assert release["tag_name"] == tag and not release["draft"] and not release["prerelease"], "publish the matching stable release first"
    assets = {asset["name"]: asset for asset in release["assets"]}
    for name in names + ["checksums.txt"]:
        assert name in assets and assets[name]["size"] > 0, f"missing release asset: {name}"
    checksum_url = assets["checksums.txt"]["browser_download_url"]
    assert checksum_url.startswith(plugin["repository"] + "/releases/download/"), "unexpected checksum download origin"
    with urlopen(checksum_url, timeout=30) as response:
        checksums = response.read(65536).decode("ascii")
    entries = {}
    for line in checksums.splitlines():
        match = re.fullmatch(r"([a-fA-F0-9]{64})  (.+)", line)
        assert match and match[2] not in entries, "invalid or duplicate checksum entry"
        entries[match[2]] = match[1]
    assert set(entries) == set(names), "checksums must cover exactly the published platform ZIPs"

    user = api("/user", token)["login"]
    branch = f'add-spx-cursor-v{plugin["version"]}-full-platform'
    existing = api(f"/repos/{STORE}/pulls?" + urlencode({"head": f"{user}:{branch}", "state": "open"}), token)
    if existing:
        print(existing[0]["html_url"])
        return
    store = api(f"/repos/{STORE}", token)
    base = store["default_branch"]
    contents = api(f"/repos/{STORE}/contents/registry.json?" + urlencode({"ref": base}), token)
    updated = append_entry(base64.b64decode(contents["content"]).decode(), plugin)
    commit = api(f"/repos/{STORE}/git/ref/heads/{base}", token)["object"]["sha"]
    fork = api(f"/repos/{STORE}/forks", token, {"default_branch_only": True}, "POST")["full_name"]
    # GitHub forks are created asynchronously; wait for the default branch.
    for attempt in range(20):
        try:
            api(f"/repos/{fork}/git/ref/heads/{base}", token)
            break
        except HTTPError as error:
            if error.code != 404 or attempt == 19:
                raise
            time.sleep(3)
    # Never overwrite an existing branch or somebody else's changes.
    api(f"/repos/{fork}/git/refs", token, {"ref": "refs/heads/" + branch, "sha": commit}, "POST")
    api(f"/repos/{fork}/contents/registry.json", token, {
        "message": title, "branch": branch, "sha": contents["sha"],
        "content": base64.b64encode(updated.encode()).decode(),
    }, "PUT")
    pull = api(f"/repos/{STORE}/pulls", token, {
        "title": title, "body": body, "head": f"{user}:{branch}", "base": base,
    }, "POST")
    print(pull["html_url"])


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, ValueError, HTTPError, URLError) as error:
        sys.exit(f"Store PR not submitted: {error}")
