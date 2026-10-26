#!/usr/bin/env python3
"""Independently compare isolated Runs with Cursor, the client and CPA.

Uses only the existing copied credential. Authorization reaches curl through
stdin with TLS verification enabled. Saved reports contain measurement fields
only. Numeric mismatches or ambiguous attribution return a nonzero exit code.
"""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import subprocess
import sys


COUNTERS = ("input_tokens", "uncached_tokens", "cached_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens", "total_tokens")


def milliseconds(value):
    if isinstance(value, (float, int)) and not isinstance(value, bool):
        return round(value * 1000)
    if not isinstance(value, str):
        raise ValueError("request timestamp missing or invalid")
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("request timestamp must include timezone")
    return int(parsed.timestamp() * 1000)


def integer(value):
    if isinstance(value, bool) or not isinstance(value, (int, str)):
        raise ValueError("invalid token counter")
    result = int(value)
    if result < 0 or result > (1 << 53) - 1:
        raise ValueError("token counter outside exact JSON range")
    return result


def counts(input_total, cached, written, output, total, reasoning=None):
    result = dict(zip(COUNTERS, (input_total, None, cached, written, output, reasoning, total)))
    result = {key: None if value is None else integer(value) for key, value in result.items()}
    if None not in (result["input_tokens"], result["cached_tokens"], result["cache_write_tokens"]):
        result["uncached_tokens"] = result["input_tokens"] - result["cached_tokens"] - result["cache_write_tokens"]
        if result["uncached_tokens"] < 0:
            raise ValueError("cache counters exceed input total")
    if result["reasoning_tokens"] is not None and result["output_tokens"] is not None and result["reasoning_tokens"] > result["output_tokens"]:
        raise ValueError("reasoning exceeds output total")
    if None not in (result["input_tokens"], result["output_tokens"], result["total_tokens"]) and result["total_tokens"] != result["input_tokens"] + result["output_tokens"]:
        raise ValueError("inconsistent token total")
    return result


def dashboard_counts(tokens):
    uncached, cached, written, output = (integer(tokens.get(key, 0)) for key in ("inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens"))
    total_input = uncached + cached + written
    return counts(total_input, cached, written, output, total_input + output, tokens.get("reasoningTokens"))


def journal_counts(usage):
    return counts(usage.get("prompt_tokens"), usage.get("cached_tokens"), usage.get("cache_write_tokens"), usage.get("completion_tokens"), usage.get("total_tokens"), usage.get("reasoning_tokens"))


def client_counts(usage):
    inp = usage.get("input_tokens_details", {})
    out = usage.get("output_tokens_details", {})
    return counts(usage.get("input_tokens"), inp.get("cached_tokens"), inp.get("cache_write_tokens"), usage.get("output_tokens"), usage.get("total_tokens"), out.get("reasoning_tokens"))


def cpa_counts(record):
    tokens = record["tokens"]
    if "cached_tokens" in tokens and "cache_read_tokens" in tokens and tokens["cached_tokens"] != tokens["cache_read_tokens"]:
        raise ValueError("CPA cached and cache-read counters disagree")
    return counts(tokens.get("input_tokens"), tokens.get("cache_read_tokens", tokens.get("cached_tokens")), tokens.get("cache_creation_tokens"), tokens.get("output_tokens"), tokens.get("total_tokens"), tokens.get("reasoning_tokens"))


def breakdown_counts(record):
    breakdown = record["token_breakdown"]
    inp, out = breakdown["input"], breakdown["output"]
    result = counts(inp.get("total_tokens"), inp.get("cache_read_tokens"), inp.get("cache_write_tokens"), out.get("total_tokens"), breakdown.get("total_tokens"), out.get("reasoning_tokens"))
    if result["uncached_tokens"] != inp.get("uncached_tokens"):
        raise ValueError("CPA uncached breakdown inconsistent")
    if result["output_tokens"] - result["reasoning_tokens"] != out.get("non_reasoning_tokens"):
        raise ValueError("CPA output breakdown inconsistent")
    return result


