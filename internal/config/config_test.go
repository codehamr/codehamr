package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCreatesLayout(t *testing.T) {
	dir := t.TempDir()
	cfg, created, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("should report created=true on first bootstrap")
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, "config.yaml")); err != nil {
		t.Errorf("missing config.yaml: %v", err)
	}
	// PROMPT_SYS lives in the embed — it must never touch disk.
	if _, err := os.Stat(filepath.Join(dir, DirName, "PROMPT_SYS.md")); err == nil {
		t.Errorf("embedded PROMPT_SYS.md must not be written to disk")
	}
	if cfg.Active != "local" {
		t.Fatalf("default Active = %q, want local", cfg.Active)
	}
	p, ok := cfg.Models["local"]
	if !ok {
		t.Fatal("default should include a 'local' profile")
	}
	if p.URL != "http://localhost:11434" || p.LLM != "qwen3.6:27b" || p.ContextSize != 65536 {
		t.Fatalf("default local profile mismatch: %+v", p)
	}
	hp, ok := cfg.Models["hamrpass"]
	if !ok {
		t.Fatal("default should include a 'hamrpass' profile")
	}
	// hamrpass intentionally has ContextSize=0 — server-authoritative via
	// X-Context-Window, kept out of config.yaml by omitempty + Coerce skip.
	if hp.URL != "https://codehamr.com" || hp.LLM != "hamrpass" || hp.Key != "" || hp.ContextSize != 0 {
		t.Fatalf("default hamrpass profile mismatch: %+v", hp)
	}
}

// TestBootstrapHamrpassHasNoContextSizeOnDisk: a freshly bootstrapped
// project's config.yaml must not contain a context_size line for the
// hamrpass profile — that field is server-authoritative via the
// X-Context-Window response header. The omitempty yaml tag plus the
// IsCloudProfile skip in the Coerce loop guarantee this.
func TestBootstrapHamrpassHasNoContextSizeOnDisk(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Bootstrap(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DirName, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Locate the hamrpass block by name and walk its scalar children,
	// asserting context_size is not among them. Cheap line scan rather
	// than a full YAML re-decode — the goal is "is the literal field
	// gone from disk", which is exactly what omitempty controls.
	// gopkg.in/yaml.v3 uses a 4-space indent by default; the children
	// of a profile sit at 8 spaces, the next sibling profile at 4.
	lines := strings.Split(string(raw), "\n")
	in := false
	for i, line := range lines {
		if strings.HasPrefix(line, "    hamrpass:") {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "        ") {
				break // next sibling profile
			}
			if strings.Contains(line, "context_size") {
				t.Fatalf("hamrpass profile must not carry context_size on disk, found at line %d:\n%s", i, line)
			}
		}
	}
	if !in {
		t.Fatal("hamrpass block not found in serialized config.yaml")
	}
}

