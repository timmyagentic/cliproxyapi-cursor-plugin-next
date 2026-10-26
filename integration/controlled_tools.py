#!/usr/bin/env python3
"""Bounded real-model test: parallel tools, their results, another tool, final answer.

Only synthetic data and allowlisted arithmetic tools are used. Run against an
isolated start_isolated.py instance. Never point this at a shared CPA port.
"""
import argparse
import json
from pathlib import Path
import time
import urllib.request
import uuid


def request(base, key, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    return urllib.request.urlopen(urllib.request.Request(base + path, data=data, headers={
        "Authorization": "Bearer " + key, "Content-Type": "application/json"}), timeout=180)


def run(args):
    root = args.state_dir.resolve()
    keys = json.loads((root / "keys.json").read_text())
    base = f"http://127.0.0.1:{args.port}"
    tools = [
        {"type": "function", "name": "sum_numbers", "description": "Compute exact integer sum.", "parameters": {"type": "object", "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}}, "required": ["a", "b"], "additionalProperties": False}},
        {"type": "function", "name": "describe_number", "description": "Return parity of an integer.", "parameters": {"type": "object", "properties": {"value": {"type": "integer"}}, "required": ["value"], "additionalProperties": False}},
        {"type": "function", "name": "verify_number", "description": "Check that an integer equals 42.", "parameters": {"type": "object", "properties": {"value": {"type": "integer"}}, "required": ["value"], "additionalProperties": False}},
    ]
    inputs = [{"role": "user", "content": "Follow this exact tool protocol. First call sum_numbers(a=17,b=25) and describe_number(value=42), both before seeing any result. After both actual results arrive, call verify_number(value=42) as a second round. After that actual result arrives, answer exactly VERIFIED 42 EVEN. Do not invent tool results. These tools are side-effect-free."}]
    session = "controlled-tools-" + uuid.uuid4().hex
    results = []
    for step in range(1, 5):
        payload = {"model": args.model, "input": inputs, "tools": tools, "stream": True,
                   "prompt_cache_key": session, "metadata": {"canonical_session_id": session}, "reasoning": {"effort": "high"}}
        start = time.monotonic()
        completed = None
        events = []
        with request(base, keys["client"], "/v1/responses", payload) as response:
            header = response.headers.get("X-Cursor-Usage-Policy", "")
            for line in response:
                if not line.startswith(b"data:"):
                    continue
                data = line[5:].strip()
                if data == b"[DONE]":
                    continue
                event = json.loads(data)
                events.append(event)
                if event.get("type") in ("response.failed", "error"):
                    raise RuntimeError("isolated model stream failed")
                if event.get("type") == "response.completed":
                    completed = event["response"]
        if completed is None:
            raise RuntimeError("stream did not contain response.completed")
        output = completed.get("output", [])
        calls = [x for x in output if x.get("type") == "function_call"]
        row = {"step": step, "seconds": round(time.monotonic() - start, 3), "tool_names": [x["name"] for x in calls], "usage": completed.get("usage"), "usage_header": header}
        journal_path = root / "journal" / "usage.json"
        if journal_path.exists():
            journal = json.loads(journal_path.read_text()).get("records", [])
            if journal:
                latest = journal[-1]
                row["journal"] = {key: latest.get(key) for key in ("status", "source", "internal_tool_calls", "usage")}
        results.append(row)
        (root / f"controlled-step-{step}.json").write_text(json.dumps({"request": payload, "events": events}, indent=2))
        print(json.dumps(row), flush=True)
        inputs.extend(output)
        if not calls:
            text = "".join(c.get("text", "") for x in output if x.get("type") == "message" for c in x.get("content", []))
            if text.strip() != "VERIFIED 42 EVEN":
                raise AssertionError("unexpected final answer in synthetic protocol")
            break
        for call in calls:
            values = json.loads(call["arguments"])
            if call["name"] == "sum_numbers":
                result = {"sum": int(values["a"]) + int(values["b"])}
            elif call["name"] == "describe_number":
                result = {"parity": "even" if int(values["value"]) % 2 == 0 else "odd"}
            elif call["name"] == "verify_number":
                result = {"verified": int(values["value"]) == 42}
            else:
                raise AssertionError("unregistered tool requested")
            inputs.append({"type": "function_call_output", "call_id": call["call_id"], "output": json.dumps(result)})
    else:
        raise AssertionError("bounded protocol exceeded four model requests")
    assert set(results[0]["tool_names"]) == {"sum_numbers", "describe_number"}, results
    assert results[1]["tool_names"] == ["verify_number"], results
    assert len(results) == 3 and results[2]["tool_names"] == [], results
    with request(base, keys["management"], "/v8/management/observability/usage/queue?count=30") as response:
        queue = json.load(response)
    # CPA's queue includes the local client key. Persist only measurement fields.
    fields = ("timestamp", "latency_ms", "ttft_ms", "tokens", "failed", "accounting_version", "token_breakdown", "provider", "model", "alias", "endpoint", "reasoning_effort", "service_tier")
    queue = [{key: row[key] for key in fields if key in row} for row in queue]
    (root / "controlled-usage-queue.json").write_text(json.dumps(queue, indent=2))
    (root / "controlled-summary.json").write_text(json.dumps(results, indent=2))
    print("PASS: real parallel tools → actual results → next tool → actual result → final answer", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--port", type=int, default=28347)
    parser.add_argument("--model", default="cursor/grok-4.7@256k")
    run(parser.parse_args())