def align_by_time(records, candidates, key):
    """Require a unique request start within two seconds; never trust queue order."""
    aligned, used = [], set()
    for record in records:
        matches = [index for index, candidate in enumerate(candidates)
                   if abs(milliseconds(candidate[key]) - milliseconds(record["started"])) <= 2000]
        if len(matches) != 1 or matches[0] in used:
            raise ValueError("request time cannot be uniquely attributed")
        used.add(matches[0])
        aligned.append(candidates[matches[0]])
    if len(used) != len(candidates):
        raise ValueError("unmatched request records remain")
    return aligned


def differences(expected, actual, label, optional=()):
    failures = []
    for key in COUNTERS:
        if expected[key] is None:
            continue
        if actual[key] is None and key in optional:
            continue
        if expected[key] != actual[key]:
            failures.append(f"{label}.{key}: expected {expected[key]}, observed {actual[key]}")
    return failures


def build_report(records, emitted, queue, events):
    records = sorted(records, key=lambda record: milliseconds(record["started"]))
    if not records or len(records) != len(emitted) or len(records) != len(queue):
        raise ValueError("request, journal and CPA record counts do not match")
    billed_rows = align_by_time(records, queue, "timestamp")
    if all("started" in row for row in emitted):
        client_rows = align_by_time(records, emitted, "started")
        client_alignment = "unique request start time"
    else:
        # controlled_tools explicitly numbers sequential requests. Its original
        # fixtures predate per-request timestamps; the CPA alignment stays strict.
        by_step = {row["step"]: row for row in emitted}
        if len(by_step) != len(records) or set(by_step) != set(range(1, len(records) + 1)):
            raise ValueError("controlled client sequence is ambiguous")
        client_rows = [by_step[step] for step in range(1, len(records) + 1)]
        client_alignment = "explicit sequential controlled step"
    report = []
    for index, record in enumerate(records):
        errors, limitations = [], []
        candidates = [event for event in events if event.get("conversationId") == record["conversation_id"]
            and milliseconds(record["started"]) - 1000 <= int(event["timestamp"]) <= milliseconds(record["ended"]) + 1000
            and event.get("model") == record["model"]]
        if len(candidates) != 1 or not isinstance(candidates[0].get("tokenUsage"), dict):
            raise ValueError("dashboard event cannot be uniquely matched to Run")
        event = candidates[0]
        if record.get("source") == "dashboard":
            event_hash = hashlib.sha256(("cursor dashboard event\0" + event["usageEventId"]).encode()).hexdigest()
            if event_hash != record.get("event_id"):
                errors.append("dashboard event identity changed")
        upstream = dashboard_counts(event["tokenUsage"])
        observed = journal_counts(record["usage"]) if record.get("usage") is not None else None
        if observed is not None:
            errors += differences(upstream, observed, "journal")
        else:
            limitations.append("journal measurement remains unknown")
        client_raw = client_rows[index].get("usage")
        client = None if client_raw is None else client_counts(client_raw)
        cpa = cpa_counts(billed_rows[index])
        breakdown = breakdown_counts(billed_rows[index])
        context_unknown = bool(record.get("internal_tool_calls")) or observed is None
        if context_unknown:
            if client is not None:
                errors.append("unknown current context was published as standard client usage")
            zeros = {key: 0 for key in COUNTERS}
            errors += differences(zeros, cpa, "CPA unknown-placeholder")
            errors += differences(zeros, breakdown, "CPA unknown-placeholder-breakdown")
            limitations.append("CPA records unknown as zero with quality=" + str(billed_rows[index]["token_breakdown"].get("quality")))
        elif client is None:
            errors.append("measured current context missing from client response")
        else:
            errors += differences(upstream, client, "Responses", optional=("cache_write_tokens", "uncached_tokens"))
            errors += differences(observed, client, "Responses-vs-journal", optional=("cache_write_tokens", "uncached_tokens"))
            errors += differences(upstream, cpa, "CPA")
            errors += differences(upstream, breakdown, "CPA breakdown")
            errors += differences(observed, cpa, "CPA-vs-journal")
            errors += differences(observed, breakdown, "CPA-breakdown-vs-journal")
            if client["cache_write_tokens"] is None:
                limitations.append("Responses host omits cache-write detail")
            if billed_rows[index]["token_breakdown"].get("quality") != "complete":
                errors.append("CPA measured token breakdown is not complete")
        if billed_rows[index].get("failed"):
            errors.append("CPA request marked failed")
        report.append({"step": index + 1, "passed": not errors, "internal_tool_calls": record.get("internal_tool_calls", 0),
            "source": record.get("source"), "client_alignment": client_alignment, "cpa_alignment": "unique request start time",
            "dashboard_later": upstream, "journal_observed": observed, "responses": client, "cpa_tokens": cpa, "cpa_breakdown": breakdown,
            "context_unknown": context_unknown, "run_total_withheld_from_context": context_unknown and client is None,
            "errors": errors, "limitations": limitations})
    return {"passed": all(row["passed"] for row in report), "rows": report}


