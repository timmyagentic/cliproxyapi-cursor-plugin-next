package main

// Native protocol adaptation: magpie-community/plugins Cursor 0.2.23 (MIT),
// Copyright (c) 2026 magpie-community contributors. The original license is
// retained at reference/node-v0.2.0/vendor/cursor/LICENSE.

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func nativeExecClose(id uint64) []byte {
	return protobuf(nil).bytes(5, protobuf(nil).bytes(1, protobuf(nil).uint(1, id)))
}

func nativeRequire(fs []pbField, n, wire int, required bool) error {
	seen := false
	for _, f := range fs {
		if f.number == n {
			if seen || f.wire != wire {
				return failure(502, "invalid native protobuf field %d", n)
			}
			seen = true
		}
	}
	if required && !seen {
		return failure(502, "missing native protobuf field %d", n)
	}
	return nil
}

func nativeString(fs []pbField, n int) (string, error) {
	if err := nativeRequire(fs, n, 2, false); err != nil {
		return "", err
	}
	v := pbText(fs, n)
	if !utf8.ValidString(v) {
		return "", failure(502, "invalid UTF-8 in native protocol")
	}
	return v, nil
}

func nativeMCPArguments(data []byte, limit int) ([]pbField, map[string]any, error) {
	if len(data) > limit {
		return nil, nil, failure(502, "native MCP argument byte limit exceeded")
	}
	fs, err := pbFields(data)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range []int{1, 3, 4, 5, 6, 9} {
		if err = nativeRequire(fs, n, 2, false); err != nil {
			return nil, nil, err
		}
	}
	if err = nativeRequire(fs, 7, 0, false); err != nil {
		return nil, nil, err
	}
	args := map[string]any{}
	for _, f := range fs {
		switch f.number {
		case 1, 3, 4, 5, 9:
			if f.wire != 2 || !utf8.Valid(f.data) {
				return nil, nil, failure(502, "invalid native MCP string field")
			}
		case 2:
			if f.wire != 2 {
				return nil, nil, failure(502, "invalid native MCP argument entry")
			}
			kv, e := pbFields(f.data)
			if e != nil {
				return nil, nil, e
			}
			if e = nativeRequire(kv, 1, 2, true); e != nil {
				return nil, nil, e
			}
			if e = nativeRequire(kv, 2, 2, true); e != nil {
				return nil, nil, e
			}
			key, e := nativeString(kv, 1)
			if e != nil {
				return nil, nil, e
			}
			if _, exists := args[key]; exists {
				return nil, nil, failure(502, "duplicate native MCP argument key")
			}
			val, e := pbAny(pbGet(kv, 2).data)
			if e != nil {
				return nil, nil, e
			}
			args[key] = val
		case 6:
			if f.wire != 2 {
				return nil, nil, failure(502, "invalid native MCP file field")
			}
		case 7:
			if f.wire != 0 {
				return nil, nil, failure(502, "invalid native MCP background flag")
			}
		}
	}
	return fs, args, nil
}

