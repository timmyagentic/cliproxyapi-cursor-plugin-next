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
PRESENCE = {"input_tokens": "input_tokens", "cached_tokens": "cache_read_tokens",
            "cache_write_tokens": "cache_write_tokens", "output_tokens": "output_tokens",
            "reasoning_tokens": "reasoning_tokens", "total_tokens": "total_tokens"}


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
    result = counts(tokens.get("input_tokens"), tokens.get("cache_read_tokens", tokens.get("cached_tokens")), tokens.get("cache_creation_tokens"), tokens.get("output_tokens"), tokens.get("total_tokens"), tokens.get("reasoning_tokens"))
    observed = record["token_breakdown"].get("observed")
    if observed is None:
        return result
    if not isinstance(observed, dict) or any(type(observed.get(key)) is not bool for key in PRESENCE.values()):
        raise ValueError("CPA observed presence must contain boolean counters")
    for key, flag in PRESENCE.items():
        known = observed[flag] or key == "total_tokens" and observed["input_tokens"] and observed["output_tokens"]
        if not known:
            if result[key] not in (None, 0):
                raise ValueError("CPA unobserved counter is nonzero: " + key)
            result[key] = None
    return counts(result["input_tokens"], result["cached_tokens"], result["cache_write_tokens"], result["output_tokens"], result["total_tokens"], result["reasoning_tokens"])


def breakdown_counts(record):
    breakdown = record["token_breakdown"]
    inp, out = breakdown["input"], breakdown["output"]
    # Classified input/output buckets need not equal raw parent counters: an
    # unknown subdivision belongs in unclassified_tokens, not a fabricated zero.
    classified = integer(inp.get("total_tokens")) + integer(out.get("total_tokens"))
    result = counts(inp.get("total_tokens"), inp.get("cache_read_tokens"), inp.get("cache_write_tokens"), out.get("total_tokens"), classified, out.get("reasoning_tokens"))
    if result["uncached_tokens"] != integer(inp.get("uncached_tokens")):
        raise ValueError("CPA uncached breakdown inconsistent")
    if result["output_tokens"] - result["reasoning_tokens"] != integer(out.get("non_reasoning_tokens")):
        raise ValueError("CPA output breakdown inconsistent")
    unclassified = integer(breakdown.get("unclassified_tokens", 0))
    result["total_tokens"] = integer(breakdown.get("total_tokens"))
    result["unclassified_tokens"] = unclassified
    if result["total_tokens"] != classified + unclassified:
        raise ValueError("CPA classified and unclassified total inconsistent")
    quality = breakdown.get("quality")
    if quality not in ("complete", "unclassified") or quality == "complete" and unclassified:
        raise ValueError("CPA breakdown quality inconsistent")
    observed = breakdown.get("observed")
    if observed is not None:
        raw = cpa_counts(record)
        expected = {"uncached_tokens": raw["uncached_tokens"] or 0,
                    "cached_tokens": raw["cached_tokens"] or 0,
                    "cache_write_tokens": raw["cache_write_tokens"] or 0,
                    "reasoning_tokens": raw["reasoning_tokens"] or 0}
        expected["input_tokens"] = sum(expected[key] for key in ("uncached_tokens", "cached_tokens", "cache_write_tokens"))
        non_reasoning = raw["output_tokens"] - raw["reasoning_tokens"] if None not in (raw["output_tokens"], raw["reasoning_tokens"]) else 0
        expected["output_tokens"] = non_reasoning + expected["reasoning_tokens"]
        expected["total_tokens"] = integer(record["tokens"]["total_tokens"])
        expected["unclassified_tokens"] = expected["total_tokens"] - expected["input_tokens"] - expected["output_tokens"]
        if any(result[key] != value for key, value in expected.items()):
            raise ValueError("CPA breakdown does not match observed classifications")
        wanted_quality = "unclassified" if unclassified or not any(observed.values()) else "complete"
        if quality != wanted_quality:
            raise ValueError("CPA observed breakdown quality inconsistent")
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


