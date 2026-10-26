package main

import (
	"bytes"
	"testing"
)

func FuzzNativeProtobufAndConnect(f *testing.F) {
	for _, seed := range [][]byte{{}, {0x1a, 1, 'x'}, {0x08, 0}, {0x2a, 0}, {0, 0, 0, 0, 2, 8, 0}, {0xff, 0xff, 0xff, 0xff, 0xff}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		_, _ = pbFields(data)
		_, _ = pbAny(data)
		_, _, _ = readConnectFrame(bytes.NewReader(data))
	})
}
