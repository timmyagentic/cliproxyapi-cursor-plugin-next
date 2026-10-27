package main

// Native continuation design adapted from magpie-community/plugins Cursor
// 0.2.23, commit 9c886c12f92d0da504b9c54cf2dd9df623a61617 (MIT).
// Copyright (c) 2026 magpie-community contributors.
// The upstream license is preserved in reference/node-v0.2.0/vendor/cursor/LICENSE.
// This Go implementation deliberately rejects uncertain/stale continuations
// instead of resubmitting their results to a reconstructed native exec.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type nativeInput struct {
	Chat                            chatRequest
	Model                           modelResolution
	Tools                           []chatTool
	Conversation                    chatConversation
	CID, Account, Token, OwnerScope string
	SourceFormat                    string
}

type nativeReceipt struct {
	RunID, CID, RequestID, Account, ModelID, Protocol string
	Started                                           time.Time
	Resumed, Replay                                   bool
	Delivered                                         func() `json:"-"`
	token                                             string
}

type nativeLimits struct {
	PendingTTL, Lifetime, BatchWait, ReplayTTL, Idle, Quiet, ExecHeartbeat time.Duration
	PerAccount, PerProcess, RetainedBytes, BlobBytes, ArgumentBytes        int
}

func defaultNativeLimits() nativeLimits {
	return nativeLimits{120 * time.Second, 15 * time.Minute, 30 * time.Second, 120 * time.Second,
		60 * time.Second, 240 * time.Second, 3 * time.Second, 8, 32, 16 << 20, 64 << 20, 1 << 20}
}

type nativeSessions struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	closing bool
	ctx     context.Context
	cancel  context.CancelFunc
	open    func(context.Context, string, string, string, []byte) (cursorRunTransport, error)
	limits  nativeLimits
	entries map[string]*nativeSession
	handles map[string]*nativeSegment
	// Hooks are configured before the first Start. They must not retain a
	// requestScope. OnStart admits one physical Run before opening it. OnEnd
	// runs once after transport closure and before the final segment is closed.
	OnStart func(nativeReceipt) error
	OnEnd   func(nativeReceipt, runEvent, error)
}

type nativeSegment struct {
	owner              *nativeSession
	prefixLength       int
	prefixDigest       string
	events             []runEvent
	calls              []*nativeCall
	content, reasoning string
	expected, answered int
	closed, delivered  bool
	notify             chan struct{}
	detach             func() bool
	caller             context.Context
	resultDigest       string
	next               *nativeSegment
	deadline           time.Time
}

type nativeCall struct {
	ID, Name, Args, ExecID, NativeID, Digest string
	RequestID                                uint64
	submitted                                bool
}

type nativeSession struct {
	mu                                                      sync.Mutex
	pool                                                    *nativeSessions
	ctx                                                     context.Context
	cancel                                                  context.CancelFunc
	receipt                                                 nativeReceipt
	limits                                                  nativeLimits
	contract, identity                                      string
	tools                                                   []chatTool
	segment                                                 *nativeSegment
	segments                                                []*nativeSegment
	execs                                                   map[uint64]*nativeCall
	seenExecs                                               map[uint64]string
	internalReplies                                         map[uint64][][]byte
	nativeIDs                                               map[string]uint64
	blobs                                                   map[string][]byte
	bootstrap                                               map[string]bool
	checkpoint                                              []byte
	blobBytes, retained, turn, internalCalls, internalSteps int
	transport                                               cursorRunTransport
	state                                                   string // opening, running, submitting, parked, finishing, complete, failed
	err                                                     error
	usage                                                   map[string]any
	started, expires                                        time.Time
	lifetimeTimer, pendingTimer, batchTimer, replayTimer    *time.Timer
	batchGeneration                                         uint64
	waiting                                                 bool
	waitSince                                               time.Time
	heard                                                   time.Duration
	workDone                                                chan struct{}
	workOnce                                                sync.Once
	admissionDone                                           chan struct{}
	admitted                                                bool
	endDone                                                 chan struct{}
	endOnce                                                 sync.Once
}

func newNativeSessions(parent context.Context, open func(context.Context, string, string, string, []byte) (cursorRunTransport, error)) *nativeSessions {
	ctx, cancel := context.WithCancel(parent)
	return &nativeSessions{ctx: ctx, cancel: cancel, open: open, limits: defaultNativeLimits(), entries: map[string]*nativeSession{}, handles: map[string]*nativeSegment{}}
}

