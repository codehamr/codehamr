// Package config owns the .codehamr/ directory: config.yaml plus the
// embedded default system prompt. The system prompt lives only in the
// binary — never on disk — so users cannot tamper with it and every
// codehamr release ships a guaranteed-consistent prompt.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

//go:embed PROMPT_SYS.md
var DefaultSystemPrompt string

const DirName = ".codehamr"

// defaultContextSize is the floor Bootstrap coerces bogus context_size
// values to. Matches the Default() local profile so a hand-edited config
// that forgot the field behaves the same as a freshly-bootstrapped one.
const defaultContextSize = 65536

// cloudProfileNames is the set of managed profiles whose context_size is
// authoritatively set by the server via the X-Context-Window response
// header (see internal/cloud). For these profiles we deliberately leave
// the on-disk context_size empty: Bootstrap does not seed it, the Coerce
// loop does not fall back to defaultContextSize, and the TUI reads the
// live value from each chat response. Local Ollama is not in this set —
// it has no header channel, so config.yaml stays canonical there.
var cloudProfileNames = map[string]struct{}{
	"hamrpass": {},
}

// IsCloudProfile reports whether a profile is one whose context_size is
// server-managed.
func IsCloudProfile(name string) bool {
	_, ok := cloudProfileNames[name]
	return ok
}

// managedProfiles are the two profiles codehamr always guarantees in
// config.yaml: a local Ollama target and the hosted hamrpass endpoint
// (with empty key — the user pastes their hamrpass key via /hamrpass).
// If the user manually deletes either entry, Bootstrap re-adds it on the
// next start with these canonical values and rewrites config.yaml so the
// file looks freshly seeded. Existing entries are never touched, so user
// keys / overrides survive across runs. hamrpass intentionally has
// ContextSize=0 — combined with omitempty on the yaml tag, that keeps
// the field out of config.yaml so users don't try to tune what the
// server already manages.
var managedProfiles = map[string]Profile{
	"local": {
		LLM:         "qwen3.6:27b",
		URL:         "http://localhost:11434",
		Key:         "",
		ContextSize: defaultContextSize,
	},
	"hamrpass": {
		LLM: "hamrpass",
		URL: "https://codehamr.com",
		Key: "",
	},
}

type MCPServer struct {
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Enabled     bool              `yaml:"enabled"`
	Description string            `yaml:"description,omitempty"`
}

// Profile is one named model endpoint in config.yaml. Users can have any
// number; `/models` switches between them. ContextSize is omitempty so
// cloud profiles (server-managed window via X-Context-Window) leave the
// field absent on disk; user-managed Ollama-style profiles set it to a
// concrete value and it round-trips normally.
type Profile struct {
	LLM         string `yaml:"llm"`
	URL         string `yaml:"url"`
	Key         string `yaml:"key"`
	ContextSize int    `yaml:"context_size,omitempty"`
}

// Config is the on-disk schema at .codehamr/config.yaml. Unknown top-level
// keys cause Bootstrap to fail (strict YAML decoding) so typos and stale
// schemas surface immediately rather than being silently ignored.
type Config struct {
	Active     string               `yaml:"active"`
	Models     map[string]*Profile  `yaml:"models"`
	MCPServers map[string]MCPServer `yaml:"mcp_servers,omitempty"`
	// Logging, when true, writes a fresh .codehamr/log.txt on every start
	// and appends every chat exchange to it. Debug instrumentation —
	// removable by deleting this field, internal/tui/debuglog.go, and the
	// few dbgWrite call sites.
	Logging bool `yaml:"logging,omitempty"`
	// runtime-only (not serialized)
	Dir string `yaml:"-"`
	// URLOverride, if set, takes precedence over ActiveProfile().URL
	// everywhere the client dials out. Kept separate from the Profile map
	// so a runtime override (CODEHAMR_URL) never round-trips into Save().
	URLOverride string `yaml:"-"`
}

func Default() *Config {
	models := make(map[string]*Profile, len(managedProfiles))
	for name, p := range managedProfiles {
		cp := p
		models[name] = &cp
	}
	return &Config{
		Active: "local",
		Models: models,
		MCPServers: map[string]MCPServer{
			"context7": {
				Command:     "npx",
				Args:        []string{"-y", "@upstash/context7-mcp@latest"},
				Enabled:     true,
				Description: "Documentation lookup",
			},
		},
	}
}

