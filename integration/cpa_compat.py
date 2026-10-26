#!/usr/bin/env python3
"""Exercise actual CPA C-ABI executor → usage recorder → Responses translation.

No Cursor network request or real credential is used. A synthetic ABI fixture
returns exact, unknown, zero, estimated and deferred usage. This test explicitly
records host limitations instead of treating a successful HTTP status as proof.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request


def http(base, key, path, body=None):
    return urllib.request.urlopen(urllib.request.Request(base + path,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"}), timeout=30)


def main(args):
    root = args.state_dir.resolve()
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    root.chmod(0o700)
    for name in ("tmp", "cache", "mod", "gopath", "home"):
        (root / name).mkdir(mode=0o700)
    repo = Path(__file__).resolve().parents[1]
    env = dict(os.environ, TMPDIR=str(root / "tmp"), GOCACHE=str(root / "cache"), GOMODCACHE=str(root / "mod"), GOPATH=str(root / "gopath"), GOTOOLCHAIN="local", GO111MODULE="off")
    suffix = ".dylib" if sys.platform == "darwin" else ".dll" if sys.platform == "win32" else ".so"
    library = root / ("cursor-next" + suffix)
    subprocess.run(["go", "build", "-trimpath", "-buildmode=c-shared", "-o", str(library), str(repo / "integration" / "fixture-plugin" / "main.go")], env=env, check=True)
    auth = root / "synthetic-auth.json"
    auth.write_text(json.dumps({"type": "cursor", "cursor_auth": {"type": "oauth", "access": "synthetic-unused", "refresh": "none", "expires": 9999999999999}}))
    auth.chmod(0o600)
    log = (root / "host.log").open("w")
    proc = subprocess.Popen([sys.executable, str(repo / "integration" / "start_isolated.py"),
        "--cpa-binary", str(args.cpa_binary.resolve()), "--plugin", str(library), "--auth-source", str(auth),
        "--state-dir", str(root / "host"), "--port", str(args.port)], stdout=log, stderr=subprocess.STDOUT, env=env)
    base = f"http://127.0.0.1:{args.port}"
    try:
        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError("isolated CPA exited; inspect private host.log")
            try:
                keys = json.loads((root / "host" / "keys.json").read_text())
                with http(base, keys["client"], "/v1/models") as response:
                    catalog = json.load(response)
                if len(catalog.get("data", [])) == 6:
                    break
            except (OSError, ValueError):
                pass
            time.sleep(.1)
        else:
            raise RuntimeError("isolated CPA did not become ready")
        report = []
        scenarios = args.scenario or ("known", "unknown", "zero", "estimated", "deferred", "reasoning-unknown")
        for scenario in scenarios:
            for stream in (False, True):
                payload = {"model": "cursor/compat-" + scenario, "input": "Synthetic host compatibility check.", "stream": stream}
                with http(base, keys["client"], "/v1/responses", payload) as response:
                    if not stream:
                        result = json.load(response)
                    else:
                        result = None
                        for line in response:
                            if line.startswith(b"data:") and line[5:].strip() != b"[DONE]":
                                event = json.loads(line[5:])
                                if event.get("type") == "response.completed":
                                    result = event["response"]
                        assert result is not None, "missing response.completed"
                (root / f"{scenario}-{'stream' if stream else 'nonstream'}.json").write_text(json.dumps(result, indent=2))
                usage = result.get("usage")
                if scenario == "unknown":
                    assert usage is None, "unknown usage was synthesized for client"
                elif scenario == "zero":
                    assert usage["input_tokens"] == usage["output_tokens"] == usage["total_tokens"] == 0
                else:
                    assert usage["input_tokens"] == 140260
                    assert usage["output_tokens"] == 669
                    assert usage["input_tokens_details"]["cached_tokens"] == 140160
                    expected_reasoning = 0 if scenario == "reasoning-unknown" else 200
                    assert usage["output_tokens_details"]["reasoning_tokens"] == expected_reasoning
                    assert "cache_write_tokens" not in usage["input_tokens_details"], "host behavior changed: reassess compatibility document"
                    assert "cursor_usage_estimated" not in usage and "cursor_deferred" not in usage
                report.append({"scenario": scenario, "stream": stream, "client_usage": usage})
        time.sleep(.2)
        with http(base, keys["management"], "/v8/management/observability/usage/queue?count=30") as response:
            records = json.load(response)
        assert len(records) == 2 * len(scenarios), f"expected {2 * len(scenarios)} attempts, got {len(records)}"
        for record in records:
            scenario = record["model"].removeprefix("cursor/compat-")
            assert not record["failed"]
            if scenario == "unknown":
                assert record["tokens"]["total_tokens"] == 0
                assert record["token_breakdown"]["quality"] == "complete", "host behavior changed: unknown no longer misclassified"
            elif scenario != "zero":
                assert record["token_breakdown"]["input"]["cache_write_tokens"] == 20
                assert record["token_breakdown"]["input"]["uncached_tokens"] == 80
                assert record["token_breakdown"]["output"]["reasoning_tokens"] == (0 if scenario == "reasoning-unknown" else 200)
                assert record["token_breakdown"]["quality"] == "complete"
        # The host queue contains the local client key. Keep only accounting.
        sanitized = [{k: record[k] for k in ("model", "stream", "tokens", "token_breakdown", "failed")} for record in records]
        result = {"responses": report, "records": sanitized,
            "limitations": ["missing usage becomes zero complete in CPA recorder", "cache-write detail omitted by Responses translator", "estimated and deferred extensions dropped", "unknown reasoning presence becomes explicit zero in CPA and Responses"]}
        (root / "result.json").write_text(json.dumps(result, indent=2))
        print(f"PASS: {2 * len(scenarios)} real-host synthetic cases; scenarios={','.join(scenarios)}")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
        log.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-binary", type=Path, required=True)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--port", type=int, default=28348)
    parser.add_argument("--scenario", action="append", choices=("known", "unknown", "zero", "estimated", "deferred", "reasoning-unknown"), help="run only selected scenario(s)")
    main(parser.parse_args())
