package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This fixture speaks the real native protocol through dispatch and the host
// HTTP callbacks. It asks for transcript blobs, validates actual caller-run
// Go tool results, and only then emits the next batch or final answer.
type accountingFixture struct {
	mu        sync.Mutex
	runs      map[string]*accountingFixtureRun
	count     int
	events    []map[string]any
	lateFirst bool
	now       func() time.Time
}

type accountingFixtureRun struct {
	number  int
	queue   chan []byte
	refs    map[uint64]bool
	history map[uint64][]byte
	cid     string
	stamp   time.Time
	replied bool
}

func (f *accountingFixture) run(id string) *accountingFixtureRun {
	r := f.runs[id]
	if r == nil {
		f.count++
		r = &accountingFixtureRun{number: f.count, queue: make(chan []byte, 32), refs: map[uint64]bool{}, history: map[uint64][]byte{}, stamp: f.now()}
		f.runs[id] = r
	}
	return r
}

func accountingToolFrame(number uint64, id, name string, a, b float64) []byte {
	args := protobuf(nil).text(1, "magpie-"+name)
	for _, pair := range []struct {
		key   string
		value float64
	}{{"a", a}, {"b", b}} {
		args = args.bytes(2, protobuf(nil).text(1, pair.key).bytes(2, pbValue(pair.value)))
	}
	args = args.text(3, id).text(4, "magpie").text(5, name).text(9, "magpie")
	return connectFrame(protobuf(nil).bytes(2, protobuf(nil).uint(1, number).text(15, fmt.Sprintf("synthetic-exec-%d", number)).bytes(11, args)))
}