func (ns *nativeSessions) Count() int {
	ns.mu.Lock()
	all := make([]*nativeSession, 0, len(ns.entries))
	for _, s := range ns.entries {
		all = append(all, s)
	}
	ns.mu.Unlock()
	n := 0
	for _, s := range all {
		s.mu.Lock()
		active := s.state != "complete" && s.state != "failed"
		s.mu.Unlock()
		if active {
			n++
		}
	}
	return n
}
func (ns *nativeSessions) Busy() bool { return ns.Count() != 0 }

// Register every asynchronous engine worker under the same gate Close uses
// before Wait. This includes readers of already terminal replay owners.
func (ns *nativeSessions) worker(fn func()) bool {
	ns.mu.Lock()
	if ns.closing {
		ns.mu.Unlock()
		return false
	}
	ns.wg.Add(1)
	ns.mu.Unlock()
	go func() { defer ns.wg.Done(); fn() }()
	return true
}

func (ns *nativeSessions) forget(s *nativeSession) {
	ns.mu.Lock()
	if ns.entries[s.receipt.RunID] != s {
		ns.mu.Unlock()
		return
	}
	delete(ns.entries, s.receipt.RunID)
	for id, seg := range ns.handles {
		if seg.owner == s {
			delete(ns.handles, id)
		}
	}
	ns.mu.Unlock()
	s.mu.Lock()
	if s.replayTimer != nil {
		s.replayTimer.Stop()
		s.replayTimer = nil
	}
	s.mu.Unlock()
}

func (ns *nativeSessions) Close() error {
	ns.mu.Lock()
	ns.closing = true
	ns.cancel()
	all := make([]*nativeSession, 0, len(ns.entries))
	for _, s := range ns.entries {
		all = append(all, s)
	}
	ns.mu.Unlock()
	for _, s := range all {
		s.finish(failure(409, "native session store shut down"), runEvent{})
	}
	for _, s := range all {
		<-s.endDone
		<-s.workDone
		ns.forget(s)
	}
	ns.wg.Wait()
	return nil
}