// TestBootstrapDoesNotRestoreDeletedHamrpass: once config.yaml exists,
// the user owns its profile list. A removed hamrpass entry stays gone
// across restarts (re-creation only happens via /hamrpass), and the
// on-disk file is not silently rewritten. User customisations on other
// profiles round-trip untouched.
func TestBootstrapDoesNotRestoreDeletedHamrpass(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: local
models:
  local:
    llm: qwen3.6:27b
    url: http://host.docker.internal:11434
    key: ""
    context_size: 262144
  custom:
    llm: foo
    url: http://x
    key: sk-keep
    context_size: 8000
`)
	cfgPath := filepath.Join(cdir, "config.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Models["hamrpass"]; ok {
		t.Fatal("hamrpass was deleted from config.yaml; Bootstrap must not restore it")
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("expected exactly the two user profiles, got %d: %+v", len(cfg.Models), cfg.Models)
	}
	// User customisations must survive intact.
	if cfg.Models["local"].URL != "http://host.docker.internal:11434" || cfg.Models["local"].ContextSize != 262144 {
		t.Fatalf("local profile was mutated: %+v", cfg.Models["local"])
	}
	if cfg.Models["custom"].Key != "sk-keep" {
		t.Fatalf("custom profile was mutated: %+v", cfg.Models["custom"])
	}
	// No spurious rewrite of config.yaml — Bootstrap's only job here is
	// to read, not to "tidy". mtime is the cheapest signal.
	afterStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatal("config.yaml was rewritten by Bootstrap on a clean read path")
	}
}

// TestBootstrapDoesNotRestoreRenamedLocal: a user who renames `local` to
// e.g. `ollama` must not see a fresh `local` reappear as a duplicate on
// the next start. Same invariant as the deleted-hamrpass case, exercised
// for the other managed profile.
func TestBootstrapDoesNotRestoreRenamedLocal(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: ollama
models:
  ollama:
    llm: qwen3.6:27b
    url: http://localhost:11434
    key: ""
    context_size: 65536
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Models["local"]; ok {
		t.Fatal("renamed-away `local` must not be restored")
	}
	if _, ok := cfg.Models["hamrpass"]; ok {
		t.Fatal("hamrpass not declared in config.yaml; must not appear")
	}
	if len(cfg.Models) != 1 || cfg.Active != "ollama" {
		t.Fatalf("expected single 'ollama' profile active, got Active=%q models=%+v", cfg.Active, cfg.Models)
	}
}

// TestEnsureHamrpassLazyCreates: when the user has hidden hamrpass from
// config.yaml, EnsureHamrpass returns a profile populated from the
// canonical seed values. Idempotent — calling twice returns the same
// pointer.
func TestEnsureHamrpassLazyCreates(t *testing.T) {
	cfg := &Config{
		Active: "local",
		Models: map[string]*Profile{
			"local": {LLM: "m", URL: "http://x", Key: "", ContextSize: 65536},
		},
	}
	hp := cfg.EnsureHamrpass()
	if hp == nil {
		t.Fatal("EnsureHamrpass returned nil")
	}
	if hp.URL != "https://codehamr.com" || hp.LLM != "hamrpass" || hp.Key != "" {
		t.Fatalf("lazy-created hamrpass has wrong fields: %+v", hp)
	}
	if got := cfg.Models["hamrpass"]; got != hp {
		t.Fatal("EnsureHamrpass did not store the entry on cfg.Models")
	}
	hp2 := cfg.EnsureHamrpass()
	if hp2 != hp {
		t.Fatal("EnsureHamrpass should be idempotent — second call must return the same pointer")
	}
}

// TestBootstrapPreservesExistingHamrpassKey: a user-supplied key on the
// hamrpass entry must round-trip untouched. Trivially true now that
// Bootstrap doesn't mutate existing entries at all, but kept as a
// regression guard against any future "tidy on read" temptation.
func TestBootstrapPreservesExistingHamrpassKey(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: hamrpass
models:
  local:
    llm: qwen3.6:27b
    url: http://localhost:11434
    key: ""
    context_size: 65536
  hamrpass:
    llm: hamrpass
    url: https://codehamr.com
    key: hp-secret-1234567890abcdef
    context_size: 262144
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["hamrpass"].Key != "hp-secret-1234567890abcdef" {
		t.Fatalf("existing key was overwritten: %q", cfg.Models["hamrpass"].Key)
	}
}

// TestBootstrapLoadsMultipleProfiles: a user-authored config with two
// profiles round-trips and Bootstrap picks the declared `active`.
func TestBootstrapLoadsMultipleProfiles(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: work
models:
  home:
    llm: qwen3.5:27b
    url: http://llm:11434
    key: ""
    context_size: 65536
  work:
    llm: gpt-5.1
    url: https://api.example/v1
    key: sk-abc
    context_size: 200000
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "work" {
		t.Fatalf("Active = %q, want work", cfg.Active)
	}
	// User-authored config defines exactly these two profiles. Bootstrap
	// must not silently inject local/hamrpass on top.
	if len(cfg.Models) != 2 {
		t.Fatalf("expected exactly the two declared profiles, got %d: %+v", len(cfg.Models), cfg.Models)
	}
	for _, name := range []string{"home", "work"} {
		if _, ok := cfg.Models[name]; !ok {
			t.Fatalf("expected profile %q in loaded config", name)
		}
	}
	p := cfg.ActiveProfile()
	if p.LLM != "gpt-5.1" || p.URL != "https://api.example/v1" || p.Key != "sk-abc" {
		t.Fatalf("active profile wrong: %+v", p)
	}
}

// TestSetActivePersists: SetActive flips Active and writes config.yaml.
func TestSetActivePersists(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	// add a second profile so SetActive has somewhere to go
	cfg.Models["other"] = &Profile{LLM: "m", URL: "http://x", ContextSize: 1}
	if err := cfg.SetActive("other"); err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "other" {
		t.Fatalf("Active = %q, want other", cfg.Active)
	}
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Active != "other" {
		t.Fatal("Active did not persist")
	}
}

// TestSetActiveRejectsUnknown: SetActive returns an error for an unknown name.
func TestSetActiveRejectsUnknown(t *testing.T) {
	cfg := &Config{Active: "a", Models: map[string]*Profile{"a": {}}}
	if err := cfg.SetActive("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

// TestActiveProfileResolvesByName: the helper returns the right struct.
func TestActiveProfileResolvesByName(t *testing.T) {
	cfg := &Config{
		Active: "b",
		Models: map[string]*Profile{
			"a": {LLM: "m-a"},
			"b": {LLM: "m-b"},
		},
	}
	if cfg.ActiveProfile().LLM != "m-b" {
		t.Fatalf("ActiveProfile().LLM = %q, want m-b", cfg.ActiveProfile().LLM)
	}
}

// TestBootstrapCoercesUnknownActive: an unknown `active:` in config.yaml is
// coerced to the first profile in sorted order so the runtime never hits a
// nil ActiveProfile.
func TestBootstrapCoercesUnknownActive(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: ghost
models:
  zulu:
    llm: m
    url: http://z
    key: ""
    context_size: 1
  alpha:
    llm: m
    url: http://a
    key: ""
    context_size: 1
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active != "alpha" {
		t.Fatalf("unknown active should coerce to first sorted, got %q", cfg.Active)
	}
}

// TestBootstrapRejectsEmptyModels: a config.yaml with an empty `models:`
// block has nothing for Active to point at; Bootstrap must error out with
// a readable message rather than panic in the Active coercer. The user
// is told exactly how to recover (add a profile or delete the file).
func TestBootstrapRejectsEmptyModels(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("active: none\nmodels: {}\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(dir)
	if err == nil {
		t.Fatal("empty models map must be rejected, not silently coerced")
	}
	if !strings.Contains(err.Error(), "no profiles configured") {
		t.Fatalf("error should explain the problem, got: %v", err)
	}
}

// TestStrictYAMLRejectsUnknownKey: unknown top-level keys in config.yaml
// must fail loud, not be silently ignored — surfaces typos immediately.
func TestStrictYAMLRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte("active: local\nmodels: {local: {llm: m, url: http://x, key: '', context_size: 1}}\nmystery_key: 7\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Bootstrap(dir); err == nil {
		t.Fatal("expected Bootstrap to reject unknown top-level key")
	}
}

// TestBootstrapCoercesBogusContextSize: context_size of 0 (or missing) must
// be coerced to the default rather than silently degrading Pack() to
// "newest message only".
func TestBootstrapCoercesBogusContextSize(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: local
models:
  local:
    llm: m
    url: http://x
    key: ""
    context_size: 0
`)
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProfile().ContextSize != defaultContextSize {
		t.Fatalf("context_size=0 should be coerced to %d, got %d",
			defaultContextSize, cfg.ActiveProfile().ContextSize)
	}
}

