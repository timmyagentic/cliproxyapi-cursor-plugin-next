#!/usr/bin/env python3
"""Bounded synthetic Codex app-server -> CPA -> Cursor tool/accounting probe.

Uses an existing isolated CPA instance. Does not start, stop or reconfigure CPA.
Credentials are read from a private file into the child environment, never argv.
Only synthetic tool outputs and whitelisted accounting metadata are persisted.
"""
import argparse
import copy
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import queue
import re
import secrets
import subprocess
import threading
import time
import urllib.parse


def tools(large):
    specs = [
        {"name": "probe_left", "description": "Return the synthetic left operand and its receipt.",
         "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False}},
        {"name": "probe_right", "description": "Return the synthetic right operand and its receipt.",
         "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False}},
        {"name": "probe_difference", "description": "Compute right minus left from the two actual operand receipts; return the final verification marker.",
         "inputSchema": {"type": "object", "properties": {"left_receipt": {"type": "string"}, "right_receipt": {"type": "string"}}, "required": ["left_receipt", "right_receipt"], "additionalProperties": False}},
    ]
    if large:
        for i in range(29):
            # Large but purely synthetic tool metadata. No local/user schemas
            # or conversation history are copied into this public fixture.
            description = "Unused synthetic fixture option. " * 440
            specs.append({"name": f"unused_fixture_{i:02}", "description": "Not needed for the arithmetic probe. " + description,
                          "inputSchema": {"type": "object", "properties": {"fixture_option": {"type": "string", "description": description}}, "additionalProperties": False}})
    return [{"type": "function", "deferLoading": False, **spec} for spec in specs]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--codex", required=True, type=Path)
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--key-file", required=True, type=Path, help="private JSON containing client key")
    ap.add_argument("--model", default="cursor/grok-4.7@256k")
    ap.add_argument("--model-catalog", required=True, type=Path)
    ap.add_argument("--catalog-slug", required=True, help="existing catalog model; context settings are preserved")
    ap.add_argument("--output", required=True, type=Path, help="new private, isolated test directory")
    ap.add_argument("--large-tools", action="store_true")
    ap.add_argument("--reasoning-effort", choices=("low", "medium", "high"), default="high")
    ap.add_argument("--service-tier", choices=("auto", "priority"), help="Explicit isolated CPA payload policy; never changes the user's router")
    ap.add_argument("--timeout", type=int, default=360)
    args = ap.parse_args()
    base = urllib.parse.urlsplit(args.base_url)
    if base.scheme != "http" or base.hostname not in ("127.0.0.1", "localhost"):
        ap.error("base-url must be an explicitly isolated loopback CPA instance")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    root = args.output.resolve()
    for name in ("home", "work", "tmp"):
        (root/name).mkdir(mode=0o700)
    key = json.loads(args.key_file.read_text())["client"]
    catalog = json.loads(args.model_catalog.read_text())
    selected = next(m for m in catalog["models"] if m["slug"] == args.catalog_slug)
    selected = copy.deepcopy(selected)
    selected["slug"] = args.model
    selected["display_name"] = "Synthetic Cursor accounting probe"
    # Use the actual prior context and compaction settings without raising them.
    (root/"model-catalog.json").write_text(json.dumps({"models": [selected]}))
    requests, events, calls = [], [], []
    request_lock = threading.Lock()

    class Proxy(http.server.BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_POST(self):
            if self.path != "/v1/responses":
                self.send_error(404)
                return
            body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            payload = json.loads(body)
            if args.service_tier:
                payload["service_tier"] = args.service_tier
                body = json.dumps(payload, separators=(",", ":")).encode()
            entry = {"started": time.time(), "request_bytes": len(body), "tool_count": len(payload.get("tools", [])),
                     "tools_json_bytes": len(json.dumps(payload.get("tools", []), separators=(",", ":")).encode()), "model": payload.get("model"), "reasoning": payload.get("reasoning"), "service_tier": payload.get("service_tier")}
            with request_lock:
                if len(requests) >= 12:
                    self.send_error(429, "bounded probe request budget exhausted")
                    return
                requests.append(entry)
                (root/"request-metadata.json").write_text(json.dumps(requests, indent=2))
            connection = http.client.HTTPConnection(base.hostname, base.port, timeout=args.timeout)
            try:
                connection.request("POST", base.path.rstrip("/")+"/responses", body, {"Content-Type": "application/json", "Authorization": "Bearer "+key})
                response = connection.getresponse()
                entry["status"] = response.status
                self.send_response(response.status)
                self.send_header("Content-Type", response.getheader("Content-Type", "text/event-stream"))
                self.send_header("Connection", "close")
                self.end_headers()
                while True:
                    line = response.readline()
                    if not line:
                        break
                    if line.startswith(b"data:"):
                        try:
                            value = json.loads(line[5:])
                            if value.get("type") == "response.completed":
                                result = value.get("response", {})
                                entry["usage"] = result.get("usage")
                                entry["response_status"] = result.get("status")
                        except (ValueError, TypeError):
                            pass
                    self.wfile.write(line)
                    self.wfile.flush()
            except Exception as exc:
                entry["error_type"] = type(exc).__name__
            finally:
                entry["ended"] = time.time()
                connection.close()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    config = f'''model = {json.dumps(args.model)}
model_provider = "accounting-probe"
model_reasoning_effort = {json.dumps(args.reasoning_effort)}
model_catalog_json = {json.dumps(str(root/"model-catalog.json"))}
approval_policy = "never"
sandbox_mode = "read-only"
project_doc_max_bytes = 0
[model_providers.accounting-probe]
name = "Isolated accounting probe"
base_url = "http://127.0.0.1:{server.server_port}/v1"
wire_api = "responses"
env_key = "CURSOR_ACCOUNTING_PROBE_KEY"
request_max_retries = 0
stream_max_retries = 0
'''
    (root/"home/config.toml").write_text(config)
    env = os.environ.copy()
    for name in ("OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL", "CODEX_SQLITE_HOME"):
        env.pop(name, None)
    env.update(CODEX_HOME=str(root/"home"), TMPDIR=str(root/"tmp"), CURSOR_ACCOUNTING_PROBE_KEY=key)
    stderr = (root/"stderr.log").open("w")
    process = subprocess.Popen([str(args.codex), "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, text=True, env=env, cwd=root/"work")
    incoming = queue.Queue()

    def read():
        for line in process.stdout:
            try:
                incoming.put(json.loads(line))
            except ValueError:
                pass
        incoming.put({"probe_eof": True})

    threading.Thread(target=read, daemon=True).start()
    left, right = 23017, 78129
    left_receipt, right_receipt = secrets.token_hex(8), secrets.token_hex(8)
    marker = "VERIFIED_"+hashlib.sha256(f"{right-left}:{left_receipt}:{right_receipt}".encode()).hexdigest()[:20]

    def send(method=None, params=None, ident=None, result=None):
        msg = {"jsonrpc": "2.0", "id": ident} if ident is not None else {"jsonrpc": "2.0"}
        if method:
            msg.update(method=method, params=params)
        else:
            msg["result"] = result
        process.stdin.write(json.dumps(msg)+"\n")
        process.stdin.flush()

    def receive_until(predicate):
        deadline = time.monotonic()+args.timeout
        while time.monotonic() < deadline:
            event = incoming.get(timeout=max(.1, deadline-time.monotonic()))
            if event.get("probe_eof"):
                raise RuntimeError("app-server exited")
            method, params = event.get("method"), event.get("params", {})
            if method == "item/tool/call":
                name, arguments = params["tool"], params["arguments"]
                if isinstance(arguments, str):
                    arguments = json.loads(arguments)
                if len(calls) >= 8:
                    raise RuntimeError("bounded tool call budget exhausted")
                value, success = {}, True
                if name == "probe_left":
                    value = {"value": left, "receipt": left_receipt}
                elif name == "probe_right":
                    value = {"value": right, "receipt": right_receipt}
                elif name == "probe_difference" and arguments == {"left_receipt": left_receipt, "right_receipt": right_receipt}:
                    value = {"difference": right-left, "verification": marker}
                else:
                    value, success = {"error": "unexpected tool or invalid receipt"}, False
                calls.append({"tool": name, "success": success})
                send(ident=event["id"], result={"success": success, "contentItems": [{"type": "inputText", "text": json.dumps(value)}]})
            if method == "thread/tokenUsage/updated":
                events.append({"method": method, "tokenUsage": params["tokenUsage"]})
            elif method in ("item/started", "item/completed"):
                item = params.get("item", {})
                if item.get("type") in ("contextCompaction", "agentMessage", "commandExecution", "dynamicToolCall"):
                    events.append({"method": method, "item": {k: item[k] for k in ("type", "text", "status", "tool") if k in item}})
            elif method == "turn/completed":
                events.append({"method": method, "status": params.get("turn", {}).get("status")})
            if predicate(event):
                return event
        raise TimeoutError("bounded probe deadline")

    result = {"large_tools": args.large_tools, "configured_context_window": selected.get("context_window"), "configured_auto_compact_token_limit": selected.get("auto_compact_token_limit")}
    try:
        send("initialize", {"clientInfo": {"name": "cursor-accounting-probe", "version": "1"}, "capabilities": {"experimentalApi": True}}, 1)
        if "error" in receive_until(lambda e: e.get("id") == 1):
            raise RuntimeError("app-server initialization failed")
        send("initialized", {})
        send("thread/start", {"model": args.model, "modelProvider": "accounting-probe", "cwd": str(root/"work"), "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": True,
                              "baseInstructions": "You are executing a bounded synthetic arithmetic test. Use only probe_left, probe_right, and probe_difference. Do not use shell, browse, or any other tools. Follow the user's requested tool sequence exactly.", "dynamicTools": tools(args.large_tools)}, 2)
        start = receive_until(lambda e: e.get("id") == 2)
        if "error" in start:
            raise RuntimeError("thread start failed")
        tid = start["result"]["thread"]["id"]
        prompt = "Call probe_left and probe_right to get both receipts. Then call probe_difference with those exact receipts. Finally reply with only the verification marker from probe_difference. You must execute all three tools; the marker cannot be inferred."
        send("turn/start", {"threadId": tid, "input": [{"type": "text", "text": prompt, "text_elements": []}]}, 3)
        done = receive_until(lambda e: e.get("method") == "turn/completed")
        texts = [e["item"].get("text", "") for e in events if e.get("method") == "item/completed" and e.get("item", {}).get("type") == "agentMessage"]
        turn = done.get("params", {}).get("turn", {})
        result["completed"] = turn.get("status") == "completed"
        if turn.get("error"):
            detail = json.dumps(turn["error"]).replace(key, "[redacted]")
            detail = re.sub(r"(?i)(bearer\s+|token[=:]\s*|api[-_]?key[=:]\s*)[^\s,\"}]+", r"\1[redacted]", detail)
            result["turn_error"] = detail[:3000]
        result["verified_marker"] = any(text.strip() == marker for text in texts)
        result["compactions"] = sum(e.get("method") == "item/completed" and e.get("item", {}).get("type") == "contextCompaction" for e in events)
        result["tools"] = calls
        result["passed"] = result["completed"] and result["verified_marker"] and result["compactions"] == 0 and len(calls) >= 3 and all(c["success"] for c in calls)
    except Exception as exc:
        result.update(passed=False, error_type=type(exc).__name__)
    finally:
        process.terminate()
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        stderr.close()
        server.shutdown()
        server.server_close()
        result["requests"] = requests
        result["codex_events"] = events
        (root/"result.json").write_text(json.dumps(result, indent=2))
    print(json.dumps({k: v for k, v in result.items() if k not in ("requests", "codex_events")}, indent=2))
    raise SystemExit(0 if result.get("passed") else 1)


if __name__ == "__main__":
    main()