// Start opens only the transport synchronously, never waits for inference.
// open sends the bootstrap exactly once. Retained owners use their process
// context, not a request's callback lifetime; Delivered detaches the latter.
func (ns *nativeSessions) Start(ctx context.Context, in nativeInput) (<-chan runEvent, nativeReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, nativeReceipt{}, err
	}
	if err := ns.ctx.Err(); err != nil {
		return nil, nativeReceipt{}, failure(409, "native session store is closed")
	}
	if len(in.Chat.Messages) == 0 || in.Account == "" {
		return nil, nativeReceipt{}, failure(400, "native session requires messages and account identity")
	}
	if len(encodeJSON(in.Chat.Messages)) > ns.limits.RetainedBytes {
		return nil, nativeReceipt{}, failure(413, "native history size limit exceeded")
	}
	contract := nativeContract(in)
	ids := nativeTailHandles(in.Chat.Messages)
	if len(ids) > 0 {
		ns.mu.Lock()
		seg := ns.handles[ids[0]]
		for _, id := range ids {
			if ns.handles[id] == nil || ns.handles[id] != seg {
				seg = nil
				break
			}
		}
		ns.mu.Unlock()
		if seg == nil {
			return nil, nativeReceipt{}, failure(409, "native continuation owner is unavailable or expired")
		}
		return seg.owner.resume(ctx, in, contract, seg)
	}
	// A result carrying a native ID outside the complete trailing batch must
	// not be silently imported into a new Run as if it were fresh input.
	if nativeTailAssistant(in.Chat.Messages) {
		return nil, nativeReceipt{}, failure(409, "native continuation has no complete correlated result batch")
	}
	prefix, err := nativeHistoryDigest(in.Chat.Messages)
	if err != nil {
		return nil, nativeReceipt{}, err
	}
	first, blobs := buildRun(in.Conversation, in.Tools, in.Model, in.CID)
	blobBytes := 0
	for _, b := range blobs {
		blobBytes += len(b)
	}
	if blobBytes > ns.limits.BlobBytes {
		return nil, nativeReceipt{}, failure(413, "native bootstrap blob limit exceeded")
	}
	now := time.Now()
	runCtx, cancel := context.WithCancel(ns.ctx)
	s := &nativeSession{pool: ns, ctx: runCtx, cancel: cancel, limits: ns.limits, contract: contract, identity: strings.ReplaceAll(randomID(), "-", ""), tools: nativeCloneTools(in.Tools), execs: map[uint64]*nativeCall{}, seenExecs: map[uint64]string{}, internalReplies: map[uint64][][]byte{}, nativeIDs: map[string]uint64{}, blobs: blobs, bootstrap: map[string]bool{}, blobBytes: blobBytes, state: "opening", started: now, expires: now.Add(ns.limits.Lifetime), workDone: make(chan struct{}), endDone: make(chan struct{}), internalSteps: 1, admissionDone: make(chan struct{})}
	s.receipt = nativeReceipt{RunID: randomID(), CID: in.CID, RequestID: randomID(), Account: in.Account, ModelID: in.Model.ID, Started: now, token: in.Token}
	for k := range blobs {
		s.bootstrap[k] = true
	}
	seg := &nativeSegment{owner: s, prefixLength: len(in.Chat.Messages), prefixDigest: prefix, notify: make(chan struct{})}
	s.segment = seg
	s.segments = []*nativeSegment{seg}
	// Evict only terminal replay owners, only under capacity pressure. Do not
	// hold the pool lock while inspecting session state (seal takes the other
	// direction to register handles before exposing a completed handoff).
	ns.mu.Lock()
	oldEntries := []*nativeSession{}
	atCapacity := len(ns.entries) >= ns.limits.PerProcess
	accountEntries := 0
	for _, old := range ns.entries {
		if old.receipt.Account == in.Account {
			accountEntries++
		}
		oldEntries = append(oldEntries, old)
	}
	ns.mu.Unlock()
	if atCapacity || accountEntries >= ns.limits.PerAccount {
		for _, old := range oldEntries {
			old.mu.Lock()
			terminal := old.state == "complete" || old.state == "failed"
			old.mu.Unlock()
			if terminal {
				ns.forget(old)
			}
		}
	}
	ns.mu.Lock()
	activeAccount := 0
	for _, old := range ns.entries {
		if old.receipt.Account == in.Account {
			activeAccount++
		}
	}
	if ns.ctx.Err() != nil || len(ns.entries) >= ns.limits.PerProcess || activeAccount >= ns.limits.PerAccount {
		ns.mu.Unlock()
		cancel()
		return nil, nativeReceipt{}, failure(503, "native session capacity reached")
	}
	ns.entries[s.receipt.RunID] = s
	ns.mu.Unlock()
	if ns.OnStart != nil {
		err = ns.OnStart(s.receipt)
	}
	s.mu.Lock()
	s.admitted = err == nil
	close(s.admissionDone)
	s.mu.Unlock()
	if err != nil {
		cancel()
		ns.forget(s)
		s.workOnce.Do(func() { close(s.workDone) })
		s.endOnce.Do(func() { close(s.endDone) })
		return nil, nativeReceipt{}, err
	}
	s.attach(ctx, seg)
	s.mu.Lock()
	s.lifetimeTimer = time.AfterFunc(s.limits.Lifetime, func() { s.finish(failure(409, "native Run lifetime expired"), runEvent{}) })
	s.mu.Unlock()
	tr, err := ns.open(s.ctx, in.Account, in.Token, s.receipt.RequestID, first)
	if err != nil {
		s.finish(err, runEvent{})
		s.workOnce.Do(func() { close(s.workDone) })
		return nil, nativeReceipt{}, err
	}
	s.mu.Lock()
	if s.state == "finishing" || s.state == "complete" || s.state == "failed" {
		s.mu.Unlock()
		_ = tr.Close()
		s.workOnce.Do(func() { close(s.workDone) })
		return nil, nativeReceipt{}, failure(409, "native Run cancelled while opening")
	}
	s.transport = tr
	s.receipt.Protocol = tr.Protocol()
	s.state = "running"
	receipt := s.receipt
	s.mu.Unlock()
	receipt.Delivered = func() { s.delivered(seg) }
	if !ns.worker(s.pump) {
		s.finish(failure(409, "native session store shut down while opening"), runEvent{})
		s.workOnce.Do(func() { close(s.workDone) })
		return nil, nativeReceipt{}, failure(409, "native session store is closed")
	}
	ns.worker(s.watch)
	return s.subscribe(ctx, seg, false), receipt, nil
}

