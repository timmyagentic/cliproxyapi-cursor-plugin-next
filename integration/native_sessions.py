#!/usr/bin/env python3
"""Synthetic native-session/host-stateless A/B through isolated CPA listeners.

No provider is contacted by --dry-run or unit tests. A live run needs an already
started isolated CPA; this script never starts, installs or reloads a service.
Only receipt tools execute here. The 500K route is kept unchanged; the practical
stress case is about 100K synthetic padding units, NOT proof of filling 500K.
"""
import argparse
import copy
from dataclasses import dataclass
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import statistics
import sys
import time
import urllib.error
import urllib.request


STATE_ROOT = Path(os.environ.get("CURSOR_NATIVE_TEST_ROOT", Path(__file__).resolve().parents[1] / ".local-tests" / "native")).expanduser().resolve()
MODEL = "cursor/grok-4.7@500k"
SCHEMA_BYTES = 442107
TOOL_COUNT = 33
STEPS = ("receipt_seed", "synthetic_receipts.advance", "synthetic_receipts.advance", "receipt_seal")
RECEIPT_GRAMMAR = r'''start: "{" "\"step\"" ":" "4" "," "\"receipt\"" ":" RECEIPT "}"
RECEIPT: /"[0-9a-f]{40}"/
%import common.WS
%ignore WS'''
RECEIPT_GRAMMAR_PACKET = re.compile(r'\s*\{\s*"step"\s*:\s*4\s*,\s*"receipt"\s*:\s*"[0-9a-f]{40}"\s*\}\s*')
TOKEN_FIELDS = {"input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens",
                "cached_tokens", "cache_write_tokens", "reasoning_tokens", "input_tokens_details",
                "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"}
TELEMETRY_FIELDS = {"state", "record_id", "usage_scope", "continuity", "run_id", "cursor_request_id", "resumed", "transport"}