func (f *accountingFixture) respond(_ context.Context, req mockRequest) (mockResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(req.URL, "AvailableModels") {
		return mockResponse{Status: 200, Body: []byte(`{"models":[{"name":"grok-4.7","contextTokenLimit":243200}]}`)}, nil
	}
	if strings.HasSuffix(req.URL, "GetFilteredUsageEvents") {
		events := []any{}
		for n, event := range f.events {
			if f.lateFirst && n == 0 && f.count < 2 {
				continue
			}
			events = append(events, event)
		}
		return mockResponse{Status: 200, Body: encodeJSON(map[string]any{"usageEventsDisplay": events, "totalUsageEventsCount": len(events)})}, nil
	}
	if !strings.HasSuffix(req.URL, "RunSSE") && !strings.HasSuffix(req.URL, "BidiAppend") {
		return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
	}
	r := f.run(req.Headers.Get("X-Request-Id"))
	if strings.HasSuffix(req.URL, "RunSSE") {
		return mockResponse{Status: 200, Read: func(ctx context.Context) ([]byte, bool, error) {
			select {
			case payload := <-r.queue:
				return payload, false, nil
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}}, nil
	}
	appendFields, err := pbFields(req.Body)
	if err != nil {
		return mockResponse{}, err
	}
	wire, err := hex.DecodeString(pbText(appendFields, 1))
	if err != nil {
		return mockResponse{}, err
	}
	fields, err := pbFields(wire)
	if err != nil {
		return mockResponse{}, err
	}
	if first := pbGet(fields, 1); first != nil {
		runFields, err := pbFields(first.data)
		if err != nil {
			return mockResponse{}, err
		}
		r.cid = pbText(runFields, 5)
		state, err := fixtureFields(first.data, 1)
		if err != nil {
			return mockResponse{}, err
		}
		refs, err := pbFields(state)
		if err != nil {
			return mockResponse{}, err
		}
		index := uint64(1000)
		for _, ref := range refs {
			if ref.number != 1 {
				continue
			}
			r.refs[index] = true
			r.queue <- connectFrame(protobuf(nil).bytes(4, protobuf(nil).uint(1, index).bytes(2, protobuf(nil).bytes(1, ref.data))))
			index++
		}
	} else if reply := pbGet(fields, 3); reply != nil {
		kv, err := pbFields(reply.data)
		if err != nil {
			return mockResponse{}, err
		}
		id := pbNum(kv, 1)
		if r.refs[id] {
			content, err := fixtureFields(reply.data, 2)
			if err != nil {
				return mockResponse{}, err
			}
			blob, err := fixtureFields(content, 1)
			if err != nil {
				return mockResponse{}, err
			}
			r.history[id] = blob
			if len(r.history) == len(r.refs) && !r.replied {
				r.replied = true
				if err = f.reply(r); err != nil {
					return mockResponse{}, err
				}
			}
		}
	}
	return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
}

func (f *accountingFixture) reply(r *accountingFixtureRun) error {
	results := map[string]float64{}
	for _, raw := range r.history {
		message, err := jsonObject(raw)
		if err != nil {
			return err
		}
		if str(message["role"]) != "tool" {
			continue
		}
		for _, value := range array(message["content"]) {
			part := object(value)
			if part["isError"] == true {
				return fmt.Errorf("synthetic tool unexpectedly failed")
			}
			result, ok := part["result"].(float64)
			if !ok {
				return fmt.Errorf("synthetic tool did not return a numeric calculation")
			}
			results[str(part["toolCallId"])] = result
		}
	}
	if r.number >= 2 && (results["call_synthetic_add"] != 42 || results["call_synthetic_multiply"] != 54) {
		return fmt.Errorf("next Run did not receive both actual first-round tool results")
	}
	if r.number == 3 && results["call_synthetic_subtract"] != 12 {
		return fmt.Errorf("final Run did not receive the actual second-round result")
	}
	switch r.number {
	case 1:
		r.queue <- append(append(connectFrame(protobuf(nil).bytes(1, protobuf(nil).bytes(27, protobuf(nil).uint(1, 2)))), accountingToolFrame(1, "call_synthetic_add", "add", 19, 23)...), accountingToolFrame(2, "call_synthetic_multiply", "multiply", 6, 9)...)
	case 2:
		r.queue <- append(connectFrame(protobuf(nil).bytes(1, protobuf(nil).bytes(27, protobuf(nil).uint(1, 1)))), accountingToolFrame(3, "call_synthetic_subtract", "subtract", results["call_synthetic_multiply"], results["call_synthetic_add"])...)
	case 3:
		text := connectFrame(protobuf(nil).bytes(1, protobuf(nil).bytes(1, protobuf(nil).text(1, "The verified difference is 12."))))
		usage := protobuf(nil).uint(1, 142000).uint(2, 47).uint(3, 141800).uint(4, 100).uint(5, 11)
		r.queue <- append(text, connectFrame(protobuf(nil).bytes(1, protobuf(nil).bytes(14, usage)))...)
	default:
		return fmt.Errorf("synthetic workflow issued an unexpected extra Run")
	}
	if r.number <= 2 {
		f.events = append(f.events, map[string]any{
			"conversationId": r.cid, "model": "grok-4.7-high", "timestamp": strconv.FormatInt(r.stamp.UnixMilli(), 10),
			"tokenUsage": map[string]any{"inputTokens": 100, "cacheReadTokens": 140160, "cacheWriteTokens": 0, "outputTokens": 669, "reasoningTokens": 107},
		})
	}
	return nil
}

func accountingTools() []map[string]any {
	var tools []map[string]any
	for _, name := range []string{"add", "multiply", "subtract"} {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": "Perform the named synthetic arithmetic operation.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}, "required": []any{"a", "b"}, "additionalProperties": false},
		}})
	}
	return tools
}