func (s *nativeSession) attach(ctx context.Context, seg *nativeSegment) {
	s.mu.Lock()
	seg.caller = ctx
	s.mu.Unlock()
	detach := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		clean := seg.delivered
		s.mu.Unlock()
		if !clean {
			s.finish(failure(409, "native caller cancelled before response delivery"), runEvent{})
		}
	})
	s.mu.Lock()
	if s.state == "finishing" || s.state == "complete" || s.state == "failed" {
		detach()
	} else {
		seg.detach = detach
	}
	s.mu.Unlock()
}
func (s *nativeSession) delivered(seg *nativeSegment) {
	s.mu.Lock()
	if !seg.delivered && seg.caller != nil && seg.caller.Err() != nil {
		s.mu.Unlock()
		s.finish(failure(409, "native caller cancelled before response delivery"), runEvent{})
		return
	}
	seg.delivered = true
	seg.caller = nil
	if seg.detach != nil {
		seg.detach()
		seg.detach = nil
	}
	s.mu.Unlock()
}
func (s *nativeSession) wakeLocked(seg *nativeSegment) {
	close(seg.notify)
	seg.notify = make(chan struct{})
}
func (s *nativeSession) retainLocked(n int) error {
	if n > s.limits.RetainedBytes-s.retained {
		return failure(503, "native retained output limit exceeded")
	}
	s.retained += n
	return nil
}
func (s *nativeSession) emitLocked(ev runEvent) error {
	if err := s.retainLocked(len(encodeJSON(map[string]any{"text": ev.text, "reasoning": ev.reasoning, "tool": ev.tool, "stop": ev.stop}))); err != nil {
		return err
	}
	seg := s.segment
	seg.events = append(seg.events, ev)
	seg.content += ev.text
	seg.reasoning += ev.reasoning
	s.wakeLocked(seg)
	return nil
}
func (s *nativeSession) subscribe(ctx context.Context, seg *nativeSegment, replay bool) <-chan runEvent {
	out := make(chan runEvent, 1)
	if !s.pool.worker(func() {
		defer close(out)
		at := 0
		for {
			s.mu.Lock()
			if at < len(seg.events) {
				ev := seg.events[at]
				at++
				s.mu.Unlock()
				if replay && ev.stop != "" {
					ev.usage = nil
					ev.accounting = map[string]any{"status": "replayed", "run_id": s.receipt.RunID}
				}
				if ev.tool != nil {
					var tool map[string]any
					_ = json.Unmarshal(encodeJSON(ev.tool), &tool)
					ev.tool = tool
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				case <-s.pool.ctx.Done():
					return
				}
				continue
			}
			closed := seg.closed
			notify := seg.notify
			s.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-notify:
			case <-ctx.Done():
				return
			case <-s.pool.ctx.Done():
				return
			}
		}
	}) {
		close(out)
	}
	return out
}

