"""Synthetic regression coverage for cross-layer usage attribution and checking."""
import contextlib
import copy
import datetime
import importlib.util
import io
import json
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "integration" / "compare_dashboard.py"
SPEC = importlib.util.spec_from_file_location("compare_dashboard", SCRIPT)
comparison = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(comparison)
BASE = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)


def stamp(seconds):
    return (BASE + datetime.timedelta(seconds=seconds)).isoformat()


def event(index, seconds):
    return {
        "conversationId": "synthetic-conversation", "model": "synthetic-model",
        "timestamp": str(comparison.milliseconds(stamp(seconds + 1))),
        "usageEventId": f"synthetic-event-{index}",
        "tokenUsage": {"inputTokens": 80, "cacheReadTokens": 100,
                       "cacheWriteTokens": 20, "outputTokens": 9, "reasoningTokens": 3},
    }


def journal(seconds, internal=0):
    return {
        "started": stamp(seconds), "ended": stamp(seconds + 5),
        "conversation_id": "synthetic-conversation", "model": "synthetic-model",
        "source": "turn_ended", "internal_tool_calls": internal,
        "usage": {"prompt_tokens": 200, "cached_tokens": 100,
                  "cache_write_tokens": 20, "completion_tokens": 9,
                  "reasoning_tokens": 3, "total_tokens": 209},
    }


def cpa(seconds, zero=False):
    scale = 0 if zero else 1
    return {
        "timestamp": stamp(seconds), "failed": False,
        "tokens": {"input_tokens": 200 * scale, "cached_tokens": 100 * scale,
                   "cache_read_tokens": 100 * scale, "cache_creation_tokens": 20 * scale,
                   "output_tokens": 9 * scale, "reasoning_tokens": 3 * scale,
                   "total_tokens": 209 * scale},
        "token_breakdown": {
            "quality": "complete", "total_tokens": 209 * scale,
            "input": {"total_tokens": 200 * scale, "uncached_tokens": 80 * scale,
                      "cache_read_tokens": 100 * scale, "cache_write_tokens": 20 * scale},
            "output": {"total_tokens": 9 * scale, "non_reasoning_tokens": 6 * scale,
                       "reasoning_tokens": 3 * scale},
        },
    }


def client(seconds):
    return {
        "started": comparison.milliseconds(stamp(seconds)) / 1000,
        "usage": {"input_tokens": 200, "input_tokens_details": {"cached_tokens": 100},
                  "output_tokens": 9, "output_tokens_details": {"reasoning_tokens": 3},
                  "total_tokens": 209},
    }


class CompareDashboardTests(unittest.TestCase):
    def setUp(self):
        self.records = [journal(0), journal(10)]
        self.events = [event(1, 0), event(2, 10)]
        # Intentionally reverse both sources. Pairing by zip would be wrong.
        self.queue = [cpa(10), cpa(0)]
        self.clients = [client(10), client(0)]

    def report(self, queue=None, clients=None, events=None):
        return comparison.build_report(
            self.records, self.clients if clients is None else clients,
            self.queue if queue is None else queue, self.events if events is None else events,
        )

    def test_reordered_records_align_by_unique_start_time(self):
        result = self.report()
        self.assertTrue(result["passed"])
        self.assertEqual([row["cpa_alignment"] for row in result["rows"]],
                         ["unique request start time"] * 2)

    def test_cpa_numeric_mismatch_fails(self):
        queue = copy.deepcopy(self.queue)
        queue[0]["tokens"]["output_tokens"] = 10
        queue[0]["tokens"]["total_tokens"] = 210
        result = self.report(queue=queue)
        self.assertFalse(result["passed"])
        self.assertTrue(any("CPA.output_tokens" in error for error in result["rows"][1]["errors"]))

    def test_reasoning_mismatch_fails(self):
        clients = copy.deepcopy(self.clients)
        clients[0]["usage"]["output_tokens_details"]["reasoning_tokens"] = 4
        result = self.report(clients=clients)
        self.assertFalse(result["passed"])
        self.assertTrue(any("reasoning_tokens" in error for error in result["rows"][1]["errors"]))

    def test_cache_write_mismatch_fails(self):
        events = copy.deepcopy(self.events)
        events[0]["tokenUsage"]["cacheWriteTokens"] = 21
        result = self.report(events=events)
        self.assertFalse(result["passed"])
        self.assertTrue(any("cache_write_tokens" in error for error in result["rows"][0]["errors"]))

    def test_ambiguous_start_times_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "uniquely attributed"):
            comparison.align_by_time([self.records[0]], [cpa(0), cpa(1)], "timestamp")

    def test_context_unknown_is_omitted_and_host_zero_is_explicit(self):
        result = comparison.build_report(
            [journal(0, internal=1)], [{"started": client(0)["started"], "usage": None}],
            [cpa(0, zero=True)], [event(1, 0)],
        )
        self.assertTrue(result["passed"])
        row = result["rows"][0]
        self.assertTrue(row["context_unknown"])
        self.assertTrue(row["run_total_withheld_from_context"])
        self.assertIsNone(row["responses"])
        self.assertIn("CPA records unknown as zero with quality=complete", row["limitations"])

    def test_cli_numeric_failure_exits_one_and_preserves_report(self):
        queue = copy.deepcopy(self.queue)
        queue[0]["tokens"]["cache_creation_tokens"] = 19
        with tempfile.TemporaryDirectory(prefix="compare-dashboard-synthetic-") as temporary:
            root = Path(temporary)
            (root / "journal").mkdir()
            (root / "oauth").mkdir()
            (root / "journal/usage.json").write_text(json.dumps({"records": self.records}))
            (root / "controlled-summary.json").write_text(json.dumps(self.clients))
            (root / "controlled-usage-queue.json").write_text(json.dumps(queue))
            (root / "oauth/cursor-test.json").write_text(json.dumps({
                "cursor_auth": {"type": "oauth", "access": "synthetic-never-sent"},
            }))
            response = subprocess.CompletedProcess(["curl"], 0, json.dumps({"usageEventsDisplay": self.events}), "")
            with mock.patch.object(sys, "argv", [str(SCRIPT), "--state-dir", str(root)]), \
                    mock.patch("subprocess.run", return_value=response), \
                    contextlib.redirect_stdout(io.StringIO()), \
                    self.assertRaises(SystemExit) as exited:
                runpy.run_path(str(SCRIPT), run_name="__main__")
            self.assertEqual(exited.exception.code, 1)
            saved = json.loads((root / "controlled-dashboard-comparison.json").read_text())
            self.assertFalse(saved["passed"])
            self.assertTrue(saved["rows"][1]["errors"])
            self.assertEqual(len(list(root.glob("controlled-dashboard-comparison-*.json"))), 1)


if __name__ == "__main__":
    unittest.main()