func executeAccountingRound(t *testing.T, p *pluginRuntime, host *mockHost, stream bool, messages []map[string]any, tools []map[string]any) map[string]any {
	t.Helper()
	req := map[string]any{
		"host_callback_id": "synthetic-accounting-context", "stream_id": "synthetic-accounting-stream",
		"StorageJSON": []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"synthetic-accounting-token","expires":9999999999999}}`),
		"Model":       "cursor/grok-4.7", "Format": "openai", "AuthID": "synthetic-accounting-account",
		"Metadata": map[string]any{"canonical_session_id": "synthetic-accounting-session"},
		"Payload":  encodeJSON(map[string]any{"messages": messages, "tools": tools}),
	}
	method := "executor.execute"
	if !stream {
		result := invokeNative(t, p, method, req, host.call)
		if result["ok"] != true {
			t.Fatalf("synthetic nonstream execution failed: %v", result)
		}
		raw, err := base64.StdEncoding.DecodeString(str(object(result["result"])["Payload"]))
		if err != nil {
			t.Fatal(err)
		}
		body, err := jsonObject(raw)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	method = "executor.execute_stream"
	var mu sync.Mutex
	var chunks [][]byte
	closed := make(chan struct{})
	var once sync.Once
	caller := func(method string, raw []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var fields struct{ Payload []byte }
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, err
			}
			mu.Lock()
			chunks = append(chunks, fields.Payload)
			mu.Unlock()
			return hostResult(map[string]any{}), nil
		case "host.stream.close":
			once.Do(func() { close(closed) })
			return hostResult(map[string]any{}), nil
		}
		return host.call(method, raw)
	}
	result := invokeNative(t, p, method, req, caller)
	if result["ok"] != true {
		t.Fatalf("synthetic stream setup failed: %v", result)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic stream did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	message := map[string]any{"role": "assistant", "content": ""}
	body := map[string]any{}
	var calls []any
	stop, sawDone := "", false
	for _, chunk := range chunks {
		data := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data: "))
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		value, err := jsonObject([]byte(data))
		if err != nil || value["error"] != nil {
			t.Fatal("synthetic stream contained an error or invalid JSON")
		}
		choice := object(array(value["choices"])[0])
		delta := object(choice["delta"])
		message["content"] = str(message["content"]) + str(delta["content"])
		for _, rawCall := range array(delta["tool_calls"]) {
			call := copyMap(object(rawCall))
			delete(call, "index")
			calls = append(calls, call)
		}
		if finish := str(choice["finish_reason"]); finish != "" {
			stop = finish
		}
		for _, key := range []string{"usage", "cursor_usage"} {
			if value[key] != nil {
				body[key] = value[key]
			}
		}
	}
	if !sawDone || stop == "" {
		t.Fatal("synthetic stream omitted its final stop or DONE marker")
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	body["choices"] = []any{map[string]any{"message": message, "finish_reason": stop}}
	return body
}

func runAccountingTools(t *testing.T, message map[string]any) []map[string]any {
	t.Helper()
	var results []map[string]any
	for _, raw := range array(message["tool_calls"]) {
		call := object(raw)
		function := object(call["function"])
		args, err := jsonObject([]byte(str(function["arguments"])))
		if err != nil {
			t.Fatal(err)
		}
		a, b := num(args["a"]), num(args["b"])
		var result float64
		switch str(function["name"]) {
		case "add":
			result = a + b
		case "multiply":
			result = a * b
		case "subtract":
			result = a - b
		default:
			t.Fatal("synthetic workflow requested an unauthorized tool")
		}
		results = append(results, map[string]any{"role": "tool", "tool_call_id": str(call["id"]), "content": strconv.FormatFloat(result, 'f', -1, 64)})
	}
	return results
}

func TestAccountingDispatchMultiToolMultiRoundMeasuredUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/late-first=%v", stream, late), func(t *testing.T) {
				t.Setenv("CURSOR_USAGE_JOURNAL", "")
				p := newTestRuntime(t)
				p.usagePolicy = stepUsagePolicy{Wait: 12 * time.Millisecond, First: 0, Interval: time.Millisecond, Ask: 5 * time.Millisecond, Skew: time.Millisecond, PageSize: 100, MaxPages: 2}
				base := time.Now().Add(-time.Hour).Truncate(time.Second)
				var stamp atomic.Int64
				stamp.Store(base.UnixMilli())
				p.now = func() time.Time { return time.UnixMilli(stamp.Load()) }
				host := newMockHost(t)
				fixture := &accountingFixture{runs: map[string]*accountingFixtureRun{}, lateFirst: late, now: p.now}
				host.respond = fixture.respond
				tools := accountingTools()
				user := map[string]any{"role": "user", "content": "Add 19 and 23, multiply 6 and 9, subtract the sum from the product, then state the result."}
				chat := chatRequest{Messages: []map[string]any{user}, Tools: tools}
				conv, err := conversation(chat, toolsOf(chat))
				if err != nil {
					t.Fatal(err)
				}
				// Reproduce the misleading former input estimate without treating
				// byte count as a token oracle. Dashboard input is independently set.
				padding := strings.Repeat("S", 4*(236373-conv.Estimate))
				messages := []map[string]any{{"role": "system", "content": padding}, user}
				chat.Messages = messages
				conv, err = conversation(chat, toolsOf(chat))
				if err != nil || conv.Estimate != 236373 {
					t.Fatal("synthetic regression did not reproduce the old 236373 input estimate")
				}
				for round := 1; round <= 3; round++ {
					stamp.Store(base.Add(time.Duration(round-1) * 10 * time.Second).UnixMilli())
					body := executeAccountingRound(t, p, host, stream, messages, tools)
					usage := object(body["usage"])
					if round == 1 && late {
						if body["usage"] != nil || str(object(body["cursor_usage"])["state"]) != "unknown" {
							t.Fatal("missing authoritative usage became an estimate or exact zero")
						}
					} else {
						input, output, cached, write, reasoning := float64(140260), float64(669), float64(140160), float64(0), float64(107)
						if round == 3 {
							input, output, cached, write, reasoning = 142000, 47, 141800, 100, 11
						}
						if num(usage["prompt_tokens"]) != input || num(usage["completion_tokens"]) != output || num(usage["total_tokens"]) != input+output || num(object(usage["prompt_tokens_details"])["cached_tokens"]) != cached || num(object(usage["prompt_tokens_details"])["cache_write_tokens"]) != write || num(object(usage["completion_tokens_details"])["reasoning_tokens"]) != reasoning {
							t.Fatalf("round %d lost independent measured token semantics: %v", round, usage)
						}
						if usage["cursor_usage_estimated"] != nil || usage["cursor_usage_event_id"] != nil || usage["cursor_usage_timestamp"] != nil || usage["magpie_deferred"] != nil {
							t.Fatal("standard usage leaked estimates, event identities, or old-round counters")
						}
					}
					choice := object(array(body["choices"])[0])
					message := object(choice["message"])
					if round <= 2 {
						wantCalls := 3 - round
						if str(choice["finish_reason"]) != "tool_calls" || len(array(message["tool_calls"])) != wantCalls {
							t.Fatal("multi-tool workflow lost a call or tool boundary")
						}
						messages = append(messages, message)
						messages = append(messages, runAccountingTools(t, message)...)
					} else if str(choice["finish_reason"]) != "stop" || str(message["content"]) != "The verified difference is 12." {
						t.Fatal("final response did not use the actual computed tool results")
					}
				}
				records := p.journal.Records()
				if len(records) != 3 || records[0].Usage == nil || records[1].Usage == nil || records[2].Usage == nil || records[0].Usage.PromptTokens != 140260 || records[1].Usage.PromptTokens != 140260 || records[2].Usage.PromptTokens != 142000 {
					t.Fatal("journal double counted, lost, or aggregated independent Run measurements")
				}
				wantStatus := "measured"
				if late {
					wantStatus = "deferred"
				}
				if records[0].Status != wantStatus || records[1].Status != "measured" || records[2].Status != "measured" || records[0].Source != "dashboard" || records[2].Source != "turn_ended" {
					t.Fatal("journal mislabeled current versus deferred measurements")
				}
				if records[0].Conversation != records[1].Conversation || records[1].Conversation != records[2].Conversation {
					t.Fatal("conversation identity changed across legitimate tool continuation")
				}
				host.assertClosed(t)
			})
		}
	}
}

func TestAccountingConcurrentRunGatePreservesAccountAndConversationIsolation(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	accountA, accountB := digest("synthetic-account-a"), digest("synthetic-account-b")
	if err := p.acquireUsageRun(accountA, "conversation-a"); err != nil {
		t.Fatal(err)
	}
	defer p.releaseUsageRun(accountA, "conversation-a")
	if err := p.acquireUsageRun(accountA, "conversation-a"); errorStatus(err) != 409 {
		t.Fatal("simultaneous Runs in one account/conversation were not rejected")
	}
	for _, identity := range [][2]string{{accountA, "conversation-b"}, {accountB, "conversation-a"}} {
		if err := p.acquireUsageRun(identity[0], identity[1]); err != nil {
			t.Fatal("independent account or conversation was unnecessarily blocked")
		}
		p.releaseUsageRun(identity[0], identity[1])
	}
}

func TestAccountingAccountFingerprintSurvivesTokenRefreshAndAuthLabels(t *testing.T) {
	for _, auth := range []credential{
		{Type: "api", Key: "synthetic-fixed-api-key"},
		{Type: "oauth", Access: "synthetic-old-access", AccountID: "synthetic-fixed-account"},
		{Type: "oauth", Access: "synthetic-old-access", Refresh: "synthetic-fixed-refresh"},
	} {
		req := rpcRequest{AuthID: "synthetic-label-one", StorageJSON: encodeJSON(map[string]any{"type": "cursor", "cursor_auth": auth})}
		before := usageAccount(req, "synthetic-old-access")
		req.AuthID = "synthetic-label-two"
		auth.Access = "synthetic-new-access"
		req.StorageJSON = encodeJSON(map[string]any{"type": "cursor", "cursor_auth": auth})
		after := usageAccount(req, "synthetic-new-access")
		if !isUsageHash(before) || before != after {
			t.Fatal("credential exchange or label change broke account isolation identity")
		}
		chat := chatRequest{PromptCacheKey: "synthetic-shared-cache-key", Messages: []map[string]any{{"role": "user", "content": "Synthetic task"}}}
		identity := req
		identity.AuthID = before
		firstCID := conversationID(chat, identity)
		identity.AuthID = after
		if conversationID(chat, identity) != firstCID {
			t.Fatal("access token refresh changed the legitimate continuation identity")
		}
		identity.AuthID = digest("synthetic-other-account")
		if conversationID(chat, identity) == firstCID {
			t.Fatal("same cache key merged different accounts into one conversation")
		}
	}
}

func TestAccountingRejectsAmbiguousRunWindowWithoutLosingOtherSessions(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	account := digest("synthetic-window-account")
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	previous, err := p.journal.Begin(account, "synthetic-same-conversation", "previous-run", "grok-4.7", base)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.Close(previous.ID, base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = p.journal.MarkUnknown(previous.ID, "unavailable"); err != nil {
		t.Fatal(err)
	}
	measurement := map[string]any{"cursor_usage_timestamp": base.Add(2 * time.Second).UnixMilli()}
	if p.unambiguousUsage(account, "synthetic-same-conversation", "current-run", measurement) {
		t.Fatal("one candidate was labeled exact despite an overlapping unknown Run")
	}
	if !p.unambiguousUsage(account, "synthetic-another-conversation", "current-run", measurement) || !p.unambiguousUsage(digest("synthetic-another-account"), "synthetic-same-conversation", "current-run", measurement) {
		t.Fatal("unrelated account or conversation contaminated event matching")
	}
	measurement["cursor_usage_timestamp"] = base.Add(20 * time.Second).UnixMilli()
	if !p.unambiguousUsage(account, "synthetic-same-conversation", "current-run", measurement) {
		t.Fatal("a distinct subsequent Run remained ambiguous")
	}
	delete(measurement, "cursor_usage_timestamp")
	if p.unambiguousUsage(account, "synthetic-same-conversation", "current-run", measurement) {
		t.Fatal("missing event identity evidence became an exact count")
	}
}
