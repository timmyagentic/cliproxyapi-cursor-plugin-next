package main

import (
	"testing"
	"time"
)

func newTestRuntime(t *testing.T) *pluginRuntime {
	t.Helper()
	p := newRuntime()
	p.usagePolicy.Wait = 20 * time.Millisecond
	p.usagePolicy.First = 0
	p.usagePolicy.Interval = time.Millisecond
	p.usagePolicy.Ask = 10 * time.Millisecond
	p.usagePolicy.Stable = 0
	t.Cleanup(p.stop)
	return p
}
