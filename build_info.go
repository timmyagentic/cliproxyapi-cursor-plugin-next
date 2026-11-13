package main

import (
	"regexp"
	"runtime"
	"runtime/debug"
)

// Read the library's own Go build info, never the host process's checkout.
// Ad hoc builds without the explicit stamp remain unattributed.
type pluginBuildInfo struct {
	ModulePath   string `json:"module_path"`
	SourceCommit string `json:"source_commit"`
	SourceDirty  *bool  `json:"source_dirty"`
	GoVersion    string `json:"go_version"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
}

const pluginModulePath = "github.com/timmyagentic/cliproxyapi-cursor-plugin-next"

// Set only by the maintained build entry point, using git -C at the plugin
// root. Go 1.26's automatic VCS discovery skips linked-worktree .git files and
// may stamp an enclosing repository instead.
var buildStamp = "unknown"
var buildStampPattern = regexp.MustCompile(`^CURSOR_NEXT_BUILD_V1\|([0-9a-f]{40}|[0-9a-f]{64})\|([01])\|END$`)

func currentBuildInfo() pluginBuildInfo {
	info, _ := debug.ReadBuildInfo()
	return buildInfoFrom(info, buildStamp)
}

func buildInfoFrom(info *debug.BuildInfo, stamp string) pluginBuildInfo {
	result := pluginBuildInfo{ModulePath: "unknown", SourceCommit: "unknown", GoVersion: "unknown", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	// A Go host may load this C shared library. Never attribute the host's
	// revision/toolchain to the plugin if its runtime returns host build info.
	if info == nil || info.Main.Path != pluginModulePath {
		return result
	}
	result.ModulePath = info.Main.Path
	if info.GoVersion != "" {
		result.GoVersion = info.GoVersion
	}
	if fields := buildStampPattern.FindStringSubmatch(stamp); len(fields) == 3 {
		dirty := fields[2] == "1"
		result.SourceCommit = fields[1]
		result.SourceDirty = &dirty
	}
	return result
}
