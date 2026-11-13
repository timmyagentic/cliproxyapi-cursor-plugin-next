package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildInfoReportsStampedSourceAndUnknownUnstampedBuilds(t *testing.T) {
	for _, info := range []*debug.BuildInfo{nil, {GoVersion: "go1.26.10"}, {
		GoVersion: "go1.26.10", Main: debug.Module{Path: "synthetic-cpa-host"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "host-source-must-not-be-used"}},
	}} {
		got := buildInfoFrom(info, "unknown")
		if got.ModulePath != "unknown" || got.SourceCommit != "unknown" || got.SourceDirty != nil || got.GoVersion != "unknown" || got.GOOS == "" || got.GOARCH == "" {
			t.Fatalf("unstamped build invented provenance: %+v", got)
		}
	}
	for _, value := range []string{"1", "0"} {
		got := buildInfoFrom(&debug.BuildInfo{GoVersion: "go1.26.10", Main: debug.Module{Path: pluginModulePath}, Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "must-not-use-enclosing-repository"},
		}}, "CURSOR_NEXT_BUILD_V1|0123456789abcdef0123456789abcdef01234567|"+value+"|END")
		if got.SourceCommit != "0123456789abcdef0123456789abcdef01234567" || got.SourceDirty == nil || *got.SourceDirty != (value == "1") || got.GoVersion != "go1.26.10" {
			t.Fatalf("embedded build provenance was lost: %+v", got)
		}
	}
	unstamped := buildInfoFrom(&debug.BuildInfo{Main: debug.Module{Path: pluginModulePath}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.modified", Value: "false"},
	}}, "unknown")
	if unstamped.SourceCommit != "unknown" || unstamped.SourceDirty != nil {
		t.Fatal("automatic VCS metadata was trusted without the explicit plugin stamp")
	}
}
