package main

import (
	"math"
	"testing"
)

func TestNativeQuotaWindowsAndNoFabricatedAllowance(t *testing.T) {
	data := map[string]any{"billingCycleEnd": "1793491200000", "planUsage": map[string]any{"autoPercentUsed": 25.0, "apiPercentUsed": 150.0, "totalPercentUsed": 40.0}}
	result, err := normalizedQuota(data, map[string]any{"noUsageBasedAllowed": true}, map[string]any{"planInfo": map[string]any{"planName": "Pro"}})
	if err != nil {
		t.Fatal(err)
	}
	groups := result["groups"].([]any)
	buckets := groups[0].(map[string]any)["buckets"].([]any)
	if buckets[0].(map[string]any)["remainingFraction"] != 0.75 || buckets[1].(map[string]any)["remainingFraction"] != float64(0) {
		t.Fatal(result)
	}
	if str(buckets[0].(map[string]any)["resetTime"]) != "2026-11-01T00:00:00Z" {
		t.Fatal("reset time lost")
	}
	empty, _ := normalizedQuota(map[string]any{}, nil, map[string]any{"planInfo": map[string]any{"planName": "Enterprise"}})
	if _, ok := empty["groups"]; ok {
		t.Fatal("unknown usage became a full allowance")
	}
	if _, err := normalizedQuota(nil, nil, nil); err == nil {
		t.Fatal("unknown empty quota succeeded")
	}
}
func TestNativeOnDemandAndTeamPool(t *testing.T) {
	cases := []struct {
		name        string
		data, hard  map[string]any
		kind        string
		used, limit float64
	}{
		{"personal", map[string]any{"spendLimitUsage": map[string]any{"individualUsed": 3000.0}}, map[string]any{"hardLimit": 100.0}, "fixed", 30, 100},
		{"unlimited", map[string]any{"spendLimitUsage": map[string]any{"individualUsed": 123456.0}}, map[string]any{"hardLimit": 2147483647.0}, "unlimited", 1234.56, 0},
		{"team pool", map[string]any{"spendLimitUsage": map[string]any{"limitType": "team", "pooledLimit": "50000", "pooledUsed": 20000.0, "individualUsed": 5000.0}}, map[string]any{"hardLimit": 1000.0}, "fixed", 200, 500},
		{"team individual limit", map[string]any{"spendLimitUsage": map[string]any{"limitType": "team", "individualLimit": "10000", "individualUsed": 5000.0}}, nil, "fixed", 50, 100},
		{"disabled", map[string]any{}, map[string]any{"noUsageBasedAllowed": true}, "disabled", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := demandOf(c.data, c.hard)
			if got.kind != c.kind || math.Abs(got.used-c.used) > 1e-9 || got.limit != c.limit {
				t.Fatalf("got %+v", got)
			}
		})
	}
	result, err := normalizedQuota(cases[1].data, cases[1].hard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["groups"]; ok {
		t.Fatal("unlimited spending became a fabricated remaining percentage")
	}
	if result["summary"].([]any)[0].(map[string]any)["currency"] != "USD" {
		t.Fatal(result)
	}
}

func TestQuotaRowTitlesAndTotal(t *testing.T) {
	for _, hard := range []map[string]any{
		{"noUsageBasedAllowed": true},
		{"hardLimit": 100.0},
		{"hardLimit": 2147483647.0},
	} {
		result, err := normalizedQuota(map[string]any{
			"planUsage": map[string]any{"autoPercentUsed": 78.0, "apiPercentUsed": 100.0, "totalPercentUsed": 80.0},
		}, hard, nil)
		if err != nil {
			t.Fatal(err)
		}
		buckets := result["groups"].([]any)[0].(map[string]any)["buckets"].([]any)
		want := []string{"Cursor Models", "Other Models", "Total"}
		for i, raw := range buckets {
			bucket := raw.(map[string]any)
			title := str(bucket["description"])
			if title == "" {
				title = str(bucket["window"])
			}
			expected := "On-demand"
			if i < len(want) {
				expected = want[i]
			}
			if title != expected {
				t.Fatalf("quota row title %q, want %q", title, expected)
			}
		}
		if math.Abs(num(buckets[2].(map[string]any)["remainingFraction"])-0.2) > 1e-9 {
			t.Fatal("total remaining fraction changed")
		}
	}
}