func (s *nativeSession) execLocked(data []byte) ([][]byte, error) {
	fs, err := pbFields(data)
	if err != nil {
		return nil, err
	}
	// proto3 omits a uint request ID of zero. Missing is the valid default;
	// duplicate IDs and IDs encoded with another wire type remain invalid.
	if err = nativeRequire(fs, 1, 0, false); err != nil {
		return nil, err
	}
	id := pbNum(fs, 1)
	execID, err := nativeString(fs, 15)
	if err != nil {
		return nil, err
	}
	d := digest(string(data))
	if prior, ok := s.seenExecs[id]; ok {
		if prior != d {
			return nil, failure(502, "native exec identity changed during retransmission")
		}
		return s.internalReplies[id], nil
	}
	s.seenExecs[id] = d
	answer := func(field int, value []byte) [][]byte {
		return [][]byte{protobuf(nil).bytes(2, protobuf(nil).uint(1, id).text(15, execID).bytes(field, value)), nativeExecClose(id)}
	}
	unavailable := func() [][]byte {
		return [][]byte{protobuf(nil).bytes(5, protobuf(nil).bytes(2, protobuf(nil).uint(1, id).text(2, "not available"))), nativeExecClose(id)}
	}
	var replies [][]byte
	counted := true
	for _, f := range fs {
		switch f.number {
		case 11:
			if f.wire != 2 {
				return nil, failure(502, "invalid native MCP exec field")
			}
			af, args, e := nativeMCPArguments(f.data, s.limits.ArgumentBytes)
			if e != nil {
				return nil, e
			}
			name, e := nativeString(af, 5)
			if e != nil {
				return nil, e
			}
			if name == "" {
				name = strings.TrimPrefix(pbText(af, 1), "magpie-")
			}
			allowed := false
			for _, t := range s.tools {
				if t.Name == name {
					allowed = true
				}
			}
			provider, server := pbText(af, 4), pbText(af, 9)
			if !allowed || provider != "" && provider != "magpie" || server != "" && server != "magpie" || pbGet(af, 6) != nil || pbNum(af, 7) != 0 {
				replies = unavailable()
				break
			}
			if s.state == "parked" || s.state == "finishing" || s.state == "complete" {
				return nil, failure(502, "native tool call arrived outside a running segment")
			}
			nativeID := pbText(af, 3)
			if prior, ok := s.nativeIDs[nativeID]; nativeID != "" && ok && prior != id {
				return nil, failure(502, "native tool-call identity reused for another exec")
			}
			c := &nativeCall{ID: fmt.Sprintf("call_cursor_%s_%d_%d", s.identity, s.turn, len(s.segment.calls)), Name: name, Args: string(encodeJSON(args)), ExecID: execID, NativeID: nativeID, Digest: d, RequestID: id}
			if err = s.retainLocked(len(c.Args) + len(c.ID) + len(c.Name) + len(data)); err != nil {
				return nil, err
			}
			s.execs[id] = c
			if nativeID != "" {
				s.nativeIDs[nativeID] = id
			}
			s.segment.calls = append(s.segment.calls, c)
			s.resetBatchLocked()
			return nil, nil
		case 36:
			if f.wire != 2 {
				return nil, failure(502, "invalid native tool-list exec")
			}
			server := protobuf(nil).text(1, "magpie").text(2, "magpie").text(7, "connected")
			for _, t := range s.tools {
				server = server.bytes(5, toolDefinition(t))
			}
			replies = answer(36, protobuf(nil).bytes(1, protobuf(nil).bytes(1, server)))
		case 10:
			if f.wire != 2 {
				return nil, failure(502, "invalid native context exec")
			}
			replies = answer(10, protobuf(nil).bytes(1, protobuf(nil).bytes(1, protobuf(nil).bytes(4, cursorEnvironment()))))
			counted = false
		}
	}
	if replies == nil {
		replies = unavailable()
	}
	retained := len(data) + 64
	for _, reply := range replies {
		retained += len(reply)
	}
	if err = s.retainLocked(retained); err != nil {
		return nil, err
	}
	s.internalReplies[id] = replies
	if counted {
		if s.state == "parked" {
			return nil, failure(502, "new native tool exec arrived after its batch was sealed")
		}
		s.segment.answered++
		s.internalCalls++
		if s.batchTimer != nil {
			s.resetBatchLocked()
		}
	}
	return replies, nil
}

func (s *nativeSession) resetBatchLocked() {
	if s.batchTimer != nil {
		s.batchTimer.Stop()
	}
	seg := s.segment
	s.batchGeneration++
	generation := s.batchGeneration
	s.batchTimer = time.AfterFunc(s.limits.BatchWait, func() {
		s.mu.Lock()
		calls, want := len(seg.calls), seg.expected
		s.mu.Unlock()
		s.finishIf(failure(502, "native tool batch incomplete: %d of %d calls", calls, want), runEvent{}, func() bool {
			return s.batchGeneration == generation && s.batchTimer != nil && s.segment == seg && !seg.closed && (s.state == "running" || s.state == "submitting")
		})
	})
}