func (s *nativeSession) resume(ctx context.Context, in nativeInput, contract string, batch *nativeSegment) (<-chan runEvent, nativeReceipt, error) {
	s.mu.Lock()
	if s.contract != contract || s.receipt.Account != in.Account {
		s.mu.Unlock()
		return nil, nativeReceipt{}, failure(409, "native continuation account, model, or executable tool contract changed")
	}
	if s.state == "failed" || s.state == "finishing" || time.Now().After(s.expires) {
		s.mu.Unlock()
		return nil, nativeReceipt{}, failure(409, "native continuation expired or failed")
	}
	results, d, err := nativeMatchResults(batch, in.Chat.Messages, s.limits, in.SourceFormat)
	if err != nil {
		s.mu.Unlock()
		return nil, nativeReceipt{}, err
	}
	receipt := s.receipt
	receipt.Resumed = true
	if batch.resultDigest != "" {
		if batch.resultDigest != d || batch.next == nil || !batch.next.closed || !batch.next.delivered {
			s.mu.Unlock()
			return nil, nativeReceipt{}, failure(409, "native results already reserved, different, or still in flight")
		}
		next := batch.next
		receipt.Replay = true
		receipt.Delivered = func() { s.delivered(next) }
		s.mu.Unlock()
		return s.subscribe(ctx, next, true), receipt, nil
	}
	if s.state != "parked" || s.segment != batch || !batch.delivered {
		s.mu.Unlock()
		return nil, nativeReceipt{}, failure(409, "native owner is busy or its handoff was not delivered")
	}
	if !time.Now().Before(batch.deadline) {
		s.mu.Unlock()
		return nil, nativeReceipt{}, failure(409, "native tool-result deadline expired")
	}
	if time.Until(s.expires) <= min(time.Minute, s.limits.Lifetime/10) {
		s.mu.Unlock()
		return nil, nativeReceipt{}, failure(409, "native Run is near its lifetime deadline")
	}
	prefix, err := nativeHistoryDigest(in.Chat.Messages)
	if err != nil {
		s.mu.Unlock()
		return nil, nativeReceipt{}, err
	}
	next := &nativeSegment{owner: s, prefixLength: len(in.Chat.Messages), prefixDigest: prefix, notify: make(chan struct{})}
	batch.resultDigest = d
	batch.next = next
	s.segment = next
	s.segments = append(s.segments, next)
	s.turn++
	s.internalSteps++
	s.state = "submitting"
	s.heard = 0
	s.waitSince = time.Now()
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	s.mu.Unlock()
	s.attach(ctx, next)
	receipt.Delivered = func() { s.delivered(next) }
	if !s.pool.worker(func() {
		for _, r := range results {
			msg := protobuf(nil).uint(1, r.call.RequestID)
			if r.call.ExecID != "" {
				msg = msg.text(15, r.call.ExecID)
			}
			msg = msg.bytes(11, r.encoded)
			// Reservation happens before the first write. A failed write can have
			// reached Cursor, so this owner can never accept a second submission.
			s.mu.Lock()
			r.call.submitted = true
			s.mu.Unlock()
			if err := s.send(protobuf(nil).bytes(2, msg)); err != nil {
				s.finish(failure(errorStatus(err), "native result submission failed; outcome uncertain, cannot resubmit"), runEvent{})
				return
			}
			if err := s.send(nativeExecClose(r.call.RequestID)); err != nil {
				s.finish(failure(errorStatus(err), "native result close failed; outcome uncertain, cannot resubmit"), runEvent{})
				return
			}
		}
		s.mu.Lock()
		if s.state == "submitting" && s.segment == next {
			s.state = "running"
			s.heard = 0
			s.waitSince = time.Now()
		}
		s.mu.Unlock()
	}) {
		s.finish(failure(409, "native session store closed before submitting results"), runEvent{})
		return nil, nativeReceipt{}, failure(409, "native session store is closed")
	}
	return s.subscribe(ctx, next, false), receipt, nil
}

func (s *nativeSession) send(data []byte) error {
	s.mu.Lock()
	tr := s.transport
	state := s.state
	s.mu.Unlock()
	if tr == nil || state == "finishing" || state == "complete" || state == "failed" {
		return failure(409, "native transport is closed")
	}
	return tr.Send(s.ctx, data)
}

func (s *nativeSession) finish(err error, end runEvent) {
	s.finishIf(err, end, nil)
}

// Timer predicates are checked under the same lock that retires the owner.
// A queued old timer cannot invalidate a Run after progress/reservation won.
func (s *nativeSession) finishIf(err error, end runEvent, check func() bool) bool {
	<-s.admissionDone
	s.mu.Lock()
	if check != nil && !check() {
		s.mu.Unlock()
		return false
	}
	if s.state == "finishing" || s.state == "complete" || s.state == "failed" {
		s.mu.Unlock()
		return false
	}
	s.state = "finishing"
	defer s.endOnce.Do(func() { close(s.endDone) })
	s.err = err
	for _, timer := range []*time.Timer{s.lifetimeTimer, s.pendingTimer, s.batchTimer, s.replayTimer} {
		if timer != nil {
			timer.Stop()
		}
	}
	for _, seg := range s.segments {
		seg.caller = nil
		if seg.detach != nil {
			seg.detach()
			seg.detach = nil
		}
	}
	tr := s.transport
	receipt := s.receipt
	end.internalCalls = s.internalCalls
	end.internalSteps = s.internalSteps
	seg := s.segment
	s.mu.Unlock()
	s.cancel()
	if tr != nil {
		_ = tr.Close()
	}
	if s.admitted && s.pool.OnEnd != nil {
		s.pool.OnEnd(receipt, end, err)
	}
	s.mu.Lock()
	s.receipt.token = ""
	s.blobs = nil
	s.bootstrap = nil
	s.checkpoint = nil
	s.tools = nil
	s.seenExecs = nil
	s.internalReplies = nil
	s.nativeIDs = nil
	if err != nil {
		s.state = "failed"
		if !seg.closed {
			seg.events = append(seg.events, runEvent{err: err})
			seg.closed = true
			s.wakeLocked(seg)
		}
	} else {
		s.state = "complete"
		end.usage = nil // TurnEnded is a Run total, never this step's context.
		seg.events = append(seg.events, end)
		seg.closed = true
		s.wakeLocked(seg)
		s.replayTimer = time.AfterFunc(s.limits.ReplayTTL, func() { s.pool.forget(s) })
	}
	s.mu.Unlock()
	if err != nil {
		s.pool.forget(s)
	}
	return true
}

