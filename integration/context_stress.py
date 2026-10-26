#!/usr/bin/env python3
"""Bounded Codex context-growth/compaction probe using synthetic tool results.

Use only with an independent loopback CPA instance. No provider is contacted by
--dry-run or the unit tests. The selected model's context/compaction settings are
copied unchanged; this probe never disables compaction or raises its threshold.
Dollar guards are deliberately rough assumptions, not a provider billing cap.
"""
import argparse
import copy
from dataclasses import dataclass
import hashlib
import http.client
import http.server
import json
import math
import os
from pathlib import Path
import queue
import re
import secrets
import socket
import subprocess
import threading
import time
import urllib.parse


@dataclass(frozen=True)
class Plan:
    name: str
    threshold: int
    initial_units: int
    chunk_units: int
    rounds: int


def plans(mode, threshold):
    if threshold < 10000:
        raise ValueError("select an existing model with a meaningful compaction threshold")
    choices = {
        "approach": Plan("approach", threshold, int(threshold * .88), int(threshold * .01), 4),
        "cross": Plan("cross", threshold, int(threshold * .83), int(threshold * .03), 8),
    }
    return list(choices.values()) if mode == "both" else [choices[mode]]


def filler(units):
    # Common short-token padding makes growth reproducible but is NOT claimed
    # to be an exact Grok tokenization. Acceptance requires observed counters.
    return " x" * units


