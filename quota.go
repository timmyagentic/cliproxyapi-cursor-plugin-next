package main

import (
	"encoding/base64"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type onDemand struct {
	kind        string
	used, limit float64
	mine        *float64
}

func demandOf(data, hard map[string]any) onDemand {
	s := object(data["spendLimitUsage"])
	used := num(s["individualUsed"]) / 100
	own := onDemand{kind: "unavailable", used: used}
	if raw, ok := s["individualLimit"]; ok {
		own.kind = "disabled"
		if num(raw) > 0 {
			own.kind = "fixed"
			own.limit = num(raw) / 100
		}
	}
	if str(s["limitType"]) == "team" {
		if own.kind != "unavailable" {
			return own
		}
		if hard != nil && (hard["noUsageBasedAllowed"] == true || num(hard["hardLimit"]) <= 0) {
			return onDemand{kind: "disabled", used: used}
		}
		if limit := num(s["pooledLimit"]); limit > 0 {
			mine := used
			return onDemand{kind: "fixed", used: num(s["pooledUsed"]) / 100, limit: limit / 100, mine: &mine}
		}
		if hard != nil {
			return onDemand{kind: "unlimited", used: used}
		}
		return own
	}
	if hard == nil {
		return own
	}
	if hard["noUsageBasedAllowed"] == true {
		return onDemand{kind: "disabled", used: used}
	}
	limit := num(hard["hardLimit"])
	if limit >= 2147483647 {
		return onDemand{kind: "unlimited", used: used}
	}
	if limit > 0 {
		return onDemand{kind: "fixed", used: used, limit: limit}
	}
	return onDemand{kind: "disabled", used: used}
}
func normalizedQuota(data, hard, plan map[string]any) (map[string]any, error) {
	result := map[string]any{}
	name := str(object(plan["planInfo"])["planName"])
	if name != "" {
		result["subscription"] = map[string]any{"plan": name}
	}
	end := num(data["billingCycleEnd"])
	reset := ""
	if end > 0 {
		reset = time.UnixMilli(int64(end)).UTC().Format(time.RFC3339)
	}
	demand := demandOf(data, hard)
	buckets := []any{}
	summary := []any{}
	addBucket := func(name string, used float64) {
		if math.IsNaN(used) || math.IsInf(used, 0) {
			return
		}
		bucket := map[string]any{"window": name, "remainingFraction": math.Max(0, math.Min(1, 1-used/100))}
		if reset != "" {
			bucket["resetTime"] = reset
		}
		// CPA panels may use description as the row title ahead of window.
		// Keep the quota name in window without an overriding prose description.
		buckets = append(buckets, bucket)
	}
	u := object(data["planUsage"])
	for _, entry := range []struct{ name, key string }{{"Cursor Models", "autoPercentUsed"}, {"Other Models", "apiPercentUsed"}, {"Total", "totalPercentUsed"}} {
		if raw, ok := u[entry.key]; ok && raw != nil {
			addBucket(entry.name, num(raw))
		}
	}
	metric := func(key, label string, value float64) {
		summary = append(summary, map[string]any{"key": key, "label": label, "value": value, "format": "currency", "currency": "USD"})
	}
	switch demand.kind {
	case "fixed":
		addBucket("On-demand", 100*demand.used/demand.limit)
		metric("on_demand_spend", "On-demand spend", demand.used)
		metric("on_demand_limit", "On-demand limit", demand.limit)
		if demand.mine != nil {
			metric("on_demand_individual_spend", "Your share of team spending", *demand.mine)
		}
	case "unlimited":
		metric("on_demand_spend", "On-demand spend (no fixed limit)", demand.used)
	}
	if len(buckets) > 0 {
		result["groups"] = []any{map[string]any{"displayName": "Cursor subscription", "buckets": buckets}}
	}
	if len(summary) > 0 {
		result["summary"] = summary
	}
	if len(result) == 0 {
		return nil, failure(502, "Cursor returned no quota data")
	}
	return result, nil
}
func (p *pluginRuntime) quota(s *requestScope, req rpcRequest) (any, error) {
	token, err := p.token(s, req)
	if err != nil {
		return nil, err
	}
	var hard, plan map[string]any
	hardDone, planDone := make(chan struct{}), make(chan struct{})
	go func() {
		hard, _ = s.json(s.ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetHardLimit", token, map[string]any{})
		close(hardDone)
	}()
	go func() {
		plan, _ = s.json(s.ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetPlanInfo", token, map[string]any{})
		close(planDone)
	}()
	data, err := s.json(s.ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetCurrentPeriodUsage", token, map[string]any{})
	<-hardDone
	<-planDone
	if err != nil {
		return nil, err
	}
	result, err := normalizedQuota(data, hard, plan)
	if err != nil {
		return nil, err
	}
	if bot := p.botQuota(s, token); bot != nil {
		groups, _ := result["groups"].([]any)
		result["groups"] = append(groups, map[string]any{"displayName": "Grok Bot (informational)", "buckets": []any{bot}})
	}
	return result, nil
}
func (p *pluginRuntime) botQuota(s *requestScope, token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	claims, _ := jsonObject(raw)
	subject := str(claims["sub"])
	pieces := strings.Split(subject, "|")
	user := pieces[len(pieces)-1]
	if user == "" || strings.IndexFunc(user, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
	}) >= 0 {
		return nil
	}
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}, "Origin": {"https://cursor.com"}, "Cookie": {"WorkosCursorSessionToken=" + url.QueryEscape(user+"::"+token)}}
	resp, err := s.do(s.ctx, http.MethodPost, "https://cursor.com/api/dashboard/get-sand-usage-status", h, []byte(`{}`))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	data, err := jsonObject(b)
	if err != nil {
		return nil
	}
	paid := data["hasNonZeroIncludedLimit"] == true
	if zero, ok := data["includedLimitZero"].(bool); ok {
		paid = !zero
	}
	trialUntil, _ := time.Parse(time.RFC3339, str(data["sandTrialExpiresAt"]))
	trial := !paid && trialUntil.After(p.now())
	if !paid && !trial {
		return nil
	}
	used, ok := data["usagePercent"]
	if !ok {
		return nil
	}
	name := "Grok Bot"
	if trial {
		name += " (trial)"
	}
	bucket := map[string]any{"window": name, "remainingFraction": math.Max(0, math.Min(1, 1-num(used)/100))}
	if reset, err := time.Parse(time.RFC3339, str(data["nextResetTimestampUtc"])); err == nil && paid {
		bucket["resetTime"] = reset.UTC().Format(time.RFC3339)
	}
	return bucket
}