func (s *nativeSession) pump() {
	defer s.workOnce.Do(func() { close(s.workDone) })
	for {
		s.mu.Lock()
		s.waiting = true
		s.waitSince = time.Now()
		tr := s.transport
		s.mu.Unlock()
		data, end, err := tr.Next(s.ctx)
		s.mu.Lock()
		if s.state != "parked" {
			s.heard += time.Since(s.waitSince)
		}
		s.waiting = false
		s.mu.Unlock()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = failure(502, "native reply broke off before TurnEnded: EOF")
			}
			s.finish(err, runEvent{})
			return
		}
		if end {
			if err = connectError(200, data); err == nil {
				err = failure(502, "native stream ended before TurnEnded")
			}
			s.finish(err, runEvent{})
			return
		}
		terminal, err := s.handleFrame(data)
		if err != nil {
			s.finish(err, runEvent{})
			return
		}
		if terminal {
			s.mu.Lock()
			u := s.usage
			s.mu.Unlock()
			s.finish(nil, runEvent{stop: "stop", usage: u})
			return
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

func (s *nativeSession) watch() {
	interval := min(100*time.Millisecond, min(s.limits.Idle, s.limits.Quiet)/4)
	if interval <= 0 {
		interval = time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	execTick := time.NewTicker(s.limits.ExecHeartbeat)
	defer execTick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			if s.finishIf(failure(502, "Cursor went quiet while native inference was pending"), runEvent{}, func() bool {
				gap := time.Since(s.waitSince)
				return s.waiting && (s.state == "running" || s.state == "submitting") && (gap >= s.limits.Idle || s.heard+gap >= s.limits.Quiet)
			}) {
				return
			}
		case <-execTick.C:
			s.mu.Lock()
			ids := []uint64{}
			for id, c := range s.execs {
				if !c.submitted {
					ids = append(ids, id)
				}
			}
			s.mu.Unlock()
			for _, id := range ids {
				if err := s.send(protobuf(nil).bytes(5, protobuf(nil).bytes(3, protobuf(nil).uint(1, id)))); err != nil {
					s.finish(err, runEvent{})
					return
				}
			}
		}
	}
}

var nativeHandlePattern = regexp.MustCompile(`^call_cursor_[a-f0-9]{32}_[0-9]+_[0-9]+$`)

func nativeTailAssistant(messages []map[string]any) bool {
	i := len(messages) - 1
	for i >= 0 && str(messages[i]["role"]) == "tool" {
		i--
	}
	if i == len(messages)-1 {
		return false
	}
	for ; i >= 0 && str(messages[i]["role"]) == "assistant"; i-- {
		for _, raw := range array(messages[i]["tool_calls"]) {
			if nativeHandlePattern.MatchString(str(object(raw)["id"])) {
				return true
			}
		}
	}
	return false
}

func nativeTailHandles(messages []map[string]any) []string {
	out := []string{}
	for i := len(messages) - 1; i >= 0 && str(messages[i]["role"]) == "tool"; i-- {
		id := str(messages[i]["tool_call_id"])
		if nativeHandlePattern.MatchString(id) {
			out = append(out, id)
		}
	}
	return out
}

