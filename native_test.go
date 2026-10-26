package main

import "testing"

func newTestRuntime(t *testing.T) *pluginRuntime {
	t.Helper()
	p := newRuntime()
	t.Cleanup(p.stop)
	return p
}