// Bootstrap returns the config for the current project, creating .codehamr/
// and config.yaml on first use. config.yaml is never overwritten. The system
// prompt is not written to disk — it's embedded.
func Bootstrap(projectRoot string) (*Config, bool, error) {
	dir := filepath.Join(projectRoot, DirName)
	created := false
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, err
		}
		created = true
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	var cfg *Config
	if b, err := os.ReadFile(cfgPath); err == nil {
		cfg = &Config{} // do NOT merge Default here — strict means strict
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, false, fmt.Errorf("config.yaml: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		cfg = Default()
		if err := writeYAML(cfgPath, cfg); err != nil {
			return nil, false, err
		}
	} else {
		return nil, false, err
	}
	cfg.Dir = dir

	// YAML `models: { name: ~ }` decodes to a nil *Profile entry, which would
	// panic on the ContextSize deref below. Reject up front so the error is
	// readable, not a stack trace.
	for name, p := range cfg.Models {
		if p == nil {
			return nil, false, fmt.Errorf("config.yaml: profile %q is empty — remove it or fill in the required fields", name)
		}
	}
	// Re-add any managed profile (local, hamrpass) the user has deleted
	// from config.yaml. Empty key on hamrpass is intentional — the user
	// pastes their own via /hamrpass. If anything was restored, persist
	// so the file on disk reflects the real set of profiles next time
	// the user looks at it.
	if cfg.Models == nil {
		cfg.Models = map[string]*Profile{}
	}
	restored := false
	for name, p := range managedProfiles {
		if _, ok := cfg.Models[name]; ok {
			continue
		}
		cp := p
		cfg.Models[name] = &cp
		restored = true
	}
	// Coerce any missing / zero / negative context_size to the default.
	// The packer subtracts fixed reservations and floors at 0, so a bogus
	// value would degenerate packing to "keep only the newest message" —
	// silently. Coerce up-front so nothing downstream has to defend.
	// Cloud profiles are exempt: their context_size is server-authoritative
	// and arrives via X-Context-Window on the first chat response. The TUI
	// keeps a safe runtime fallback for the brief window before that
	// response, so leaving the field empty here is correct.
	for name, p := range cfg.Models {
		if IsCloudProfile(name) {
			continue
		}
		if p.ContextSize <= 0 {
			p.ContextSize = defaultContextSize
		}
	}
	// Coerce Active if it points to a non-existent profile, picking the
	// first profile in sorted order (deterministic across runs).
	if _, ok := cfg.Models[cfg.Active]; !ok {
		cfg.Active = cfg.ModelNames()[0]
	}
	if restored {
		if err := writeYAML(cfgPath, cfg); err != nil {
			return nil, false, err
		}
	}

	return cfg, created, nil
}

// Save rewrites config.yaml.
func (c *Config) Save() error {
	if c.Dir == "" {
		return errors.New("config: Dir not set")
	}
	return writeYAML(filepath.Join(c.Dir, "config.yaml"), c)
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	header := []byte("# codehamr configuration\n\n")
	return os.WriteFile(path, append(header, b...), 0o644)
}

// ActiveProfile returns the currently-selected profile. Bootstrap guarantees
// that c.Active names a real profile, so this is a straight map lookup.
func (c *Config) ActiveProfile() *Profile {
	return c.Models[c.Active]
}

// ActiveURL is the endpoint every dial-out should use: the runtime override
// if set, otherwise the active profile's URL. Use this instead of reading
// ActiveProfile().URL directly so CODEHAMR_URL doesn't leak back into Save.
func (c *Config) ActiveURL() string {
	if c.URLOverride != "" {
		return c.URLOverride
	}
	return c.ActiveProfile().URL
}

// ModelNames returns the profile names in sorted order. Sorted so the
// popover cycles deterministically regardless of Go's map iteration order.
func (c *Config) ModelNames() []string {
	return slices.Sorted(maps.Keys(c.Models))
}

// MCPServerNames returns enabled+disabled MCP server names in sorted order.
// Same rationale as ModelNames — callers that iterate for display need a
// deterministic order.
func (c *Config) MCPServerNames() []string {
	return slices.Sorted(maps.Keys(c.MCPServers))
}

// SetActive switches the active profile and persists. Fails if name is
// unknown — no silent "hope you meant this" coercion.
func (c *Config) SetActive(name string) error {
	if _, ok := c.Models[name]; !ok {
		return fmt.Errorf("unknown model: %s", name)
	}
	c.Active = name
	return c.Save()
}