// TestBootstrapRejectsNilProfile: `models: { local: ~ }` in YAML decodes to
// a nil *Profile entry; the ContextSize coercion loop would panic on the
// dereference. Bootstrap must reject the config with a readable error
// instead of a runtime stack trace.
func TestBootstrapRejectsNilProfile(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("active: local\nmodels:\n  local: ~\n")
	if err := os.WriteFile(filepath.Join(cdir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(dir)
	if err == nil {
		t.Fatal("nil YAML profile must be rejected (not panic on deref)")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Fatalf("error should name the offending profile, got: %v", err)
	}
}

// TestURLOverrideDoesNotPersist: a CODEHAMR_URL style override lives in
// cfg.URLOverride, ActiveURL reflects it, but Save writes only the
// on-disk URL so re-bootstrapping without the env var restores the
// original endpoint.
func TestURLOverrideDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	originalURL := cfg.ActiveProfile().URL
	cfg.URLOverride = "http://override:9999"
	if got := cfg.ActiveURL(); got != "http://override:9999" {
		t.Fatalf("ActiveURL() ignored override: %q", got)
	}
	if got := cfg.ActiveProfile().URL; got != originalURL {
		t.Fatalf("override leaked into stored profile: %q", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveProfile().URL != originalURL {
		t.Fatalf("Save persisted the override: %q", reloaded.ActiveProfile().URL)
	}
	if reloaded.URLOverride != "" {
		t.Fatalf("URLOverride round-tripped through YAML: %q", reloaded.URLOverride)
	}
}