func (s *nativeSession) sealLocked() error {
	seg := s.segment
	if seg.expected == 0 || seg.closed {
		return nil
	}
	n := len(seg.calls) + seg.answered
	if n > seg.expected {
		return failure(502, "native tool batch exceeded its declared count")
	}
	if n != seg.expected {
		return nil
	}
	if s.batchTimer != nil {
		s.batchTimer.Stop()
		s.batchTimer = nil
	}
	if len(seg.calls) == 0 {
		seg.expected = 0
		seg.answered = 0
		s.internalSteps++
		return nil
	}
	s.state = "parked"
	s.heard = 0
	// Register before exposing the handoff. Pool operations never hold their
	// lock while acquiring a session lock, so this lock order is one-way.
	s.pool.mu.Lock()
	if s.pool.entries[s.receipt.RunID] == s {
		for _, c := range seg.calls {
			s.pool.handles[c.ID] = seg
		}
	}
	s.pool.mu.Unlock()
	for i, c := range seg.calls {
		if err := s.emitLocked(runEvent{tool: map[string]any{"index": i, "id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Args}}}); err != nil {
			return err
		}
	}
	if err := s.emitLocked(runEvent{stop: "tool_calls", internalCalls: s.internalCalls, internalSteps: s.internalSteps}); err != nil {
		return err
	}
	seg.closed = true
	s.wakeLocked(seg)
	seg.deadline = time.Now().Add(s.limits.PendingTTL)
	s.pendingTimer = time.AfterFunc(s.limits.PendingTTL, func() {
		s.finishIf(failure(409, "native tool-result deadline expired"), runEvent{}, func() bool { return s.state == "parked" && s.segment == seg && seg.resultDigest == "" })
	})
	return nil
}

func (s *nativeSession) storeBlobLocked(id, data []byte) error {
	if len(id) == 0 {
		return failure(502, "native blob ID is empty")
	}
	key := hex.EncodeToString(id)
	if s.bootstrap[key] && digest(string(data)) != key {
		return failure(502, "native content-addressed bootstrap blob changed")
	}
	size := s.blobBytes - len(s.blobs[key]) + len(data)
	if size+len(s.checkpoint) > s.limits.BlobBytes {
		return failure(503, "native blob retention limit exceeded")
	}
	s.blobs[key] = append([]byte(nil), data...)
	s.blobBytes = size
	return nil
}

func (s *nativeSession) handleFrame(data []byte) (bool, error) {
	fs, err := pbFields(data)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if s.state == "finishing" || s.state == "failed" || s.state == "complete" {
		s.mu.Unlock()
		return false, s.ctx.Err()
	}
	replies := [][]byte{}
	terminal := false
	heartbeatOnly := len(fs) > 0
	process := func() error {
		for _, m := range fs {
			switch m.number {
			case 1, 2, 3, 4, 5, 7:
			default:
				heartbeatOnly = false
				continue
			}
			if m.wire != 2 {
				return failure(502, "invalid native server message field %d wire %d length %d", m.number, m.wire, len(m.data))
			}
			switch m.number {
			case 1:
				updates, e := pbFields(m.data)
				if e != nil {
					return e
				}
				if len(updates) == 0 {
					heartbeatOnly = false
				}
				for _, u := range updates {
					if u.number == 13 {
						// Cursor versions have used an empty message or scalar
						// heartbeat. It carries no payload we consume.
						if u.wire != 0 && u.wire != 2 {
							return failure(502, "invalid native heartbeat field 13 wire %d length %d", u.wire, len(u.data))
						}
						if s.batchTimer != nil && s.state != "parked" {
							s.resetBatchLocked()
						}
						continue
					}
					// Unknown fields can be scalars or opaque bytes. Their wire
					// envelope was validated by pbFields; treating every future
					// field as an embedded message rejects valid proto evolution.
					switch u.number {
					case 1, 4, 14, 27:
					default:
						heartbeatOnly = false
						continue
					}
					if u.wire != 2 {
						return failure(502, "invalid native interaction update field %d wire %d length %d", u.number, u.wire, len(u.data))
					}
					uf, e := pbFields(u.data)
					if e != nil {
						return e
					}
					if u.number != 13 {
						heartbeatOnly = false
					}
					switch u.number {
					case 1, 4:
						if terminal || s.state == "parked" {
							return failure(502, "native text arrived outside a running segment")
						}
						text, e := nativeString(uf, 1)
						if e != nil {
							return e
						}
						if text != "" {
							ev := runEvent{}
							if u.number == 1 {
								ev.text = text
							} else {
								ev.reasoning = text
							}
							if e = s.emitLocked(ev); e != nil {
								return e
							}
							if s.batchTimer != nil {
								s.resetBatchLocked()
							}
						}
					case 14:
						if len(s.segment.calls) > 0 || s.state == "parked" {
							return failure(502, "Cursor ended a turn with unresolved native calls")
						}
						if s.segment.content == "" {
							return failure(502, "Cursor ended a native turn without text")
						}
						for _, f := range uf {
							if f.number >= 1 && f.number <= 5 && (f.wire != 0 || f.n > 1<<53-1) {
								return failure(502, "invalid native terminal usage")
							}
						}
						s.usage = usageFromFields(uf)
						for _, f := range uf {
							if f.number >= 1 && f.number <= 5 && s.usage == nil {
								return failure(502, "inconsistent native terminal usage")
							}
						}
						terminal = true
					case 27:
						if terminal {
							return failure(502, "native tool count arrived after TurnEnded")
						}
						if e = nativeRequire(uf, 1, 0, true); e != nil {
							return e
						}
						n := pbNum(uf, 1)
						if n == 0 || n > 100000 {
							return failure(502, "invalid native tool batch count")
						}
						if s.segment.expected != 0 && s.segment.expected != int(n) {
							return failure(502, "inconsistent native tool batch count")
						}
						s.segment.expected = int(n)
						if s.state != "parked" {
							s.resetBatchLocked()
						}
					}
				}
			case 2:
				heartbeatOnly = false
				if terminal {
					return failure(502, "native exec arrived after TurnEnded")
				}
				out, e := s.execLocked(m.data)
				replies = append(replies, out...)
				if e != nil {
					return e
				}
			case 3:
				heartbeatOnly = false
				if _, e := pbFields(m.data); e != nil {
					return e
				}
				if len(m.data)+s.blobBytes > s.limits.BlobBytes {
					return failure(503, "native checkpoint retention limit exceeded")
				}
				s.checkpoint = append([]byte(nil), m.data...)
			case 4:
				heartbeatOnly = false
				kv, e := pbFields(m.data)
				if e != nil {
					return e
				}
				if e = nativeRequire(kv, 1, 0, false); e != nil {
					return e
				}
				id := pbNum(kv, 1)
				for _, k := range kv {
					switch k.number {
					case 2:
						if k.wire != 2 {
							return failure(502, "invalid native blob get")
						}
						want, e := pbFields(k.data)
						if e != nil {
							return e
						}
						if e = nativeRequire(want, 1, 2, true); e != nil {
							return e
						}
						key := hex.EncodeToString(pbGet(want, 1).data)
						var res protobuf
						if b, ok := s.blobs[key]; ok {
							res = res.bytes(1, b)
						} else {
							res = res.bytes(2, protobuf(nil).text(1, "blob not found"))
						}
						replies = append(replies, protobuf(nil).bytes(3, protobuf(nil).uint(1, id).bytes(2, res)))
					case 3:
						if k.wire != 2 {
							return failure(502, "invalid native blob set")
						}
						put, e := pbFields(k.data)
						if e != nil {
							return e
						}
						if e = nativeRequire(put, 1, 2, true); e != nil {
							return e
						}
						if e = nativeRequire(put, 2, 2, false); e != nil {
							return e
						}
						var value []byte
						if f := pbGet(put, 2); f != nil {
							value = f.data
						}
						e = s.storeBlobLocked(pbGet(put, 1).data, value)
						var ack protobuf
						if e != nil {
							ack = ack.bytes(1, protobuf(nil).text(1, "native blob could not be retained"))
						}
						replies = append(replies, protobuf(nil).bytes(3, protobuf(nil).uint(1, id).bytes(3, ack)))
						if e != nil {
							return e
						}
					}
				}
			case 5:
				heartbeatOnly = false
				controls, e := pbFields(m.data)
				if e != nil {
					return e
				}
				for _, c := range controls {
					if c.number == 1 {
						if c.wire != 2 {
							return failure(502, "invalid native exec cancellation")
						}
						cf, e := pbFields(c.data)
						if e != nil {
							return e
						}
						if e = nativeRequire(cf, 1, 0, false); e != nil {
							return e
						}
						if call := s.execs[pbNum(cf, 1)]; call != nil && !call.submitted {
							return failure(409, "Cursor cancelled a pending native exec; late results cannot be submitted")
						}
					}
				}
			case 7:
				return failure(502, "unsupported native interaction query")
			default:
				heartbeatOnly = false
			}
			if e := s.sealLocked(); e != nil {
				return e
			}
		}
		return nil
	}
	err = process()
	if !heartbeatOnly {
		s.heard = 0
	}
	s.mu.Unlock()
	for _, reply := range replies {
		if e := s.send(reply); e != nil && err == nil {
			err = e
		}
	}
	return terminal, err
}

func nativeEncodeResult(m map[string]any, limits nativeLimits) ([]byte, error) {
	var parts []any
	switch v := m["content"].(type) {
	case string:
		parts = []any{map[string]any{"type": "text", "text": v}}
	case []any:
		parts = v
	default:
		return nil, failure(409, "unsupported native tool result content")
	}
	var success protobuf
	size := 0
	for _, raw := range parts {
		p := object(raw)
		if str(p["type"]) == "text" {
			text, ok := p["text"].(string)
			if !ok || !utf8.ValidString(text) {
				return nil, failure(409, "invalid native tool text")
			}
			size += len(text)
			success = success.bytes(1, protobuf(nil).bytes(1, protobuf(nil).text(1, text)))
		} else {
			url := ""
			if str(p["type"]) == "image_url" {
				url = str(p["image_url"])
				if url == "" {
					url = str(object(p["image_url"])["url"])
				}
			} else if str(p["type"]) == "image" {
				src := object(p["source"])
				if str(src["type"]) == "base64" {
					url = "data:" + str(src["media_type"]) + ";base64," + str(src["data"])
				}
			}
			at := strings.Index(url, ";base64,")
			if !strings.HasPrefix(url, "data:") || at < 5 {
				return nil, failure(409, "unsupported native tool image")
			}
			mime := url[5:at]
			if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" && mime != "image/webp" {
				return nil, failure(409, "unsupported native tool image type")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(url[at+8:])
			if err != nil || len(data) == 0 {
				return nil, failure(409, "invalid native tool image")
			}
			size += len(data)
			success = success.bytes(1, protobuf(nil).bytes(2, protobuf(nil).bytes(1, data).text(2, mime)))
		}
		if size > limits.RetainedBytes {
			return nil, failure(409, "native tool result byte limit exceeded")
		}
	}
	for _, key := range []string{"is_error", "isError"} {
		if v, ok := m[key]; ok {
			b, valid := v.(bool)
			if !valid {
				return nil, failure(409, "invalid native tool result error flag")
			}
			if b {
				success = success.uint(2, 1)
				break
			}
		}
	}
	return protobuf(nil).bytes(1, success), nil
}
