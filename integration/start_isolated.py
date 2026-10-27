#!/usr/bin/env python3
"""Start a loopback-only CPA with a copied plugin and one explicitly selected auth.

All state, copied credentials and random client/management keys stay in state-dir.
This never reads the running CPA's listener settings and never reloads it.
Requires PyYAML; no credential values are printed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import sys

import yaml


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-binary", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--auth-source", type=Path, required=True)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--proxy-config", type=Path)
    parser.add_argument("--transport", choices=("auto", "host", "native"), default="auto")
    parser.add_argument("--native-proxy-url", help="Explicit native transport proxy; never printed")
    parser.add_argument("--client-context-estimate", action="store_true", help="Opt in to supporting CPA hosts' client-only context estimates; never billed measurements")
    parser.add_argument("--port", type=int, default=28347)
    args = parser.parse_args()
    if args.transport == "native" and not args.native_proxy_url:
        parser.error("native transport requires an explicit --native-proxy-url")
    root = args.state_dir.resolve()
    if root.exists() and any(root.iterdir()):
        parser.error("state-dir must be empty; existing evidence is never overwritten")
    root.mkdir(parents=True, mode=0o700, exist_ok=True)
    root.chmod(0o700)
    for name in ("oauth", "plugins", "tmp", "home", "journal"):
        (root / name).mkdir(mode=0o700)
    with socket.socket() as check:
        check.bind(("127.0.0.1", args.port))
    auth = json.loads(args.auth_source.read_text())
    if auth.get("type") != "cursor" or auth.get("disabled"):
        parser.error("select one enabled Cursor credential")
    # Drop labels, account paths and other operational metadata from the copy.
    copied = {"type": "cursor", "cursor_auth": auth["cursor_auth"]}
    credential_path = root / "oauth" / "cursor-test.json"
    credential_path.write_text(json.dumps(copied))
    credential_path.chmod(0o600)
    shutil.copyfile(args.plugin, root / "plugins" / args.plugin.name)
    keys = {"client": secrets.token_urlsafe(32), "management": secrets.token_urlsafe(32)}
    (root / "keys.json").write_text(json.dumps(keys))
    (root / "keys.json").chmod(0o600)
    config = {
        "config-version": 8,
        "server": {"host": "127.0.0.1", "port": args.port, "discovery": {"enabled": False}},
        "management": {"allow-remote": False, "secret-key": keys["management"], "disable-control-panel": True, "disable-auto-update-panel": True},
        "access": {"api-keys": [keys["client"]]},
        "oauth": {"auth-dir": str(root / "oauth")},
        "plugins": {"enabled": True, "dir": str(root / "plugins"), "configs": {
            "cursor-next": {"enabled": True, "priority": 1, "usage-journal": str(root / "journal" / "usage.json"), "usage-wait-ms": 8000}}},
        "observability": {"logs": {"request-log": False, "logging-to-file": False}, "usage": {"usage-statistics-enabled": True, "redis-usage-queue-retention-seconds": 3600}},
        "requests": {"retry": 0},
    }
    if args.proxy_config:
        existing = yaml.safe_load(args.proxy_config.read_text())
        proxy = existing.get("requests", {}).get("proxy-url") or existing.get("proxy-url")
        if proxy:
            config["requests"]["proxy-url"] = proxy
    plugin_config = config["plugins"]["configs"]["cursor-next"]
    plugin_config["transport"] = args.transport
    plugin_config["client-context-estimate"] = args.client_context_estimate
    if args.native_proxy_url:
        plugin_config["native-proxy-url"] = args.native_proxy_url
    config_path = root / "config.yaml"
    config_path.write_text(yaml.safe_dump(config, sort_keys=False))
    config_path.chmod(0o600)
    # A comparison receipt contains identities and policy, never credential/proxy values.
    manifest = {"cpa_sha256": hashlib.sha256(args.cpa_binary.read_bytes()).hexdigest(),
                "plugin_sha256": hashlib.sha256(args.plugin.read_bytes()).hexdigest(),
                "credential_sha256": hashlib.sha256(json.dumps(copied, sort_keys=True).encode()).hexdigest(),
                "effective_proxy_sha256": hashlib.sha256((args.native_proxy_url if args.transport == "native" else config["requests"].get("proxy-url", "")).encode()).hexdigest(),
                "transport": args.transport, "native_proxy_configured": bool(args.native_proxy_url),
                "usage_wait_ms": plugin_config["usage-wait-ms"], "request_retry": 0}
    (root / "isolation.json").write_text(json.dumps(manifest, indent=2))
    env = dict(os.environ, HOME=str(root / "home"), TMPDIR=str(root / "tmp"), CURSOR_USAGE_JOURNAL=str(root / "journal" / "usage.json"))
    binary = str(args.cpa_binary.resolve())
    print(f"Isolated CPA: http://127.0.0.1:{args.port}; state-dir is private; credentials not printed", flush=True)
    os.chdir(root)
    os.execve(binary, [binary, "--local-model", "--config", str(config_path)], env)


if __name__ == "__main__":
    main()
