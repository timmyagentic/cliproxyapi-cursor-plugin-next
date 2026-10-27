"""Credential-free harness tests; no provider or local listener is contacted."""
import argparse
import copy
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import native_sessions
from native_sessions import (MODEL, RECEIPT_GRAMMAR, SCHEMA_BYTES, STATE_ROOT, ReceiptProtocol, StreamResult,
                             append_turn, canonical_name, compare, consume_sse, continuity, dashboard_evidence,
                             digest, encode, journal_snapshot, make_tools, private_path, run_protocol, sanitize_error)


def sse(*events):
    return io.BytesIO(b"".join(b"data: " + (event.encode() if isinstance(event, str) else encode(event)) + b"\n\n" for event in events))


class NativeSessionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        global STATE_ROOT
        cls.sandbox = tempfile.TemporaryDirectory(prefix="native-session-harness-")
        cls.prior_root = STATE_ROOT
        STATE_ROOT = Path(cls.sandbox.name).resolve()
        cls.scope_patch = patch.object(native_sessions, "STATE_ROOT", STATE_ROOT)
        cls.scope_patch.start()

    @classmethod
    def tearDownClass(cls):
        global STATE_ROOT
        cls.scope_patch.stop()
        STATE_ROOT = cls.prior_root
        cls.sandbox.cleanup()

    def test_original_schema_footprint_and_native_tool_shapes(self):
        for protocol in ("chat", "responses"):
            tools = make_tools(protocol)
            self.assertEqual(len(tools), 33)
            self.assertEqual(len(encode(tools)), SCHEMA_BYTES)
        tools = make_tools("responses")
        self.assertEqual(tools[1]["type"], "namespace")
        self.assertEqual(tools[2]["type"], "custom")

    def test_lark_custom_tool_keeps_exact_full_schema_byte_budget(self):
        tools = make_tools("responses", custom_grammar=True)
        self.assertEqual(len(tools), 33)
        self.assertEqual(len(encode(tools)), SCHEMA_BYTES)
        self.assertEqual(tools[2]["format"], {"type": "grammar", "syntax": "lark", "definition": RECEIPT_GRAMMAR})

    def test_custom_grammar_packet_validation_requires_exact_json_language(self):
        receipt = ReceiptProtocol("ab" * 20, step=3, custom_grammar=True)
        valid = {"step": 4, "receipt": receipt.receipt}
        receipt.execute({"name": "receipt_seal", "type": "custom_tool_call", "input": json.dumps(valid)})
        receipt = ReceiptProtocol("ab" * 20, step=3, custom_grammar=True)
        with self.assertRaisesRegex(ValueError, "Lark grammar"):
            receipt.execute({"name": "receipt_seal", "input": json.dumps({"receipt": receipt.receipt, "step": 4})})

    def test_scope_check_uses_configurable_portable_root(self):
        self.assertEqual(private_path(STATE_ROOT / "evidence"), STATE_ROOT / "evidence")
        with self.assertRaisesRegex(ValueError, "dedicated native task directory"):
            private_path(STATE_ROOT.parent / "outside-evidence")

    def test_receipts_require_real_result_and_exact_repeated_tool(self):
        receipt = ReceiptProtocol("seed")
        first = receipt.execute({"name": "receipt_seed", "arguments": json.dumps({"step": 1, "receipt": "seed"})})
        with self.assertRaisesRegex(ValueError, "actual previous receipt"):
            receipt.execute({"name": "advance", "namespace": "synthetic_receipts", "arguments": json.dumps({"step": 2, "receipt": "seed"})})
        receipt.execute({"name": "advance", "namespace": "synthetic_receipts", "arguments": json.dumps({"step": 2, "receipt": first["receipt"]})})
        with self.assertRaisesRegex(ValueError, "ordering"):
            receipt.execute({"name": "receipt_seal", "input": "{}"})

    def test_double_sse_and_multiple_done_are_failures(self):
        result = StreamResult("chat", 0)
        with self.assertRaisesRegex(ValueError, "double data"):
            consume_sse(io.BytesIO(b"data: data: {}\n\n"), result)
        with self.assertRaisesRegex(ValueError, "multiple DONE"):
            consume_sse(sse("[DONE]", "[DONE]"), StreamResult("chat", 0))

    def test_http200_done_without_semantic_terminal_cannot_pass(self):
        result = StreamResult("chat", 0)
        consume_sse(sse("[DONE]"), result)
        with self.assertRaisesRegex(ValueError, "finish reason"):
            result.finish(1)
        result = StreamResult("responses", 0)
        with self.assertRaisesRegex(ValueError, "stream failure"):
            result.ingest({"type": "response.failed"}, 1)

    def test_timing_separates_output_from_finalize_and_usage_stays_absent(self):
        result = StreamResult("chat", 10)
        result.ingest({"choices": [{"delta": {"content": "first"}}]}, 12)
        result.ingest({"choices": [{"delta": {"content": "last"}}]}, 14)
        result.ingest({"choices": [{"delta": {}, "finish_reason": "stop"}]}, 18)
        result.done_count = 1
        metrics = result.finish(19)
        self.assertEqual(metrics["ttft_ms"], 2000)
        self.assertEqual(metrics["first_to_last_output_ms"], 2000)
        self.assertEqual(metrics["finalization_ms"], 4000)
        self.assertEqual(metrics["terminal_to_eof_ms"], 1000)
        self.assertIsNone(metrics["usage"])
        self.assertFalse(metrics["usage_present"])

    def test_native_handles_must_share_one_session(self):
        self.assertTrue(continuity(["call_cursor_alpha_1_0", "call_cursor_alpha_2_0"])["same_native_session"])
        self.assertFalse(continuity(["call_cursor_alpha_1_0", "call_cursor_beta_2_0"])["same_native_session"])
        self.assertFalse(continuity(["ordinary_call"])["same_native_session"])

    def test_responses_preserves_text_and_tool_output_in_exact_order(self):
        message = {"type": "message", "id": "msg_synthetic", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "Checking the receipt first."}]}
        call = {"type": "function_call", "call_id": "call_cursor_fixture_1_0", "name": "receipt_seed", "arguments": '{"step":1,"receipt":"seed"}'}
        result = StreamResult("responses", 0)
        result.ingest({"type": "response.completed", "response": {"output": [message, call]}}, 1)
        history = [{"role": "user", "content": "synthetic"}]
        append_turn("responses", history, result, {"receipt": "actual-result"})
        self.assertEqual(history[1:3], [message, call])
        self.assertEqual(history[3]["type"], "function_call_output")
        self.assertEqual(history[3]["call_id"], call["call_id"])
        history[1]["content"][0]["text"] = "modified downstream copy"
        self.assertEqual(result.output_items[0], message)

    def test_responses_preserves_reasoning_and_custom_tool_output(self):
        reasoning = {"type": "reasoning", "id": "rs_synthetic", "summary": [{"type": "summary_text", "text": "Synthetic reasoning summary."}]}
        call = {"type": "custom_tool_call", "call_id": "call_cursor_fixture_4_0", "name": "receipt_seal", "input": '{"step":4,"receipt":"actual"}'}
        result = StreamResult("responses", 0)
        result.ingest({"type": "response.completed", "response": {"output": [reasoning, call]}}, 1)
        history = []
        append_turn("responses", history, result, {"receipt": "sealed"})
        self.assertEqual(history[:2], [reasoning, call])
        self.assertEqual(history[2]["type"], "custom_tool_call_output")

    def test_small_chat_and_responses_protocols_with_fake_wire(self):
        STATE_ROOT.joinpath("tmp").mkdir(parents=True, exist_ok=True)
        for protocol in ("chat", "responses"):
            with self.subTest(protocol=protocol), tempfile.TemporaryDirectory(dir=STATE_ROOT / "tmp") as temp:
                args = argparse.Namespace(port=29999, tool_count=3, tool_schema_bytes=4096,
                        padding_units=0, chunk_units=0, max_requests=6, timeout=30, request_timeout=5,
                        max_body_bytes=2 * 1024 * 1024, arm="native", state_dir=Path(temp))
                calls = []

                def fake_urlopen(request, timeout):
                    payload = json.loads(request.data)
                    self.assertEqual(payload["model"], MODEL)
                    self.assertEqual(payload["service_tier"], "priority")
                    self.assertEqual(payload.get("reasoning", {}).get("effort", payload.get("reasoning_effort")), "medium")
                    history = payload["messages" if protocol == "chat" else "input"]
                    index = len(calls)
                    calls.append(payload)
                    if index == 0:
                        seed = history[0]["content"].split("initial receipt for step 1 is ")[1].split(".")[0]
                    elif index < 5:
                        seed = json.loads(history[-1]["content" if protocol == "chat" else "output"])["receipt"]
                    else:
                        content = history[-2]["content"]
                        text = "".join(part.get("text", "") for part in content) if isinstance(content, list) else content
                        seed = text.split("VERIFIED ")[1]
                    if index < 4:
                        name = ("receipt_seed", "synthetic_receipts__advance", "synthetic_receipts__advance", "receipt_seal")[index]
                        arguments = json.dumps({"step": index + 1, "receipt": seed})
                        call_id = f"call_cursor_fixture_{index + 1}_0"
                        if protocol == "chat":
                            events = [{"choices": [{"delta": {"tool_calls": [{"index": 0, "id": call_id, "function": {"name": name, "arguments": arguments}}]}, "finish_reason": "tool_calls"}]}, "[DONE]"]
                        else:
                            item = {"type": "function_call", "call_id": call_id, "name": name, "arguments": arguments}
                            if index in (1, 2):
                                item.update(name="advance", namespace="synthetic_receipts")
                            if index == 3:
                                item = {"type": "custom_tool_call", "call_id": call_id, "name": name, "input": arguments}
                            events = [{"type": "response.function_call_arguments.delta", "delta": arguments}, {"type": "response.completed", "response": {"output": [item]}}]
                    else:
                        text = "VERIFIED " + seed
                        events = ([{"choices": [{"delta": {"content": text}, "finish_reason": "stop"}]}, "[DONE]"] if protocol == "chat" else
                                  [{"type": "response.output_text.delta", "delta": text}, {"type": "response.completed", "response": {"output": [{"type": "message", "content": [{"type": "output_text", "text": text}]}]}}])
                    response = sse(*events)
                    response.headers = {}
                    return response

                with patch("native_sessions.urllib.request.urlopen", side_effect=fake_urlopen):
                    report = run_protocol(args, protocol, "synthetic-key", Path(temp))
                self.assertTrue(report["functional_passed"], report["issues"])
                self.assertEqual(report["verified_tools"], 4)
                self.assertEqual(len(calls), 6)
                self.assertTrue(report["ordinary_followup_verified"])

    def report(self):
        return {"conditions": {"model": MODEL}, "isolation": {"cpa_sha256": "cpa", "plugin_sha256": "plugin", "credential_sha256": "credential",
                "effective_proxy_sha256": "proxy", "usage_wait_ms": 8000, "request_retry": 0}, "protocols": [{"protocol": "chat", "functional_passed": True,
                "requests": [{"ttft_ms": 100}], "total_ms": 200, "started_wall": 10, "ended_wall": 20,
                "journal": {"present": True, "records": [{"conversation_id_hash": digest("synthetic-cid")}]}}]}

    def test_compare_checks_same_binary_credentials_and_conditions(self):
        native, stateless = self.report(), self.report()
        self.assertTrue(compare(native, stateless)["comparable"])
        stateless["isolation"]["plugin_sha256"] = "other"
        self.assertIn("plugin_sha256", compare(native, stateless)["condition_mismatches"])
        self.assertIsNone(compare(native, stateless)["dashboard_native"]["billing_events"])

    def test_billing_attribution_needs_conversation_and_time_not_http_count(self):
        fixture = {"rows": [{"timestamp_ms": 15000, "conversationId": "synthetic-cid", "usageEventId": "one", "tokenUsage": {}},
                             {"timestamp_ms": 16000, "conversationId": "unrelated", "usageEventId": "two", "tokenUsage": {}},
                             {"timestamp_ms": 50000, "conversationId": "synthetic-cid", "usageEventId": "three", "tokenUsage": {}}]}
        evidence = dashboard_evidence(self.report(), fixture)
        self.assertEqual(evidence["billing_events"], 1)
        self.assertEqual(evidence["window_candidates"], 2)

    def test_null_checkpoint_and_live_segments_select_latest_record_sequence(self):
        STATE_ROOT.joinpath("tmp").mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=STATE_ROOT / "tmp") as temp:
            root = Path(temp)
            journal = root / "journal"
            journal.mkdir()
            (journal / "usage.json").write_text('{"version":2,"records":null}')
            segments = journal / "usage.json.segments"
            segments.mkdir()
            record = {"id": "synthetic-run", "started": "1970-01-01T00:00:00.500000+00:00", "status": "running", "sequence": 1}
            updated = {**record, "status": "unknown", "sequence": 2}
            frames = b"\n".join(encode({"sequence": value["sequence"], "record": value}) for value in (record, updated)) + b'\n{"sequence":3'
            (segments / "events-00000000000000000001.jsonl").write_bytes(frames)
            result = journal_snapshot(root, 0, 1)
            self.assertTrue(result["present"])
            self.assertEqual(len(result["records"]), 1)
            self.assertEqual(result["records"][0]["sequence"], 2)
            self.assertEqual(result["records"][0]["status"], "unknown")
            self.assertIn("partial_append_tail_ignored", result["diagnostics"])

    def test_failed_stream_is_sanitized_and_saved_without_journal_accounting(self):
        STATE_ROOT.joinpath("tmp").mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=STATE_ROOT / "tmp") as temp:
            root = Path(temp)
            args = argparse.Namespace(port=29999, tool_count=3, tool_schema_bytes=4096,
                    padding_units=0, chunk_units=0, max_requests=6, timeout=30, request_timeout=5,
                    max_body_bytes=2 * 1024 * 1024, arm="native", state_dir=root)
            response = sse({"error": {"code": "synthetic_rejection", "message": "Invalid model; Bearer cursor_12345678901234567890 https://private.test/path"}})
            response.headers = {}
            with patch("native_sessions.urllib.request.urlopen", return_value=response):
                report = run_protocol(args, "chat", "synthetic-client-key", root)
            self.assertFalse(report["functional_passed"])
            self.assertEqual(len(report["requests"]), 1)
            self.assertEqual(report["requests"][0]["stream_error"]["code"], "synthetic_rejection")
            self.assertFalse(report["journal"]["present"])
            saved = (root / "chat-requests.json").read_text()
            self.assertNotIn("cursor_12345678901234567890", saved)
            self.assertNotIn("private.test", saved)
            self.assertIn("Invalid model", saved)
            self.assertTrue((root / "chat-issues.json").exists())


if __name__ == "__main__":
    unittest.main()
