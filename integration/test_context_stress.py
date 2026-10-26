"""Offline validation of the bounded stress probe; no model/network is used."""
import argparse
import time
import unittest

from context_stress import Budget, Plan, assess, filler, observed_input, plans, tool_specs


class ContextStressTests(unittest.TestCase):
    def plan(self, name="cross"):
        return Plan(name, 200000, 166000, 6000, 8)

    def calls(self):
        return [{"round": i + 1, "success": True, "at": float(i)} for i in range(8)]

    def requests(self, values):
        return [{"started": at - .1, "ended": float(at), "status": 200, "usage": None if value is None else {"input_tokens": value}} for at, value in values]

    def compact(self, *times):
        return [{"at": float(at), "method": "item/completed", "item_type": "contextCompaction"} for at in times]

    def test_plan_preserves_threshold_and_exercises_32_tools(self):
        approach, cross = plans("both", 200000)
        self.assertEqual((approach.initial_units, approach.chunk_units, approach.rounds), (176000, 2000, 4))
        self.assertEqual((cross.initial_units, cross.chunk_units, cross.rounds), (166000, 6000, 8))
        self.assertEqual(len(tool_specs()), 32)
        self.assertEqual(len(filler(123).encode()), 246)
        self.assertEqual(plans("cross", 200000), [cross])
        with self.assertRaises(ValueError):
            plans("both", 100)

    def test_below_threshold_can_pass_without_compaction(self):
        plan = Plan("approach", 200000, 176000, 2000, 4)
        result = assess(plan, self.requests([(1, 182000), (2, 190000)]), [], self.calls()[:4], True, True)
        self.assertTrue(result["passed"], result)
        result = assess(plan, self.requests([(1, 220000)]), [], self.calls()[:4], True, True)
        self.assertEqual(result["status"], "inconclusive")

    def test_natural_compaction_requires_near_threshold_and_context_drop(self):
        result = assess(self.plan(), self.requests([(1, 185000), (2, 198000), (4, 26000), (8, 50000)]), self.compact(3), self.calls(), True, True)
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["compactions"], 1)
        self.assertEqual(result["max_emitted_input_tokens"], 198000)

    def test_unknown_usage_does_not_prove_threshold_coverage(self):
        result = assess(self.plan(), self.requests([(1, None), (2, None)]), self.compact(3), self.calls(), True, True)
        self.assertFalse(result["passed"])
        self.assertEqual(result["status"], "inconclusive")
        self.assertEqual(result["unknown_response_count"], 2)

    def test_old_high_measurement_cannot_authorize_repeated_low_context_compaction(self):
        result = assess(self.plan(), self.requests([(1, 198000), (4, 26000), (6, 24000)]), self.compact(3, 5), self.calls(), True, True)
        self.assertEqual(result["status"], "failed")
        self.assertTrue(any("loop" in issue for issue in result["issues"]))
        self.assertTrue(any("preceding" in gap for gap in result["limitations"]))

    def test_compaction_without_context_reduction_is_not_accepted(self):
        result = assess(self.plan(), self.requests([(1, 198000), (4, 197000)]), self.compact(3), self.calls(), True, True)
        self.assertEqual(result["status"], "inconclusive")
        self.assertTrue(any("reduction" in gap for gap in result["limitations"]))

    def test_receipt_or_request_failure_cannot_pass(self):
        calls = self.calls()
        calls[-1]["success"] = False
        result = assess(self.plan(), self.requests([(1, 198000), (4, 26000)]), self.compact(3), calls, True, True)
        self.assertEqual(result["status"], "failed")
        requests = self.requests([(1, 198000), (4, 26000)])
        requests[-1]["status"] = 429
        result = assess(self.plan(), requests, self.compact(3), self.calls(), True, True)
        self.assertEqual(result["status"], "failed")

    def test_invalid_or_explicitly_unknown_numbers_are_not_measurements(self):
        for usage in (None, {}, {"input_tokens": -1}, {"input_tokens": True}, {"input_tokens": "200000"}, {"input_tokens": 200000, "quality": "unknown"}, {"input_tokens": 200000, "cursor_usage_estimated": True}):
            self.assertIsNone(observed_input(usage), usage)
        self.assertEqual(observed_input({"input_tokens": 200000}), 200000)

    def test_request_cost_and_time_budgets_are_enforced_before_forward(self):
        args = argparse.Namespace(timeout=10, max_requests=1, max_estimated_usd=25, input_usd_per_million=5, output_usd_per_million=25)
        budget = Budget(args)
        reason, price = budget.admit(400000)
        self.assertIsNone(reason)
        self.assertGreater(price, 1)
        self.assertEqual(budget.admit(1)[0], "request_budget")
        budget = Budget(args)
        budget.max_usd = .001
        self.assertEqual(budget.admit(1)[0], "rough_cost_budget")
        budget = Budget(args)
        budget.deadline = time.monotonic() - 1
        self.assertEqual(budget.admit(1)[0], "time_budget")


if __name__ == "__main__":
    unittest.main()
