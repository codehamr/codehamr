// Package config owns the .codehamr/ directory: config.yaml plus the
// embedded default system prompt. The prompt lives only in the binary,
// never on disk, so it's untamperable and every release ships it consistent.
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

// defaultContextSize is the local profile's packing budget and the floor
// Bootstrap coerces a bogus/missing context_size to. It must match what a stock
// local server actually honors, NOT the model's theoretical max: Ollama's /v1
// shim reports no X-Context-Window, so codehamr packs to this value blind: set
// it too high and the server silently front-truncates the prompt, dropping the
// embedded system prompt and early tool results with no error. 32k is the safe
// stock-Ollama tier and the seeded local model's native window. Users who raise
// their server's num_ctx (OLLAMA_CONTEXT_LENGTH; see README) lift this to match.
const defaultContextSize = 32768

// cloudProfileNames are profiles whose context_size the server sets via the
// X-Context-Window header. We leave their on-disk context_size empty:
// Bootstrap won't seed it, coercion won't default it, and the TUI reads the
// live value per response. Local Ollama has no header channel, so config.yaml
// stays canonical there.
var cloudProfileNames = map[string]struct{}{
	"hamrpass": {},
}

// IsCloudProfile reports whether a profile's context_size is server-managed.
func IsCloudProfile(name string) bool {
	_, ok := cloudProfileNames[name]
	return ok
}

// managedProfiles are seeded on first run: a local Ollama target and the
// hosted hamrpass endpoint (empty key, since /hamrpass pastes it, re-creating
// the entry from this seed if the user deleted it). After first run config.yaml
// is the user's: deletions and renames stick, Bootstrap never re-adds anything.
// hamrpass keeps ContextSize=0 so omitempty drops it from disk: users can't
// tune what the server already manages.
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

// Profile is one named model endpoint in config.yaml; `/models` switches
// between them. ContextSize is omitempty so server-managed cloud profiles omit
// it on disk while user-managed profiles round-trip a concrete value.
type Profile struct {
	LLM         string `yaml:"llm"`
	URL         string `yaml:"url"`
	Key         string `yaml:"key"`
	ContextSize int    `yaml:"context_size,omitempty"`
}

// Config is the on-disk schema at .codehamr/config.yaml. Strict decoding:
// unknown top-level keys fail Bootstrap so typos and stale schemas surface
// immediately rather than being silently ignored.
type Config struct {
	Active string              `yaml:"active"`
	Models map[string]*Profile `yaml:"models"`
	// Logging writes a fresh log.txt each start and appends every exchange.
	// Debug instrumentation; removable with this field, debuglog.go, and the
	// dbgWrite call sites.
	Logging bool `yaml:"logging,omitempty"`
	// runtime-only (not serialized)
	Dir string `yaml:"-"`
	// URLOverride, if set, wins over ActiveProfile().URL everywhere we dial
	// out. Kept off the Profile map so the runtime CODEHAMR_URL override never
	// round-trips into Save().
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
	}
}

// Bootstrap returns the config for the current project, creating .codehamr/
// and config.yaml on first use. config.yaml is never overwritten; the prompt
// is embedded, never written to disk.
//
// The directory check uses Lstat (not Stat) and refuses a pre-existing
// .codehamr that isn't a real directory: a symlink there would let a co-tenant
// redirect config.yaml to an attacker path, planting a models.<name>.url that
// proxies the hamrpass key on the next dial-out.
func Bootstrap(projectRoot string) (*Config, bool, error) {
	dir := filepath.Join(projectRoot, DirName)
	created := false
	info, err := os.Lstat(dir)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%s: refuses to follow symlink, remove or replace with a real directory", dir)
		}
		if !info.IsDir() {
			return nil, false, fmt.Errorf("%s: exists but is not a directory", dir)
		}
	case errors.Is(err, os.ErrNotExist):
		// 0o700: config.yaml may carry the hamrpass key (a long-lived bearer
		// token). A world-listable dir lets other local users spot it and probe
		// for the key. Only the project owner should read here.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, err
		}
		created = true
	default:
		return nil, false, err
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	// Same symlink defence as the directory check: a symlinked config.yaml
	// could redirect the read (which config we honour) or the write (clobbering
	// an arbitrary user-writable file with the seed). Refuse with a clear error.
	if li, err := os.Lstat(cfgPath); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s: refuses to follow symlink, remove or replace with a real file", cfgPath)
	}
	var cfg *Config
	if b, err := os.ReadFile(cfgPath); err == nil {
		cfg = &Config{} // do NOT merge Default here; strict means strict
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

	// YAML `models: { name: ~ }` decodes to a nil *Profile that would panic on
	// the ContextSize deref below. Reject up front for a readable error.
	for name, p := range cfg.Models {
		if p == nil {
			return nil, false, fmt.Errorf("config.yaml: profile %q is empty; remove it or fill in the required fields", name)
		}
	}
	// Coerce missing/zero/negative context_size to the default. The packer
	// subtracts fixed reservations and floors at 0, so a bogus value would
	// silently degenerate packing to "keep only the newest message". Coerce
	// here so nothing downstream has to defend. Cloud profiles are exempt:
	// their size arrives via X-Context-Window on the first response, and the
	// TUI holds a runtime fallback until then.
	for name, p := range cfg.Models {
		if IsCloudProfile(name) {
			continue
		}
		if p.ContextSize <= 0 {
			p.ContextSize = defaultContextSize
		}
	}
	// Coerce a dangling Active to the first profile in sorted order
	// (deterministic). With no profiles at all, fail loud, since runtime would
	// otherwise nil-deref on the first dial-out.
	if _, ok := cfg.Models[cfg.Active]; !ok {
		names := cfg.ModelNames()
		if len(names) == 0 {
			return nil, false, errors.New("config.yaml: no profiles configured; add one under `models:` or delete .codehamr/config.yaml to reseed defaults")
		}
		cfg.Active = names[0]
	}

	return cfg, created, nil
}

