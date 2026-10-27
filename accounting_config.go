package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Preserve the notices in the distributed shared library as well as the source
// repository. They can be read through the plugin.license ABI method.
//
//go:embed LICENSE
var distributionLicense string

//go:embed NOTICE.md
var distributionNotice string

//go:embed third_party/YAML-LICENSE.txt
var yamlLicense string

//go:embed third_party/APACHE-2.0.txt
var apacheLicense string

func accountingConfigFields() []any {
	return []any{
		map[string]any{"Name": "transport", "Type": "string", "Default": "auto", "Description": "auto uses native sessions when native-proxy-url is explicit; host keeps CPA HTTP; native requires explicit proxy configuration."},
		map[string]any{"Name": "native-proxy-url", "Type": "string", "Default": "", "Description": "Explicit native Run proxy: direct, http://, https://, socks5:// or socks5h://. Empty retains CPA host transport. Match the host's approved route; never inferred."},
		map[string]any{"Name": "usage-journal", "Type": "string", "Default": "", "Description": "Absolute JSON path in a private directory. Enables durable, separate usage reconciliation; empty uses memory only."},
		map[string]any{"Name": "usage-wait-ms", "Type": "integer", "Default": 8000, "Description": "Bounded wait for the current Run's measured dashboard usage (3000–30000 ms). Missing usage is omitted, never estimated."},
	}
}

func (p *pluginRuntime) configureAccounting(raw []byte) error {
	native, err := parseNativeConfig(raw)
	if err != nil {
		return err
	}
	var cfg struct {
		Journal string `yaml:"usage-journal"`
		Wait    *int   `yaml:"usage-wait-ms"`
	}
	if len(raw) > 64<<10 || yaml.Unmarshal(raw, &cfg) != nil {
		return failure(400, "Invalid Cursor accounting configuration")
	}
	path := cfg.Journal
	if env := os.Getenv("CURSOR_USAGE_JOURNAL"); env != "" {
		if path != "" && filepath.Clean(path) != filepath.Clean(env) {
			return failure(400, "Cursor journal configuration conflicts with CURSOR_USAGE_JOURNAL")
		}
		path = env
	}
	if path != "" {
		if !filepath.IsAbs(path) {
			return failure(400, "Cursor usage-journal must be an absolute path")
		}
		path = filepath.Clean(path)
	}
	policy := defaultStepUsagePolicy()
	if cfg.Wait != nil {
		if *cfg.Wait < 3000 || *cfg.Wait > 30000 {
			return failure(400, "Cursor usage-wait-ms must be between 3000 and 30000")
		}
		policy.Wait = time.Duration(*cfg.Wait) * time.Millisecond
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return failure(503, "Cursor plugin is shut down")
	}
	if len(p.usageActive) != 0 || p.nativeRequests != 0 {
		return failure(409, "Wait for active Cursor Runs before reconfiguring accounting")
	}
	if path != p.journalPath {
		if p.journal != nil && len(p.journal.Records()) != 0 {
			return failure(409, "Restart the host to change an in-use Cursor journal path")
		}
		journal, err := newUsageJournal(path)
		if err != nil {
			return failure(503, "Cursor usage journal is unavailable")
		}
		if p.journal != nil {
			_ = p.journal.Shutdown()
		}
		p.journal, p.journalErr, p.journalPath = journal, nil, path
	}
	if p.journalErr != nil {
		return failure(503, "Cursor usage journal is unavailable")
	}
	p.usagePolicy = policy
	return p.configureNativeLocked(native)
}