def invented_measurements(expected, actual, label):
    return [f"{label}.{key}: unknown counter published as {actual[key]}"
            for key in COUNTERS if expected[key] is None and actual[key] is not None]


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
        first = journal_counts(record["first_usage"]) if record.get("first_usage") is not None else observed
        revised = first != observed or record.get("revision", 1) > 1
        history = [journal_counts(item["usage"]) for item in record.get("observations", [])
                   if item.get("usage") is not None and item.get("kind") in ("observation", "revision")
                   and item.get("event_id") == record.get("event_id") and item.get("source") == record.get("source")]
        if observed is not None:
            latest_differences = differences(upstream, observed, "journal")
            if record.get("provisional") and record.get("source") == "dashboard":
                if latest_differences:
                    limitations.append("dashboard changed since latest journal observation; counters remain provisional")
            else:
                errors += latest_differences
        else:
            limitations.append("journal measurement remains unknown")
        client_raw = client_rows[index].get("usage")
        client = None if client_raw is None else client_counts(client_raw)
        cpa = cpa_counts(billed_rows[index])
        breakdown = breakdown_counts(billed_rows[index])
        presence = billed_rows[index]["token_breakdown"].get("observed")
        context_unknown = bool(record.get("internal_tool_calls") or record.get("internal_steps")) or observed is None
        if context_unknown:
            if client is not None:
                errors.append("unknown current context was published as standard client usage")
            if presence is None:
                zeros = {key: 0 for key in COUNTERS}
                errors += differences(zeros, cpa, "CPA unknown-placeholder")
                errors += differences(zeros, breakdown, "CPA unknown-placeholder-breakdown")
                limitations.append("CPA records unknown as zero with quality=" + str(billed_rows[index]["token_breakdown"].get("quality")))
            elif any(presence.values()):
                errors.append("CPA unknown context contains observed counters")
            else:
                limitations.append("CPA presence marks unknown counters; zero storage fields are not measurements")
        elif client is None:
            errors.append("measured current context missing from client response")
        else:
            reference = observed
            optional = ("cache_write_tokens", "uncached_tokens") if presence is None else ()
            if revised:
                # A revision is another observation of the same Run, not another
                # bill or proof that the original response used the latest value.
                matches = [snapshot for snapshot in [first, *history, observed] if snapshot is not None
                           and not differences(snapshot, client, "Responses", optional)
                           and not differences(snapshot, cpa, "CPA")]
                if matches:
                    reference = matches[0]
                limitations.append("journal counters revised; response/CPA checked against a recorded snapshot, emission revision is not recorded")
                if not matches:
                    errors.append("response and CPA do not match the same recorded journal snapshot")
            elif not record.get("provisional"):
                errors += differences(upstream, client, "Responses", optional=optional)
                errors += differences(upstream, cpa, "CPA")
                if presence is None:
                    errors += differences(upstream, breakdown, "CPA breakdown")
            errors += differences(reference, client, "Responses-vs-journal", optional=optional)
            errors += differences(reference, cpa, "CPA-vs-journal")
            if presence is None:
                errors += differences(reference, breakdown, "CPA-breakdown-vs-journal")
            else:
                errors += invented_measurements(reference, client, "Responses")
                errors += invented_measurements(reference, cpa, "CPA")
            if client["cache_write_tokens"] is None:
                limitations.append("Responses host omits cache-write detail")
            if presence is None and billed_rows[index]["token_breakdown"].get("quality") != "complete":
                errors.append("CPA measured token breakdown is not complete")
        if billed_rows[index].get("failed"):
            errors.append("CPA request marked failed")
        report.append({"step": index + 1, "passed": not errors, "internal_tool_calls": record.get("internal_tool_calls", 0),
            "source": record.get("source"), "client_alignment": client_alignment, "cpa_alignment": "unique request start time",
            "dashboard_later": upstream, "journal_observed": observed, "responses": client, "cpa_tokens": cpa, "cpa_breakdown": breakdown,
            "journal_first": first, "journal_latest": observed, "journal_revision": record.get("revision", 1),
            "journal_changed": revised, "journal_provisional": bool(record.get("provisional")), "cpa_observed": presence,
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
        # Offline comparison consumes the stopped instance's final checkpoint;
        # it does not reconstruct a live v2 checkpoint from WAL segments.
        snapshot = json.loads((root / "journal/usage.json").read_text())
        if snapshot.get("version", 1) not in (1, 2):
            raise ValueError("unsupported journal checkpoint version")
        if snapshot.get("attribution_incomplete") or snapshot.get("lost_records") or snapshot.get("dropped_observations"):
            raise ValueError("journal checkpoint reports incomplete attribution")
        records = snapshot["records"]
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
