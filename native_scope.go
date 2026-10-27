package main

import (
	"sync"
	"time"
)

// ExecuteStream's C call may still be waiting for the native response head.
// CPA already owns a cancelable output stream at that point. A scoped empty
// chunk observes that boundary without making another upstream HTTP request.
func (s *requestScope) watchNativeOpening(streamID, id, model string, created int64, interval time.Duration) func() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return func() {}
	}
	s.pending.Add(1)
	s.mu.Unlock()
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		defer s.pending.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.ctx.Done():
				return
			case <-tick.C:
				payload := encodeJSON(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{}})
				if err := s.streamCall("host.stream.emit", map[string]any{"stream_id": streamID, "payload": payload}, nil); err != nil {
					s.cancel()
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }); <-done }
}