def sanitize_error(value):
    """Keep bounded diagnostic fields; never persist an echoed request/credential."""
    if isinstance(value, dict):
        selected = value.get("error") if isinstance(value.get("error"), dict) else value
        return {key: sanitize_error(selected[key]) for key in ("type", "code", "message", "reason") if key in selected}
    text = str(value)
    text = re.sub(r"https?://[^\s\"'<>]+", "[url omitted]", text)
    text = re.sub(r"(?i)Bearer\s+[^\s\"'<>]+", "Bearer [redacted]", text)
    text = re.sub(r"(?i)\b(?:cursor_|sk-|key_)[A-Za-z0-9._-]{12,}", "[credential redacted]", text)
    text = re.sub(r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?", "[token redacted]", text)
    text = re.sub(r"(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password|cookie|authorization)\s*[=:]\s*[^\s,;]+", r"\1=[redacted]", text)
    text = re.split(r'\{\s*"(?:model|input|messages|tools|headers|authorization)"', text, maxsplit=1)[0]
    return " ".join(text.split())[:700]


class StreamFailure(ValueError):
    def __init__(self, value):
        self.details = sanitize_error(value)
        super().__init__("upstream stream failure: " + json.dumps(self.details, ensure_ascii=False))


def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()


def digest(value):
    return hashlib.sha256(str(value).encode()).hexdigest()


def private_path(path):
    resolved = path.resolve()
    if resolved != STATE_ROOT and STATE_ROOT not in resolved.parents:
        raise ValueError("test state/output must stay under the dedicated native task directory")
    return resolved


def filtered(value, allowed):
    if not isinstance(value, dict):
        return None
    return {key: filtered(item, allowed) if isinstance(item, dict) else item
            for key, item in value.items() if key in allowed}


def make_tools(protocol, count=TOOL_COUNT, target_bytes=SCHEMA_BYTES, custom_grammar=False):
    parameters = {"type": "object", "properties": {"step": {"type": "integer"}, "receipt": {"type": "string"}},
                  "required": ["step", "receipt"], "additionalProperties": False}
    function = lambda name: {"type": "function", "name": name, "description": "Synthetic receipt only; no external side effects.", "parameters": parameters}
    if protocol == "responses":
        tools = [function("receipt_seed"), {"type": "namespace", "name": "synthetic_receipts", "description": "Synthetic receipt tools.",
                 "tools": [function("advance")]}, {"type": "custom", "name": "receipt_seal", "description": "Input is JSON with step and the actual previous receipt.", "format": {"type": "text"}}]
        if custom_grammar:
            tools[2]["format"] = {"type": "grammar", "syntax": "lark", "definition": RECEIPT_GRAMMAR}
    else:
        tools = [function("receipt_seed"), function("synthetic_receipts__advance"), function("receipt_seal")]
    tools.extend(function(f"unused_fixture_{i:02}") for i in range(count - len(tools)))
    if protocol == "chat":
        tools = [{"type": "function", "function": {key: value for key, value in tool.items() if key != "type"}} for tool in tools]
    description = tools[-1]["function"] if protocol == "chat" else tools[-1]
    remaining = target_bytes - len(encode(tools))
    if remaining < 0:
        raise ValueError("tool schema target is smaller than the synthetic tool definitions")
    sentence = " This synthetic tool describes stable receipt verification for a controlled protocol, without reading files or contacting external services. "
    description["description"] += (sentence * (remaining // len(sentence) + 1))[:remaining]
    assert len(encode(tools)) == target_bytes
    return tools


def canonical_name(call):
    name, namespace = call.get("name", ""), call.get("namespace", "")
    if namespace == "synthetic_receipts" and name == "advance":
        return "synthetic_receipts.advance"
    if name in ("synthetic_receipts.advance", "synthetic_receipts__advance"):
        return "synthetic_receipts.advance"
    return name


@dataclass
class ReceiptProtocol:
    nonce: str
    chunk_units: int = 0
    step: int = 0
    receipt: str = ""
    custom_grammar: bool = False

    def __post_init__(self):
        self.receipt = self.nonce

    def execute(self, call):
        if self.step >= len(STEPS) or canonical_name(call) != STEPS[self.step]:
            raise ValueError("unexpected tool or tool ordering")
        try:
            arguments = json.loads(call.get("arguments", call.get("input", "")))
        except (ValueError, TypeError):
            raise ValueError("tool arguments are not a JSON receipt packet") from None
        if arguments != {"step": self.step + 1, "receipt": self.receipt}:
            raise ValueError("tool did not use the actual previous receipt")
        if not isinstance(arguments.get("step"), int) or isinstance(arguments["step"], bool):
            raise ValueError("tool step is not an integer")
        if self.custom_grammar and self.step == 3 and not RECEIPT_GRAMMAR_PACKET.fullmatch(call.get("input", "")):
            raise ValueError("custom receipt text does not match the supplied Lark grammar language")
        self.step += 1
        self.receipt = secrets.token_hex(20)
        return {"step": self.step, "receipt": self.receipt, "evidence": " x" * self.chunk_units}

    def final_marker(self):
        return "VERIFIED " + self.receipt


class StreamResult:
    def __init__(self, protocol, started, clock=time.monotonic):
        self.protocol, self.started, self.clock = protocol, started, clock
        self.calls, self.chat_calls, self.done_items = [], {}, {}
        self.output_items = []
        self.text, self.usage, self.telemetry = "", None, None
        self.first_output, self.last_output, self.first_text, self.tool_ready, self.terminal = None, None, None, None, None
        self.done_count, self.completed_count, self.finish_reason = 0, 0, None
        self.output_events = 0
        self.failure = None

    def output(self, text, at, visible=False):
        if text:
            self.output_events += 1
            self.first_output = self.first_output if self.first_output is not None else at
            self.last_output = at
            if visible:
                self.first_text = self.first_text if self.first_text is not None else at

    def ingest(self, event, at):
        if event.get("error") or event.get("type") in ("error", "response.failed", "response.incomplete"):
            failure = event.get("response", {}).get("error") if isinstance(event.get("response"), dict) else None
            self.failure = sanitize_error(failure or event)
            raise StreamFailure(failure or event)
        if isinstance(event.get("usage"), dict):
            self.usage = filtered(event["usage"], TOKEN_FIELDS)
        if isinstance(event.get("cursor_usage"), dict):
            self.telemetry = filtered(event["cursor_usage"], TELEMETRY_FIELDS)
        if self.protocol == "chat":
            for choice in event.get("choices", []):
                delta = choice.get("delta", {})
                text = delta.get("content") or ""
                self.text += text
                self.output(text or delta.get("reasoning_content"), at, bool(text))
                for call in delta.get("tool_calls", []):
                    key = call.get("index", 0)
                    previous = self.chat_calls.setdefault(key, {"type": "function_call", "call_id": "", "name": "", "arguments": ""})
                    if call.get("id"):
                        previous["call_id"] = call["id"]
                    function = call.get("function", {})
                    previous["name"] += function.get("name", "")
                    previous["arguments"] += function.get("arguments", "")
                    self.output(function.get("arguments"), at)
                if choice.get("finish_reason"):
                    self.finish_reason = choice["finish_reason"]
                    self.terminal = at
                    if self.chat_calls:
                        self.tool_ready = at
            return
        kind = event.get("type", "")
        if kind.endswith(".delta"):
            self.output(event.get("delta"), at, kind == "response.output_text.delta")
            if kind == "response.output_text.delta":
                self.text += event.get("delta", "")
        if kind == "response.output_item.done":
            item = event.get("item", {})
            if item.get("type") in ("function_call", "custom_tool_call"):
                self.done_items[item.get("call_id", item.get("id"))] = item
                self.tool_ready = at
                if self.first_output is None:
                    self.output(item.get("arguments", item.get("input")), at)
        if kind == "response.completed":
            self.completed_count += 1
            if self.completed_count != 1:
                raise ValueError("duplicate response.completed")
            self.terminal = at
            response = event.get("response", {})
            self.output_items = copy.deepcopy(response.get("output", []))
            self.calls = [item for item in response.get("output", []) if item.get("type") in ("function_call", "custom_tool_call")]
            if not self.calls:
                self.calls = list(self.done_items.values())
            if self.calls and self.first_output is None:
                self.output(self.calls[0].get("arguments", self.calls[0].get("input")), at)
            if self.calls and self.tool_ready is None:
                self.tool_ready = at
            if not self.text:
                self.text = "".join(part.get("text", "") for item in response.get("output", []) if item.get("type") == "message" for part in item.get("content", []))
                self.output(self.text, at, bool(self.text))
            self.usage = filtered(response.get("usage"), TOKEN_FIELDS)
            self.telemetry = filtered(response.get("cursor_usage"), TELEMETRY_FIELDS) or self.telemetry

    def finish(self, ended):
        if self.protocol == "chat":
            if self.done_count != 1 or self.finish_reason is None:
                raise ValueError("Chat stream lacks exactly one DONE and a finish reason")
            self.calls = [self.chat_calls[key] for key in sorted(self.chat_calls)]
        elif self.completed_count != 1:
            raise ValueError("Responses stream lacks response.completed")
        if self.done_count > 1:
            raise ValueError("multiple DONE markers")
        ids = [call.get("call_id") for call in self.calls]
        if any(not value for value in ids) or len(set(ids)) != len(ids):
            raise ValueError("missing or duplicate tool call IDs")
        ms = lambda value: None if value is None else round((value - self.started) * 1000, 3)
        return {"ttft_ms": ms(self.first_output), "first_text_ms": ms(self.first_text), "last_output_ms": ms(self.last_output),
                "first_to_last_output_ms": None if self.first_output is None else round((self.last_output - self.first_output) * 1000, 3),
                "tool_ready_ms": ms(self.tool_ready), "terminal_ms": ms(self.terminal),
                "finalization_ms": None if self.last_output is None else round((self.terminal - self.last_output) * 1000, 3),
                "terminal_to_eof_ms": round((ended - self.terminal) * 1000, 3), "total_ms": ms(ended),
                "done_count": self.done_count, "usage": self.usage, "usage_present": self.usage is not None,
                "output_event_count": self.output_events, "output_span_measured": self.output_events >= 2,
                "cursor_usage": self.telemetry, "call_ids": ids,
                "tool_names": [canonical_name(call) for call in self.calls],
                "tool_shapes": [{key: call[key] for key in ("type", "name", "namespace") if key in call} for call in self.calls]}


def consume_sse(response, result):
    data = []
    for line in response:
        line = line.rstrip(b"\r\n")
        if line.startswith(b"data:"):
            value = line[5:].lstrip()
            if value.startswith(b"data:"):
                raise ValueError("double data: SSE framing")
            data.append(value)
        elif not line and data:
            value, data = b"\n".join(data), []
            if value == b"[DONE]":
                result.done_count += 1
                if result.done_count > 1:
                    raise ValueError("multiple DONE markers")
            else:
                try:
                    event = json.loads(value)
                except ValueError:
                    raise ValueError("invalid JSON SSE data") from None
                if not isinstance(event, dict):
                    raise ValueError("SSE data is not an event object")
                result.ingest(event, result.clock())
    if data:
        raise ValueError("unterminated SSE event")


def prompt(nonce, protocol, padding_units):
    advance = "synthetic_receipts.advance" if protocol == "responses" else "synthetic_receipts__advance"
    return ("Synthetic receipt protocol. Ignore the unrelated padding below. Execute exactly these sequential steps, waiting for each actual tool result: "
            f"1 receipt_seed, 2 {advance}, 3 {advance}, 4 receipt_seal. Each tool input must be JSON with exactly step (its integer number) and receipt (the actual previous result's receipt). "
            f"The initial receipt for step 1 is {nonce}. receipt_seal is a custom free-text tool on Responses: its input is the same JSON packet. "
            "Only after receiving the actual step-4 result, answer exactly VERIFIED followed by a space and its receipt. Never invent a result or call an unused tool.\nPADDING:" + " x" * padding_units)


def request_payload(protocol, history, tools, session):
    body = {"model": MODEL, "tools": tools, "stream": True, "service_tier": "priority",
            "prompt_cache_key": session, "metadata": {"canonical_session_id": session}}
    if protocol == "chat":
        body.update(messages=history, reasoning_effort="medium", stream_options={"include_usage": True})
    else:
        body.update(input=history, reasoning={"effort": "medium"})
    return body


def append_turn(protocol, history, result, tool_result=None):
    if protocol == "responses":
        # The completed output is the caller-visible transcript contract.
        # Dropping its message/reasoning items beside tool calls changes the
        # history already retained by the native Run and must not be hidden.
        if result.output_items:
            history.extend(copy.deepcopy(result.output_items))
        else:
            if result.text:
                history.append({"role": "assistant", "content": result.text})
            history.extend(copy.deepcopy(result.calls))
        if tool_result is not None:
            call = result.calls[0]
            history.append({"type": "custom_tool_call_output" if call["type"] == "custom_tool_call" else "function_call_output",
                            "call_id": call["call_id"], "output": json.dumps(tool_result)})
    else:
        history.append({"role": "assistant", "content": result.text or None,
                        **({"tool_calls": [{"id": call["call_id"], "type": "function", "function": {"name": call["name"], "arguments": call["arguments"]}} for call in result.calls]} if result.calls else {})})
        if tool_result is not None:
            history.append({"role": "tool", "tool_call_id": result.calls[0]["call_id"], "content": json.dumps(tool_result)})


def journal_snapshot(state, start, end):
    path = state / "journal" / "usage.json"
    latest, diagnostics, sources = {}, [], []
    def accept(record, sequence=None):
        if not isinstance(record, dict) or not isinstance(record.get("id"), str):
            return
        seq = record.get("sequence", 0) if sequence is None else sequence
        if not isinstance(seq, int) or isinstance(seq, bool) or seq < 0:
            diagnostics.append("invalid_record_sequence")
            return
        prior = latest.get(record["id"])
        if prior is None or seq >= prior[0]:
            latest[record["id"]] = (seq, record)
    def checkpoint(source):
        if not source.exists() or source.is_symlink():
            return
        try:
            raw = source.read_bytes()
            if len(raw) > 16 * 1024 * 1024:
                diagnostics.append("checkpoint_size_limit")
                return
            snapshot = json.loads(raw)
            records = snapshot.get("records") if isinstance(snapshot, dict) else None
            if records is not None and not isinstance(records, list):
                diagnostics.append("invalid_checkpoint_records")
                return
            for record in records or []:
                accept(record)
            sources.append(source.name)
        except (OSError, ValueError, TypeError):
            diagnostics.append("checkpoint_unavailable")
    checkpoint(path)
    segment_dir = path.with_name(path.name + ".segments")
    if segment_dir.is_dir() and not segment_dir.is_symlink():
        checkpoint(segment_dir / "import-v1.json")
        segments = sorted(file for file in segment_dir.glob("events-*.jsonl") if re.fullmatch(r"events-\d+\.jsonl", file.name))
        if len(segments) > 256:
            diagnostics.append("segment_count_limit")
        for source in segments[:256]:
            if source.is_symlink() or not source.is_file():
                diagnostics.append("unsafe_segment_skipped")
                continue
            try:
                # Freeze the observed byte length. Concurrent appends cannot make
                # this reader follow the writer or wait for a future delimiter.
                size = source.stat().st_size
                if size > 16 * 1024 * 1024:
                    diagnostics.append("segment_size_limit")
                    continue
                with source.open("rb") as file:
                    raw = file.read(size)
                sources.append(source.name)
                if raw and not raw.endswith(b"\n"):
                    diagnostics.append("partial_append_tail_ignored")
                    raw = raw.rsplit(b"\n", 1)[0] if b"\n" in raw else b""
                for line in raw.splitlines():
                    try:
                        frame = json.loads(line)
                        record, sequence = frame.get("record"), frame.get("sequence")
                        if not isinstance(record, dict) or sequence != record.get("sequence"):
                            diagnostics.append("invalid_segment_frame")
                            continue
                        accept(record, sequence)
                    except (ValueError, TypeError, AttributeError):
                        diagnostics.append("invalid_segment_json")
            except OSError:
                diagnostics.append("segment_unavailable")
    selected = []
    for _, record in latest.values():
        try:
            at = datetime.datetime.fromisoformat(record["started"].replace("Z", "+00:00")).timestamp()
        except (ValueError, KeyError, TypeError):
            continue
        if start - 1 <= at <= end + 1:
            row = {key: record[key] for key in ("status", "source", "internal_tool_calls", "internal_steps", "usage", "reason", "transport", "continuity", "sequence") if key in record}
            for key in ("id", "run_id", "conversation_id", "event_id"):
                if record.get(key):
                    row[key + "_hash"] = digest(record[key])
            selected.append(row)
    return {"present": bool(sources), "records": selected, "sources": sources, "diagnostics": sorted(set(diagnostics)),
            "snapshot_kind": "point-in-time checkpoint plus complete append frames; latest sequence per record", "checksum_verified": False}


def continuity(call_ids):
    matches = [re.fullmatch(r"(call_cursor_.+)_(\d+)_(\d+)", value) for value in call_ids]
    prefixes = {match[1] for match in matches if match}
    return {"all_native_call_ids": bool(matches) and all(matches), "native_session_prefix_count": len(prefixes),
            "native_session_prefix_hashes": sorted(digest(value) for value in prefixes),
            "same_native_session": bool(matches) and all(matches) and len(prefixes) == 1}


def run_protocol(args, protocol, key, root):
    custom_grammar = protocol == "responses" and getattr(args, "custom_grammar", False)
    tools = make_tools(protocol, args.tool_count, args.tool_schema_bytes, custom_grammar)
    (root / f"{protocol}-tools.json").write_bytes(encode(tools))
    nonce = secrets.token_hex(20)
    receipt = ReceiptProtocol(nonce, args.chunk_units, custom_grammar=custom_grammar)
    history = [{"role": "user", "content": prompt(nonce, protocol, args.padding_units)}]
    session = "native-ab-" + secrets.token_hex(16)
    rows, call_ids, issues = [], [], []
    started_wall, started = time.time(), time.monotonic()
    previous_tool_ready = None
    try:
        for index in range(6):
            if index >= args.max_requests or time.monotonic() - started >= args.timeout:
                raise ValueError("bounded request/time budget exhausted")
            body = encode(request_payload(protocol, history, tools, session))
            if len(body) > args.max_body_bytes:
                raise ValueError("synthetic request exceeded the explicit byte budget")
            at = time.monotonic()
            result = StreamResult(protocol, at)
            row = {"step": index + 1, "started_wall": time.time(), "request_bytes": len(body),
                   "request_sha256": hashlib.sha256(body).hexdigest(), "status": None}
            rows.append(row)
            (root / f"{protocol}-requests.json").write_text(json.dumps(rows, indent=2))
            request = urllib.request.Request(f"http://127.0.0.1:{args.port}/v1/" + ("chat/completions" if protocol == "chat" else "responses"), data=body,
                      headers={"Authorization": "Bearer " + key, "Content-Type": "application/json", "session_id": session})
            with urllib.request.urlopen(request, timeout=min(args.request_timeout, args.timeout - (at - started))) as response:
                row["status"] = getattr(response, "status", 200)
                header_ms = round((time.monotonic() - at) * 1000, 3)
                headers = {name: response.headers.get(name) for name in ("X-Cursor-Continuity", "X-Cursor-Run-Id", "X-Cursor-Usage-Policy", "X-Cursor-Source-Format") if response.headers.get(name)}
                row.update(headers_ms=header_ms, response_headers=headers)
                consume_sse(response, result)
            ended = time.monotonic()
            row.update(result.finish(ended))
            row.update(step=index + 1, started_wall=time.time() - (ended - at), request_bytes=len(body), request_sha256=hashlib.sha256(body).hexdigest(), response_headers=headers,
                       headers_ms=header_ms, tool_handoff_client_ms=None if previous_tool_ready is None else round((at - previous_tool_ready) * 1000, 3),
                       tool_handoff_to_output_ms=None if previous_tool_ready is None or result.first_output is None else round((result.first_output - previous_tool_ready) * 1000, 3))
            (root / f"{protocol}-requests.json").write_text(json.dumps(rows, indent=2))
            if len(result.calls) > 1:
                raise ValueError("receipt protocol requires exactly one sequential tool per boundary")
            if result.calls:
                call = result.calls[0]
                if call["call_id"] in call_ids:
                    raise ValueError("tool call ID was reused across distinct tool executions")
                call_ids.append(call["call_id"])
                if protocol == "responses" and canonical_name(call) == "receipt_seal" and call["type"] != "custom_tool_call":
                    raise ValueError("Responses custom tool shape was lost")
                if protocol == "responses" and canonical_name(call) == "synthetic_receipts.advance" and call.get("namespace") != "synthetic_receipts":
                    raise ValueError("Responses namespace was not restored")
                output = receipt.execute(call)
                if custom_grammar and receipt.step == 4:
                    row["custom_grammar_packet_validated"] = True
                previous_tool_ready = result.tool_ready
                append_turn(protocol, history, result, output)
            else:
                if receipt.step != 4:
                    raise ValueError("model answered before all actual receipts were consumed")
                if index == 4:
                    if result.text.strip() != receipt.final_marker():
                        raise ValueError("final answer did not contain the actual final receipt")
                    append_turn(protocol, history, result)
                    history.append({"role": "user", "content": "Ordinary follow-up on the same session: copy the exact preceding VERIFIED receipt line, with no tools and no extra text."})
                    previous_tool_ready = None
                elif index == 5 and result.text.strip() == receipt.final_marker():
                    break
                else:
                    raise ValueError("ordinary follow-up did not preserve the actual receipt")
        else:
            raise ValueError("six-request protocol did not finish")
    except (ValueError, OSError, TimeoutError, urllib.error.URLError) as error:
        issues.append(str(error) if isinstance(error, ValueError) else type(error).__name__)
        if rows:
            rows[-1].setdefault("total_ms", round((time.monotonic() - at) * 1000, 3))
            rows[-1]["error_type"] = type(error).__name__
            if isinstance(error, urllib.error.HTTPError):
                rows[-1]["status"] = error.code
                try:
                    rows[-1]["http_error"] = sanitize_error(json.loads(error.read(65536)))
                except (OSError, ValueError):
                    rows[-1]["http_error"] = {"message": "non-JSON HTTP error body omitted"}
            if isinstance(error, StreamFailure):
                rows[-1]["stream_error"] = error.details
            rows[-1]["output_events_before_failure"] = result.output_events
    ended_wall = time.time()
    proof = continuity(call_ids)
    if args.arm == "native" and not proof["same_native_session"]:
        issues.append("native call IDs do not establish one continued session")
    (root / f"{protocol}-requests.json").write_text(json.dumps(rows, indent=2))
    (root / f"{protocol}-issues.json").write_text(json.dumps(issues, indent=2))
    return {"protocol": protocol, "functional_passed": not issues, "issues": issues, "requests": rows,
            "verified_tools": receipt.step, "ordinary_followup_verified": len(rows) == 6 and not issues,
            "continuity": proof, "started_wall": started_wall, "ended_wall": ended_wall,
            "total_ms": round((time.monotonic() - started) * 1000, 3),
            "journal": journal_snapshot(args.state_dir, started_wall, ended_wall)}


def dashboard_evidence(report, fixture):
    if fixture is None:
        return {"present": False, "billing_events": None, "reason": "separate Dashboard metadata not supplied"}
    events = fixture.get("rows", fixture.get("usageEventsDisplay", [])) if isinstance(fixture, dict) else fixture
    if not isinstance(events, list):
        return {"present": True, "billing_events": None, "reason": "unsupported metadata fixture"}
    start = min(row["started_wall"] for row in report["protocols"]) * 1000 - 1000
    end = max(row["ended_wall"] for row in report["protocols"]) * 1000 + 1000
    owners = {row["conversation_id_hash"] for protocol in report["protocols"] for row in protocol["journal"]["records"] if row.get("conversation_id_hash")}
    matching, candidates = {}, 0
    for event in events:
        at = event.get("timestamp_ms", event.get("timestamp"))
        try:
            at = float(at)
        except (ValueError, TypeError):
            continue
        if not start <= at <= end:
            continue
        candidates += 1
        owner = event.get("conversation_hash")
        if event.get("conversationId") or event.get("conversation_id"):
            owner = digest(event.get("conversationId", event.get("conversation_id")))
        owned = isinstance(owner, str) and len(owner) >= 16 and sum(value.startswith(owner) for value in owners) == 1
        identity = event.get("usageEventId", event.get("event_hash", event.get("event_id")))
        if owned and identity:
            matching[digest(identity)] = event
    return {"present": True, "billing_events": len(matching) if matching else None, "billing_events_observed": len(matching), "window_candidates": candidates,
            "measured_events": sum(isinstance(row.get("tokenUsage"), dict) or row.get("tokens_present") is True for row in matching.values()),
            "attribution": "conversation identity plus bounded time window" if owners else "unverified: journal conversation identities unavailable",
            "stability": "point-in-time Dashboard observation; not a finality claim"}


def compare(native, stateless, dashboard_native=None, dashboard_stateless=None):
    keys = ("scenario", "model", "service_tier", "reasoning_effort", "context_window", "auto_compact_token_limit", "tool_count", "tool_schema_bytes", "padding_units", "chunk_units", "custom_grammar", "custom_grammar_sha256")
    mismatches = [key for key in keys if native.get("conditions", {}).get(key) != stateless.get("conditions", {}).get(key)]
    for key in ("cpa_sha256", "plugin_sha256", "credential_sha256", "effective_proxy_sha256", "usage_wait_ms", "request_retry"):
        if native.get("isolation", {}).get(key) is None or native.get("isolation", {}).get(key) != stateless.get("isolation", {}).get(key):
            mismatches.append(key)
    comparisons = []
    for left in native.get("protocols", []):
        right = next((row for row in stateless.get("protocols", []) if row["protocol"] == left["protocol"]), None)
        if right is None:
            mismatches.append("missing protocol " + left["protocol"])
            continue
        median = lambda report, key: statistics.median([row[key] for row in report["requests"] if row.get(key) is not None]) if any(row.get(key) is not None for row in report["requests"]) else None
        comparisons.append({"protocol": left["protocol"], "native_passed": left["functional_passed"], "stateless_passed": right["functional_passed"],
             "native_http_requests": len(left["requests"]), "stateless_http_requests": len(right["requests"]),
             "native_journal_records": len(left["journal"]["records"]) if left["journal"]["present"] else None,
             "stateless_journal_records": len(right["journal"]["records"]) if right["journal"]["present"] else None,
             "native_median_ttft_ms": median(left, "ttft_ms"), "stateless_median_ttft_ms": median(right, "ttft_ms"),
             "native_median_tool_handoff_client_ms": median(left, "tool_handoff_client_ms"), "stateless_median_tool_handoff_client_ms": median(right, "tool_handoff_client_ms"),
             "native_median_tool_handoff_to_output_ms": median(left, "tool_handoff_to_output_ms"), "stateless_median_tool_handoff_to_output_ms": median(right, "tool_handoff_to_output_ms"),
             "native_median_finalization_ms": median(left, "finalization_ms"), "stateless_median_finalization_ms": median(right, "finalization_ms"),
             "native_unique_journal_run_ids": len({row["run_id_hash"] for row in left["journal"]["records"] if row.get("run_id_hash")}) if left["journal"]["present"] else None,
             "stateless_unique_journal_run_ids": len({row["run_id_hash"] for row in right["journal"]["records"] if row.get("run_id_hash")}) if right["journal"]["present"] else None,
             "native_total_ms": left["total_ms"], "stateless_total_ms": right["total_ms"]})
    return {"comparable": not mismatches, "condition_mismatches": mismatches, "protocols": comparisons,
            "dashboard_native": dashboard_evidence(native, dashboard_native), "dashboard_stateless": dashboard_evidence(stateless, dashboard_stateless),
            "limitations": ["whole-path comparison changes transport AND continuation", "journal records are not independent Dashboard billing events", "relay timing is not a direct native decoder measurement"]}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    run = sub.add_parser("run")
    run.add_argument("--arm", choices=("native", "stateless"), required=True)
    run.add_argument("--state-dir", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--port", type=int, required=True)
    run.add_argument("--protocol", choices=("chat", "responses", "both"), default="both")
    run.add_argument("--scenario", choices=("smoke", "tools", "stress"), default="tools")
    run.add_argument("--tool-count", type=int)
    run.add_argument("--tool-schema-bytes", type=int)
    run.add_argument("--padding-units", type=int)
    run.add_argument("--chunk-units", type=int)
    run.add_argument("--max-requests", type=int, default=6, help="per protocol; six are required for the full protocol")
    run.add_argument("--timeout", type=float, default=1200, help="per-protocol wall-clock budget")
    run.add_argument("--request-timeout", type=float, default=240)
    run.add_argument("--max-body-bytes", type=int, default=2 * 1024 * 1024)
    run.add_argument("--dry-run", action="store_true")
    run.add_argument("--custom-grammar", action="store_true", default=None, help="send a real Lark custom-tool format; defaults on for non-smoke Responses")
    comparison = sub.add_parser("compare")
    comparison.add_argument("--native", type=Path, required=True)
    comparison.add_argument("--stateless", type=Path, required=True)
    comparison.add_argument("--dashboard-native", type=Path)
    comparison.add_argument("--dashboard-stateless", type=Path)
    args = parser.parse_args(argv)
    if args.command == "compare":
        result = compare(json.loads(args.native.read_text()), json.loads(args.stateless.read_text()),
                         json.loads(args.dashboard_native.read_text()) if args.dashboard_native else None,
                         json.loads(args.dashboard_stateless.read_text()) if args.dashboard_stateless else None)
        print(json.dumps(result, indent=2))
        return 0 if result["comparable"] else 2
    args.tool_count = args.tool_count if args.tool_count is not None else (3 if args.scenario == "smoke" else TOOL_COUNT)
    args.tool_schema_bytes = args.tool_schema_bytes if args.tool_schema_bytes is not None else (4096 if args.scenario == "smoke" else SCHEMA_BYTES)
    args.chunk_units = args.chunk_units if args.chunk_units is not None else (0 if args.scenario == "smoke" else 4000)
    args.custom_grammar = args.custom_grammar if args.custom_grammar is not None else args.scenario != "smoke" and args.protocol != "chat"
    if args.custom_grammar and args.protocol == "chat":
        parser.error("the custom Lark wire format requires Responses")
    if args.scenario == "smoke" and (args.tool_count != 3 or not 2048 <= args.tool_schema_bytes <= 8192 or args.chunk_units > 1000):
        parser.error("smoke uses three tools, 2–8KB schema and small synthetic results")
    if args.scenario != "smoke" and (args.tool_count < TOOL_COUNT or args.tool_schema_bytes < SCHEMA_BYTES):
        parser.error("keep the original observed tool count/schema footprint or larger")
    args.padding_units = args.padding_units if args.padding_units is not None else (100000 if args.scenario == "stress" else 0)
    if min(args.padding_units, args.chunk_units) < 0 or args.max_requests < 6 or args.timeout <= 0 or args.request_timeout <= 0:
        parser.error("invalid bounded workload")
    conditions = {"scenario": args.scenario, "model": MODEL, "service_tier": "priority", "reasoning_effort": "medium", "context_window": 500000,
                  "auto_compact_token_limit": 400000, "tool_count": args.tool_count, "tool_schema_bytes": args.tool_schema_bytes,
                  "padding_units": args.padding_units, "chunk_units": args.chunk_units,
                  "custom_grammar": args.custom_grammar, "custom_grammar_sha256": digest(RECEIPT_GRAMMAR) if args.custom_grammar else None}
    if args.dry_run:
        print(json.dumps({"dry_run": True, "conditions": conditions, "max_http_requests": 12 if args.protocol == "both" else 6,
                          "schema_source": "small synthetic smoke" if args.scenario == "smoke" else "synthetic tools matching observed count and serialized byte footprint", "stress_coverage": "500K route, not proof of 500K occupancy"}, indent=2))
        return 0
    args.state_dir = private_path(args.state_dir)
    root = private_path(args.output)
    if root.exists():
        parser.error("output must be a new directory; existing evidence is never overwritten")
    keys = json.loads((args.state_dir / "keys.json").read_text())
    isolation_path = args.state_dir / "isolation.json"
    isolation = json.loads(isolation_path.read_text()) if isolation_path.exists() else {}
    if isolation.get("transport") != {"native": "native", "stateless": "host"}[args.arm]:
        parser.error("isolation.json must prove the requested arm's transport before any live request")
    root.mkdir(parents=True, mode=0o700)
    root.chmod(0o700)
    reports = [run_protocol(args, protocol, keys["client"], root) for protocol in (("chat", "responses") if args.protocol == "both" else (args.protocol,))]
    summary = {"arm": args.arm, "conditions": conditions, "isolation": isolation, "protocols": reports,
               "functional_passed": all(row["functional_passed"] for row in reports),
               "limitations": ["tool definitions are synthetic; full tools/stress match observed count/bytes, smoke is intentionally smaller", "schema padding uses explanatory prose; bytes are not a token count", f"{args.padding_units} synthetic padding units are not an exact tokenizer count or proof of 500K occupancy", "HTTP probes do not test Codex auto-compaction", "Dashboard billing-event correlation requires separate metadata evidence", "missing usage remains absent", "validating custom receipt text against the grammar does not prove upstream grammar-constrained decoding"]}
    (root / "summary.json").write_text(json.dumps(summary, indent=2))
    print(json.dumps({"arm": args.arm, "functional_passed": summary["functional_passed"], "output": str(root),
                      "protocols": [{"protocol": row["protocol"], "functional_passed": row["functional_passed"], "issues": row["issues"]} for row in reports]}, indent=2))
    return 0 if summary["functional_passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