def cursor(path, token, body):
    config = "\n".join(["url = " + json.dumps("https://api2.cursor.sh" + path), 'request = "POST"',
        "header = " + json.dumps("Authorization: Bearer " + token), 'header = "Content-Type: application/json"',
        'header = "Connect-Protocol-Version: 1"', "data = " + json.dumps(json.dumps(body)),
        "silent", "show-error", "max-time = 30", "fail-with-body"])
    response = subprocess.run(["curl", "--config", "-"], input=config, text=True, capture_output=True)
    if response.returncode:
        raise RuntimeError("Cursor read-only query failed; credentials and response omitted")
    return json.loads(response.stdout)


def compare(root, summary_path=None, cpa_path=None):
    report = {"passed": False, "errors": [], "rows": []}
    try:
        emitted = json.loads((summary_path or root / "controlled-summary.json").read_text())
        if isinstance(emitted, dict):
            emitted = emitted["requests"]
        records = json.loads((root / "journal/usage.json").read_text())["records"]
        queue = json.loads((cpa_path or root / "controlled-usage-queue.json").read_text())
        auth = json.loads((root / "oauth/cursor-test.json").read_text())["cursor_auth"]
        token = cursor("/auth/exchange_user_api_key", auth["key"], {})["accessToken"] if auth["type"] == "api" else auth["access"]
        events = []
        for page in range(1, 21):
            data = cursor("/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, {
                "teamId": 0, "startDate": str(min(milliseconds(r["started"]) for r in records) - 1000),
                "endDate": str(max(milliseconds(r["ended"]) for r in records) + 1000), "page": page, "pageSize": 50})
            batch = data.get("usageEventsDisplay", [])
            events.extend(batch)
            if len(batch) < 50:
                break
        else:
            raise ValueError("bounded dashboard pagination exhausted")
        report = build_report(records, emitted, queue, events)
    except (ValueError, KeyError, TypeError, OSError, RuntimeError) as error:
        report["errors"].append(str(error))
    encoded = json.dumps(report, indent=2)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    (root / f"controlled-dashboard-comparison-{stamp}.json").write_text(encoded)
    (root / "controlled-dashboard-comparison.json").write_text(encoded)
    print(json.dumps({"passed": report["passed"], "rows": [{key: row[key] for key in ("step", "passed", "internal_tool_calls", "context_unknown", "errors", "limitations")} for row in report["rows"]], "errors": report.get("errors", [])}), flush=True)
    return report["passed"]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--summary-file", "--codex-result", dest="summary_file", type=Path, help="controlled summary list or codex_probe result.json")
    parser.add_argument("--cpa-records", type=Path, help="sanitized CPA usage queue JSON")
    args = parser.parse_args()
    sys.exit(0 if compare(args.state_dir.resolve(), args.summary_file, args.cpa_records) else 1)