// EnsureHamrpass returns the hamrpass profile, re-creating it from the seed if
// the user deleted it. Lets /hamrpass activate by pasting a key without a
// restart detour.
func (c *Config) EnsureHamrpass() *Profile {
	if hp, ok := c.Models["hamrpass"]; ok {
		return hp
	}
	tmpl := managedProfiles["hamrpass"]
	if c.Models == nil {
		c.Models = map[string]*Profile{}
	}
	c.Models["hamrpass"] = &tmpl
	return c.Models["hamrpass"]
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
	// Re-prepended every Save since yaml.Marshal drops free-form comments, the
	// only place a hint survives. The sandbox line catches the top first-run
	// footgun: in a devcontainer/WSL2 with Ollama on the host, `localhost`
	// doesn't reach the host and yields a baffling "connection refused". Native
	// users aren't affected, hence sandbox-vs-host framing over an OS-specific one.
	header := []byte(`# codehamr configuration
#
# Running codehamr in a devcontainer / WSL2 with Ollama on the host:
# swap 'http://localhost:11434' with 'http://host.docker.internal:11434' below.
#
# context_size is what codehamr packs to - set it to your server's ACTUAL window,
# not the model's theoretical max. For Ollama that's OLLAMA_CONTEXT_LENGTH (or a
# Modelfile 'PARAMETER num_ctx'); too high and the server silently drops the
# oldest messages. More VRAM lets you raise both together.

`)
	// Write to a sibling temp then rename over config.yaml. Rename is atomic
	// within the directory, so a crash, signal, or full disk mid-write can never
	// leave a truncated config.yaml, which Bootstrap's strict decode would fatal
	// on, bricking the next launch until the file is hand-deleted. Mirrors
	// internal/update's promote-by-rename. os.CreateTemp makes the temp 0o600 and
	// rename installs that fresh inode in place, so this also closes the
	// upgrade-path leak the old in-place write needed a trailing Chmod for:
	// config.yaml carries the hamrpass key, and only the project owner should
	// read it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename; cleans up early returns
	if _, err := tmp.Write(append(header, b...)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ActiveProfile returns the selected profile. Bootstrap guarantees c.Active
// names a real one, so this is a straight map lookup.
func (c *Config) ActiveProfile() *Profile {
	return c.Models[c.Active]
}

// ActiveURL is the endpoint every dial-out uses: the runtime override if set,
// else the active profile's URL. Use this over ActiveProfile().URL so
// CODEHAMR_URL doesn't leak back into Save.
func (c *Config) ActiveURL() string {
	if c.URLOverride != "" {
		return c.URLOverride
	}
	return c.ActiveProfile().URL
}

// ModelNames returns the profile names sorted, so the popover cycles
// deterministically regardless of map iteration order.
func (c *Config) ModelNames() []string {
	return slices.Sorted(maps.Keys(c.Models))
}

// SetActive switches the active profile and persists. Fails on an unknown name,
// no silent coercion. On Save failure it reverts in-memory Active so the live
// model and config.yaml stay in lockstep; otherwise the switch would stick this
// session but vanish on the next Bootstrap.
func (c *Config) SetActive(name string) error {
	if _, ok := c.Models[name]; !ok {
		return fmt.Errorf("unknown model: %s", name)
	}
	prev := c.Active
	c.Active = name
	if err := c.Save(); err != nil {
		c.Active = prev
		return err
	}
	return nil
}