func nativeCloneTools(tools []chatTool) []chatTool {
	out := append([]chatTool(nil), tools...)
	for i := range out {
		var m map[string]any
		_ = json.Unmarshal(encodeJSON(out[i].Schema), &m)
		out[i].Schema = m
	}
	return out
}
func nativeContract(in nativeInput) string {
	params := append([]parameter(nil), in.Model.Params...)
	sort.Slice(params, func(i, j int) bool {
		if params[i].ID == params[j].ID {
			return params[i].Value < params[j].Value
		}
		return params[i].ID < params[j].ID
	})
	tools := []any{}
	for _, t := range in.Tools {
		strict := false
		for _, raw := range in.Chat.Tools {
			f := object(raw["function"])
			if str(f["name"]) == t.Name {
				strict, _ = f["strict"].(bool)
			}
		}
		tools = append(tools, map[string]any{"name": t.Name, "schema": nativeSchemaContract(t.Schema, false), "strict": strict})
	}
	sort.Slice(tools, func(i, j int) bool { return str(object(tools[i])["name"]) < str(object(tools[j])["name"]) })
	return digest(string(encodeJSON(map[string]any{"account": in.Account, "owner_scope": in.OwnerScope, "source_format": in.SourceFormat, "model": in.Model.ID, "max": in.Model.Max, "params": params, "tools": tools, "choice": in.Chat.ToolChoice})))
}
func nativeSchemaContract(v any, names bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if !names && k == "description" {
				continue
			}
			container := !names && (k == "properties" || k == "patternProperties" || k == "$defs" || k == "definitions" || k == "dependentSchemas")
			out[k] = nativeSchemaContract(val, container)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = nativeSchemaContract(v, false)
		}
		return out
	default:
		return v
	}
}

func nativeArgs(v any) (map[string]any, error) {
	if v == nil {
		v = "{}"
	}
	if text, ok := v.(string); ok {
		var val any
		if json.Unmarshal([]byte(text), &val) != nil {
			return nil, failure(409, "invalid native continuation tool arguments")
		}
		v = val
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, failure(409, "native continuation arguments must be an object")
	}
	return m, nil
}
func nativeDisplay(m map[string]any) (string, string, error) {
	text := ""
	reason := reasoningOf(m)
	if reason == "" {
		reason = str(m["reasoning_text"])
	}
	switch content := m["content"].(type) {
	case nil:
	case string:
		text = content
	case []any:
		for _, p := range content {
			v := object(p)
			kind := str(v["type"])
			t, ok := v["text"].(string)
			if !ok || (kind != "text" && kind != "reasoning") {
				return "", "", failure(409, "unsupported native assistant display")
			}
			if kind == "text" {
				text += t
			} else {
				reason += t
			}
		}
	default:
		return "", "", failure(409, "unsupported native assistant display")
	}
	return text, reason, nil
}
func nativeHistoryDigest(messages []map[string]any) (string, error) {
	norm := make([]any, 0, len(messages))
	for _, m := range messages {
		var out map[string]any
		_ = json.Unmarshal(encodeJSON(m), &out)
		if str(m["role"]) == "assistant" {
			text, reason, err := nativeDisplay(m)
			if err != nil {
				return "", err
			}
			out["content"] = text
			delete(out, "reasoning")
			delete(out, "reasoning_text")
			delete(out, "reasoning_content")
			if reason != "" {
				out["reasoning_content"] = reason
			}
			if calls := array(m["tool_calls"]); len(calls) > 0 {
				nc := []any{}
				for _, raw := range calls {
					c := object(raw)
					f := object(c["function"])
					args, err := nativeArgs(f["arguments"])
					if err != nil {
						return "", err
					}
					nc = append(nc, map[string]any{"id": c["id"], "type": "function", "function": map[string]any{"name": f["name"], "arguments": args}})
				}
				out["tool_calls"] = nc
			}
		}
		norm = append(norm, out)
	}
	return digest(string(encodeJSON(norm))), nil
}

type nativeResult struct {
	call    *nativeCall
	encoded []byte
	digest  any
}

// CPA v8.0.43 splits streamed reasoning blocks at nonempty visible text or
// tool calls, then joins those blocks when translating Responses back to Chat.
// Reconstruct only that exact display from our own exported events. This never
// rewrites caller history or changes tool-call/result validation.
func nativeCPAResponsesReasoning(events []runEvent) string {
	var block strings.Builder
	combined := ""
	flush := func() {
		if block.Len() == 0 {
			return
		}
		incoming := block.String()
		block.Reset()
		existingTrimmed, incomingTrimmed := strings.TrimSpace(combined), strings.TrimSpace(incoming)
		switch {
		case existingTrimmed == "":
			combined = incoming
		case incomingTrimmed == "":
		case existingTrimmed == "[reasoning unavailable]":
			combined = incoming
		case incomingTrimmed == "[reasoning unavailable]", existingTrimmed == incomingTrimmed:
		default:
			combined += "\n\n" + incoming
		}
	}
	for _, ev := range events {
		block.WriteString(ev.reasoning)
		if ev.text != "" || ev.tool != nil {
			flush()
		}
	}
	flush()
	return combined
}