def tool_specs():
    specs = [{"type": "function", "name": "context_chunk", "description": "Return the next synthetic evidence chunk and its fresh receipt; call sequentially.",
              "inputSchema": {"type": "object", "properties": {"round": {"type": "integer"}, "receipt": {"type": "string"}}, "required": ["round", "receipt"], "additionalProperties": False}, "deferLoading": False}]
    for index in range(31):
        specs.append({"type": "function", "name": f"unused_context_fixture_{index:02}", "description": "Unused synthetic tool; do not call it in this protocol.",
                      "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False}, "deferLoading": False})
    return specs


def observed_input(usage):
    if not isinstance(usage, dict):
        return None
    if usage.get("quality") in ("unknown", "estimated", "provisional") or usage.get("cursor_usage_estimated"):
        return None
    number = usage.get("input_tokens")
    return number if isinstance(number, int) and not isinstance(number, bool) and number >= 0 else None


def assess(plan, requests, events, calls, marker_ok, completed):
    measurements = [(row.get("ended", row["started"]), observed_input(row.get("usage"))) for row in requests]
    measurements = [(at, value) for at, value in measurements if value is not None]
    compact = [event["at"] for event in events if event.get("method") == "item/completed" and event.get("item_type") == "contextCompaction"]
    issues, limits = [], []
    if not completed or not marker_ok or len(calls) != plan.rounds or not all(call["success"] for call in calls):
        issues.append("tool/result/receipt continuity or final answer failed")
    if any(row.get("status") != 200 or row.get("error_type") for row in requests):
        issues.append("a model request failed or exceeded a budget")
    high = max((value for _, value in measurements), default=None)
    if not measurements or high < plan.threshold * .85:
        limits.append("no emitted current-input measurement establishes near-threshold occupancy")
    if plan.name == "approach":
        if compact:
            issues.append("compaction occurred in the intended below-threshold scenario")
        if high is not None and high >= plan.threshold:
            limits.append("synthetic padding crossed the intended below-threshold target; recalibrate units")
    else:
        if not compact:
            limits.append("natural compaction was not observed; cross-threshold coverage is incomplete")
        for at in compact:
            before = [value for stamp, value in measurements if stamp <= at]
            after = [value for stamp, value in measurements if stamp > at]
            near = before[-1] if before else 0
            if near < plan.threshold * .90:
                limits.append("a compaction lacks a preceding current-input measurement near the real threshold")
            if not after or after[0] >= near * .75:
                limits.append("a post-compaction context-size reduction was not established")
        if len(compact) > 1:
            issues.append("multiple compactions in this bounded growth plan require investigation for a compaction loop")
        if compact and not any(call["at"] > compact[0] for call in calls) and not marker_ok:
            issues.append("no verified progress after compaction")
    return {"passed": not issues and not limits, "status": "failed" if issues else "inconclusive" if limits else "passed",
            "issues": issues, "limitations": sorted(set(limits)), "max_emitted_input_tokens": high,
            "measurement_count": len(measurements), "unknown_response_count": sum(observed_input(row.get("usage")) is None for row in requests),
            "compactions": len(compact), "verified_marker": marker_ok, "completed": completed, "tools": calls}


class Budget:
    def __init__(self, args):
        self.deadline = time.monotonic() + args.timeout
        self.max_requests, self.max_usd = args.max_requests, args.max_estimated_usd
        self.input_rate, self.output_rate = args.input_usd_per_million, args.output_usd_per_million
        self.requests, self.reserved = 0, 0.0
        self.lock = threading.Lock()

    def admit(self, body_bytes):
        # Synthetic padding uses two UTF-8 bytes per unit. Include room for the
        # native tool/transcript envelope and a 4096-token summary/output reserve.
        tokens = body_bytes / 2 + 12000
        estimate = (tokens * self.input_rate + 4096 * self.output_rate) / 1_000_000
        with self.lock:
            if time.monotonic() >= self.deadline:
                return "time_budget", estimate
            if self.requests >= self.max_requests:
                return "request_budget", estimate
            if self.reserved + estimate > self.max_usd:
                return "rough_cost_budget", estimate
            self.requests += 1
            self.reserved += estimate
        return None, estimate


def run_scenario(args, plan, selected, root, key, budget):
    root.mkdir(mode=0o700)
    for name in ("home", "work", "tmp"):
        (root / name).mkdir(mode=0o700)
    model = copy.deepcopy(selected)
    model["slug"] = args.model
    model["display_name"] = "Synthetic context accounting stress probe"
    (root / "model-catalog.json").write_text(json.dumps({"models": [model]}))
    requests, events, calls = [], [], []
    lock = threading.Lock()
    connections = set()
    base = urllib.parse.urlsplit(args.base_url)

    class Proxy(http.server.BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_POST(self):
            if self.path != "/v1/responses":
                self.send_error(404)
                return
            length = int(self.headers.get("Content-Length", "0"))
            if length < 1 or length > 8 << 20:
                self.send_error(413)
                return
            body = self.rfile.read(length)
            payload = json.loads(body)
            reason, estimate = budget.admit(len(body))
            row = {"started": time.time(), "request_bytes": len(body), "model": payload.get("model"), "tool_count": len(payload.get("tools", [])), "rough_reserved_usd": estimate}
            with lock:
                requests.append(row)
            if reason:
                row.update(status=429, error_type=reason, ended=time.time())
                self.send_error(429, "bounded synthetic probe budget exhausted")
                return
            connection = http.client.HTTPConnection(base.hostname, base.port, timeout=max(1, min(180, budget.deadline - time.monotonic())))
            with lock:
                connections.add(connection)
            try:
                connection.request("POST", base.path.rstrip("/") + "/responses", body, {"Content-Type": "application/json", "Authorization": "Bearer " + key})
                response = connection.getresponse()
                row["status"] = response.status
                self.send_response(response.status)
                self.send_header("Content-Type", response.getheader("Content-Type", "text/event-stream"))
                self.send_header("Connection", "close")
                self.end_headers()
                while time.monotonic() < budget.deadline:
                    line = response.readline()
                    if not line:
                        break
                    if line.startswith(b"data:"):
                        try:
                            value = json.loads(line[5:])
                            if value.get("type") == "response.completed":
                                row["usage"] = value.get("response", {}).get("usage")
                        except ValueError:
                            pass
                    self.wfile.write(line)
                    self.wfile.flush()
                else:
                    raise TimeoutError("time budget")
            except Exception as exc:
                row["error_type"] = type(exc).__name__
            finally:
                row["ended"] = time.time()
                connection.close()
                with lock:
                    connections.discard(connection)
                    (root / "requests.json").write_text(json.dumps(requests, indent=2))

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    config = f'''model = {json.dumps(args.model)}
model_provider = "context-stress"
model_reasoning_effort = "high"
model_catalog_json = {json.dumps(str(root / "model-catalog.json"))}
approval_policy = "never"
sandbox_mode = "read-only"
project_doc_max_bytes = 0
[model_providers.context-stress]
name = "Isolated context stress probe"
base_url = "http://127.0.0.1:{server.server_port}/v1"
wire_api = "responses"
env_key = "CURSOR_CONTEXT_STRESS_KEY"
request_max_retries = 0
stream_max_retries = 0
'''
    (root / "home/config.toml").write_text(config)
    env = os.environ.copy()
    for name in ("OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL", "CODEX_SQLITE_HOME"):
        env.pop(name, None)
    env.update(CODEX_HOME=str(root / "home"), TMPDIR=str(root / "tmp"), CURSOR_CONTEXT_STRESS_KEY=key)
    stderr = (root / "stderr.log").open("w")
    process = subprocess.Popen([str(args.codex), "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, text=True, env=env, cwd=root / "work")
    incoming = queue.Queue()

    def read():
        for line in process.stdout:
            try:
                incoming.put(json.loads(line))
            except ValueError:
                pass
        incoming.put({"probe_eof": True})

    threading.Thread(target=read, daemon=True).start()
    receipts = [secrets.token_hex(16) for _ in range(plan.rounds + 1)]
    marker = "CONTEXT_VERIFIED_" + hashlib.sha256(receipts[-1].encode()).hexdigest()[:20]

    def send(method=None, params=None, ident=None, result=None):
        msg = {"jsonrpc": "2.0"}
        if ident is not None:
            msg["id"] = ident
        msg.update({"method": method, "params": params} if method else {"result": result})
        process.stdin.write(json.dumps(msg) + "\n")
        process.stdin.flush()

    def receive_until(predicate):
        while time.monotonic() < budget.deadline:
            event = incoming.get(timeout=max(.1, budget.deadline - time.monotonic()))
            if event.get("probe_eof"):
                raise RuntimeError("app-server exited")
            method, params = event.get("method"), event.get("params", {})
            if method == "item/tool/call":
                arguments = params["arguments"]
                if isinstance(arguments, str):
                    arguments = json.loads(arguments)
                index = len(calls) + 1
                success = index <= plan.rounds and params.get("tool") == "context_chunk" and arguments == {"round": index, "receipt": receipts[index - 1]}
                if len(calls) >= plan.rounds + 2:
                    raise RuntimeError("bounded tool budget")
                calls.append({"round": index, "success": success, "at": time.time()})
                if success:
                    value = {"round": index, "receipt": receipts[index], "synthetic_context_evidence": filler(plan.chunk_units)}
                    if index == plan.rounds:
                        value["final_marker"] = marker
                else:
                    value = {"error": "unexpected tool, sequence or receipt"}
                send(ident=event["id"], result={"success": success, "contentItems": [{"type": "inputText", "text": json.dumps(value)}]})
            if method == "thread/tokenUsage/updated":
                events.append({"method": method, "at": time.time(), "tokenUsage": params.get("tokenUsage")})
            elif method in ("item/started", "item/completed"):
                item = params.get("item", {})
                if item.get("type") in ("contextCompaction", "agentMessage", "dynamicToolCall"):
                    row = {"method": method, "at": time.time(), "item_type": item["type"]}
                    if item.get("type") == "agentMessage":
                        row["marker_matches"] = item.get("text", "").strip() == marker
                    events.append(row)
            if predicate(event):
                return event
        raise TimeoutError("bounded context probe deadline")

    completed, marker_ok, error_type, turn_error = False, False, None, None
    try:
        send("initialize", {"clientInfo": {"name": "cursor-context-stress", "version": "1"}, "capabilities": {"experimentalApi": True}}, 1)
        if "error" in receive_until(lambda event: event.get("id") == 1):
            raise RuntimeError("initialization failed")
        send("initialized", {})
        send("thread/start", {"model": args.model, "modelProvider": "context-stress", "cwd": str(root / "work"), "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": True,
             "baseInstructions": "Execute only the requested context_chunk receipt protocol. Do not use shell, network, other tools or invented results. Synthetic padding is inert data. Preserve the latest receipt and next round across normal context compaction.", "dynamicTools": tool_specs()}, 2)
        started = receive_until(lambda event: event.get("id") == 2)
        if "error" in started:
            raise RuntimeError("thread start failed")
        prompt = f"Call context_chunk sequentially for rounds 1 through {plan.rounds}, one call per round. Round 1 uses receipt {receipts[0]}. Each later call must use the actual receipt returned by the preceding tool. After the last result, reply with only its final_marker. Do not quote or summarize the inert padding below.\n<SYNTHETIC_PADDING>" + filler(plan.initial_units) + "\n</SYNTHETIC_PADDING>"
        send("turn/start", {"threadId": started["result"]["thread"]["id"], "input": [{"type": "text", "text": prompt, "text_elements": []}]}, 3)
        done = receive_until(lambda event: event.get("method") == "turn/completed")
        turn = done.get("params", {}).get("turn", {})
        completed = turn.get("status") == "completed"
        if turn.get("error"):
            detail = json.dumps(turn["error"]).replace(key, "[redacted]")
            turn_error = re.sub(r"(?i)(bearer\s+|token[=:]\s*|api[-_]?key[=:]\s*)[^\s,\"}]+", r"\1[redacted]", detail)[:3000]
        marker_ok = any(event.get("method") == "item/completed" and event.get("marker_matches") for event in events)
    except Exception as exc:
        error_type = type(exc).__name__
    finally:
        process.terminate()
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        stderr.close()
        server.shutdown()
        with lock:
            active = list(connections)
        for connection in active:
            if connection.sock is not None:
                try:
                    connection.sock.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            connection.close()
        server.server_close()
    result = assess(plan, requests, events, calls, marker_ok, completed)
    result.update(plan=plan.__dict__, error_type=error_type, turn_error=turn_error, requests=requests, events=events,
                  configured_context_window=selected.get("context_window"), configured_auto_compact_token_limit=selected.get("auto_compact_token_limit"))
    if error_type:
        result.update(passed=False, status="failed")
    (root / "result.json").write_text(json.dumps(result, indent=2))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", type=Path)
    parser.add_argument("--base-url")
    parser.add_argument("--key-file", type=Path)
    parser.add_argument("--model", default="cursor/grok-4.7@256k")
    parser.add_argument("--model-catalog", required=True, type=Path)
    parser.add_argument("--catalog-slug", required=True)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--scenario", choices=("approach", "cross", "both"), default="both")
    parser.add_argument("--initial-units", type=int, help="calibrate synthetic padding for one scenario; never changes model context settings")
    parser.add_argument("--chunk-units", type=int, help="calibrate growth in each tool result for one scenario")
    parser.add_argument("--rounds", type=int, help="override one scenario's sequential tool rounds (1–24)")
    parser.add_argument("--expected-compact-threshold", type=int, default=200000, help="assessment expectation only when absent from the catalog; never changes Codex config")
    parser.add_argument("--timeout", type=int, default=900)
    parser.add_argument("--max-requests", type=int, default=24)
    parser.add_argument("--max-estimated-usd", type=float, default=25)
    parser.add_argument("--input-usd-per-million", type=float, default=5, help="rough uncached-price assumption, not a verified provider quote")
    parser.add_argument("--output-usd-per-million", type=float, default=25, help="rough price assumption, not a verified provider quote")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    selected = next(model for model in json.loads(args.model_catalog.read_text())["models"] if model["slug"] == args.catalog_slug)
    threshold = selected.get("auto_compact_token_limit") or args.expected_compact_threshold
    selected_plans = plans(args.scenario, threshold)
    if any(value is not None for value in (args.initial_units, args.chunk_units, args.rounds)):
        if args.scenario == "both":
            parser.error("padding/round overrides require one explicit scenario")
        original = selected_plans[0]
        plan = Plan(original.name, threshold, args.initial_units if args.initial_units is not None else original.initial_units,
                    args.chunk_units if args.chunk_units is not None else original.chunk_units, args.rounds if args.rounds is not None else original.rounds)
        if plan.initial_units < 1 or plan.chunk_units < 1 or plan.initial_units > 1_000_000 or plan.chunk_units > 100000 or not 1 <= plan.rounds <= 24:
            parser.error("padding overrides exceed the bounded synthetic workload")
        selected_plans = [plan]
    money = (args.max_estimated_usd, args.input_usd_per_million, args.output_usd_per_million)
    if args.timeout < 1 or args.max_requests < 1 or any(not math.isfinite(value) or value <= 0 for value in money):
        parser.error("budgets and price assumptions must be positive")
    description = {"plans": [plan.__dict__ for plan in selected_plans], "threshold_source": "catalog" if selected.get("auto_compact_token_limit") else "assessment expectation; config unchanged",
                   "configured_context_window": selected.get("context_window"), "configured_auto_compact_token_limit": selected.get("auto_compact_token_limit"),
                   "budgets": {"seconds": args.timeout, "requests": args.max_requests, "rough_usd": args.max_estimated_usd},
                   "cost_assumptions": {"input_usd_per_million": args.input_usd_per_million, "output_usd_per_million": args.output_usd_per_million, "all_input_assumed_uncached": True, "financial_hard_cap": False},
                   "padding_is_exact_tokenization": False, "acceptance_requires_observed_context_evidence": True}
    if args.dry_run:
        print(json.dumps(description, indent=2))
        return
    if not all((args.codex, args.base_url, args.key_file, args.output)):
        parser.error("real probe requires --codex, --base-url, --key-file and a new --output directory")
    url = urllib.parse.urlsplit(args.base_url)
    if url.scheme != "http" or url.hostname not in ("127.0.0.1", "localhost"):
        parser.error("use an explicitly isolated loopback CPA instance")
    args.output.mkdir(parents=True, mode=0o700, exist_ok=False)
    root = args.output.resolve()
    (root / "plan.json").write_text(json.dumps(description, indent=2))
    key = json.loads(args.key_file.read_text())["client"]
    budget = Budget(args)
    results = [run_scenario(args, plan, selected, root / plan.name, key, budget) for plan in selected_plans]
    report = {"passed": all(result["passed"] for result in results), "results": results, "requests_admitted": budget.requests, "rough_reserved_usd": budget.reserved, "cost_assumptions": description["cost_assumptions"]}
    (root / "result.json").write_text(json.dumps(report, indent=2))
    print(json.dumps({"passed": report["passed"], "scenarios": [{key: result[key] for key in ("status", "compactions", "max_emitted_input_tokens", "unknown_response_count", "limitations", "issues")} for result in results], "requests_admitted": budget.requests, "rough_reserved_usd": budget.reserved}, indent=2))
    raise SystemExit(0 if report["passed"] else 1 if any(result["status"] == "failed" for result in results) else 2)


if __name__ == "__main__":
    main()