func nativeMatchResults(batch *nativeSegment, messages []map[string]any, limits nativeLimits, sourceFormat string) ([]nativeResult, string, error) {
	bad := func(msg string) ([]nativeResult, string, error) {
		return nil, "", failure(409, "native continuation %s", msg)
	}
	if len(messages) < batch.prefixLength {
		return bad("history was truncated")
	}
	h, err := nativeHistoryDigest(messages[:batch.prefixLength])
	if err != nil {
		return nil, "", err
	}
	if h != batch.prefixDigest {
		return bad("history or system instructions changed")
	}
	suffix := messages[batch.prefixLength:]
	a := 0
	content, reason := "", ""
	calls := []any{}
	for a < len(suffix) && str(suffix[a]["role"]) == "assistant" {
		m := suffix[a]
		for k := range m {
			switch k {
			case "role", "content", "tool_calls", "reasoning", "reasoning_content", "reasoning_text":
			default:
				return bad("assistant fields changed")
			}
		}
		t, r, e := nativeDisplay(m)
		if e != nil {
			return nil, "", e
		}
		content += t
		reason += r
		for _, raw := range array(m["tool_calls"]) {
			c := object(raw)
			f := object(c["function"])
			if typ := str(c["type"]); typ != "" && typ != "function" {
				return bad("tool-call type changed")
			}
			args, e := nativeArgs(f["arguments"])
			if e != nil {
				return nil, "", e
			}
			calls = append(calls, map[string]any{"id": c["id"], "name": f["name"], "args": args})
		}
		a++
	}
	if a == 0 {
		return bad("assistant tool-call message is missing")
	}
	if content != batch.content {
		return nil, "", failure(409, "native continuation assistant text changed: received_bytes=%d expected_bytes=%d", len(content), len(batch.content))
	}
	reasonMatches := reason == "" || reason == batch.reasoning
	if !reasonMatches && sourceFormat == "openai-response" {
		reasonMatches = reason == nativeCPAResponsesReasoning(batch.events) || (batch.reasoning == "" && reason == "[reasoning unavailable]")
	}
	if !reasonMatches {
		return nil, "", failure(409, "native continuation assistant reasoning changed: received_bytes=%d expected_bytes=%d exact_unavailable_sentinel=%t", len(reason), len(batch.reasoning), reason == "[reasoning unavailable]")
	}
	expected := []any{}
	byID := map[string]*nativeCall{}
	for _, c := range batch.calls {
		args, _ := nativeArgs(c.Args)
		expected = append(expected, map[string]any{"id": c.ID, "name": c.Name, "args": args})
		byID[c.ID] = c
	}
	sortCalls := func(v []any) {
		sort.Slice(v, func(i, j int) bool { return str(object(v[i])["id"]) < str(object(v[j])["id"]) })
	}
	sortCalls(calls)
	sortCalls(expected)
	if string(encodeJSON(calls)) != string(encodeJSON(expected)) {
		return bad("assistant tool IDs, names, or arguments changed")
	}
	seen := map[string]bool{}
	results := []nativeResult{}
	total := 0
	for _, m := range suffix[a:] {
		if str(m["role"]) != "tool" {
			return bad("interleaves new input with tool results")
		}
		id := str(m["tool_call_id"])
		c := byID[id]
		if c == nil || seen[id] {
			return bad("contains an unknown or duplicate tool result")
		}
		seen[id] = true
		if name := str(m["name"]); name != "" && name != c.Name {
			return bad("tool result name changed")
		}
		encoded, err := nativeEncodeResult(m, limits)
		if err != nil {
			return nil, "", err
		}
		total += len(encoded)
		if total > limits.RetainedBytes {
			return bad("result batch exceeds its byte limit")
		}
		results = append(results, nativeResult{c, encoded, map[string]any{"id": id, "result": encoded}})
	}
	if len(results) != len(expected) {
		return bad("requires the complete parallel result batch")
	}
	sort.Slice(results, func(i, j int) bool { return results[i].call.ID < results[j].call.ID })
	dig := []any{}
	for _, r := range results {
		dig = append(dig, r.digest)
	}
	return results, digest(string(encodeJSON(dig))), nil
}
