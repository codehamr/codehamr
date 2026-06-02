package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapWritesSandboxHintHeader pins the comment header writeYAML
// re-prepends on every write. yaml.Marshal drops comments, so this is the only
// place the host.docker.internal hint survives, the #1 first-run footgun for
// devcontainer/WSL2 users. Guards against a switch to plain yaml.Marshal.
func TestBootstrapWritesSandboxHintHeader(t *testing.T) {
	dir := t.TempDir()
	if _, created, err := Bootstrap(dir); err != nil || !created {
		t.Fatalf("Bootstrap should create config on first run: created=%v err=%v", created, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DirName, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codehamr configuration", "host.docker.internal"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config.yaml header missing %q:\n%s", want, raw)
		}
	}
}

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
	// PROMPT_SYS is embedded, it must never touch disk.
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
	if p.URL != "http://localhost:11434" || p.LLM != "qwen3.6:27b" || p.ContextSize != 32768 {
		t.Fatalf("default local profile mismatch: %+v", p)
	}
	hp, ok := cfg.Models["hamrpass"]
	if !ok {
		t.Fatal("default should include a 'hamrpass' profile")
	}
	// hamrpass ContextSize=0, server-authoritative via X-Context-Window,
	// kept out of config.yaml by omitempty + Coerce skip.
	if hp.URL != "https://codehamr.com" || hp.LLM != "hamrpass" || hp.Key != "" || hp.ContextSize != 0 {
		t.Fatalf("default hamrpass profile mismatch: %+v", hp)
	}
}

// TestBootstrapHamrpassHasNoContextSizeOnDisk: a fresh config.yaml must carry
// no context_size for hamrpass: that field is server-authoritative via
// X-Context-Window. Guaranteed by the omitempty tag plus the IsCloudProfile
// skip in the Coerce loop.
func TestBootstrapHamrpassHasNoContextSizeOnDisk(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Bootstrap(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DirName, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Line scan, not a YAML re-decode: the question is whether the literal
	// field is gone from disk, which is what omitempty controls. yaml.v3's
	// 4-space indent puts a profile's children at 8 spaces, siblings at 4.
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

// TestBootstrapDoesNotRestoreDeletedHamrpass: once config.yaml exists the user
// owns its profile list. A removed hamrpass stays gone across restarts
// (re-created only via /hamrpass), the file is not silently rewritten, and
// other profiles' customisations round-trip untouched.
func TestBootstrapDoesNotRestoreDeletedHamrpass(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: local
models:
  local:
    llm: local-model
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
	// No spurious rewrite, Bootstrap's job here is to read, not tidy.
	// mtime is the cheapest signal.
	afterStat, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatal("config.yaml was rewritten by Bootstrap on a clean read path")
	}
}

// TestBootstrapDoesNotRestoreRenamedLocal: renaming `local` (e.g. to `ollama`)
// must not resurrect a duplicate `local` on next start. Same invariant as the
// deleted-hamrpass case, for the other managed profile.
func TestBootstrapDoesNotRestoreRenamedLocal(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: ollama
models:
  ollama:
    llm: local-model
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

// TestEnsureHamrpassLazyCreates: with hamrpass hidden from config.yaml,
// EnsureHamrpass returns a profile from the canonical seed values, and is
// idempotent: twice returns the same pointer.
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
		t.Fatal("EnsureHamrpass should be idempotent: second call must return the same pointer")
	}
}

// TestBootstrapPreservesExistingHamrpassKey: a user-supplied hamrpass key must
// round-trip untouched. Guards against any future "tidy on read" that would
// mutate existing entries.
func TestBootstrapPreservesExistingHamrpassKey(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: hamrpass
models:
  local:
    llm: local-model
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

// TestBootstrapLoadsMultipleProfiles: a two-profile config round-trips and
// Bootstrap picks the declared `active`.
func TestBootstrapLoadsMultipleProfiles(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`active: work
models:
  home:
    llm: local-model
    url: http://llm:11434
    key: ""
    context_size: 65536
  work:
    llm: cloud-model
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
	// Bootstrap must not inject local/hamrpass on top of the declared profiles.
	if len(cfg.Models) != 2 {
		t.Fatalf("expected exactly the two declared profiles, got %d: %+v", len(cfg.Models), cfg.Models)
	}
	for _, name := range []string{"home", "work"} {
		if _, ok := cfg.Models[name]; !ok {
			t.Fatalf("expected profile %q in loaded config", name)
		}
	}
	p := cfg.ActiveProfile()
	if p.LLM != "cloud-model" || p.URL != "https://api.example/v1" || p.Key != "sk-abc" {
		t.Fatalf("active profile wrong: %+v", p)
	}
}

// TestConfigFilePermissionsAreOwnerOnly is the regression for a world-readable
// hamrpass key: /hamrpass stores the bearer token in plaintext, so any local
// user could cat it. Fresh-bootstrap and post-Save paths must both write 0o600.
func TestConfigFilePermissionsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, DirName, "config.yaml")
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("fresh config.yaml perms = %v, want 0o600 (key may leak to other local users)", got)
	}

	// Save() must keep 0o600; otherwise a /hamrpass write would widen perms
	// right after the user pasted a key.
	cfg.Models["hamrpass"].Key = "hp-secret-12345678"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Mode().Perm(); got != 0o600 {
		t.Fatalf("Save() widened config.yaml perms to %v (must stay 0o600)", got)
	}

	// The .codehamr/ dir mustn't be world-listable either: even with a 0o600
	// config.yaml, a listable parent leaks the key's existence and invites probing.
	parentSt, err := os.Stat(filepath.Join(dir, DirName))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentSt.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf(".codehamr/ dir perms = %v - must not grant any other-user bits", got)
	}
}

// TestSaveIsAtomicAndLeavesNoTemp: writeYAML writes a sibling temp then renames
// it over config.yaml so a torn write can't brick the next launch. Pin that the
// rename leaves no leftover .config-*.yaml temp and the result still decodes.
// A regression here would mean the atomic-write path leaks temps or wrote junk.
func TestSaveIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, DirName)
	// Save a few times, each must rename cleanly with no temp accumulation.
	for i := range 3 {
		cfg.Models["hamrpass"].Key = fmt.Sprintf("hp-key-%d-0000000000", i)
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(cdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("Save left a temp file behind: %s", e.Name())
		}
	}
	// The committed file must still be a valid, re-decodable config.
	reloaded, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatalf("config.yaml not decodable after atomic Save: %v", err)
	}
	if reloaded.Models["hamrpass"].Key != "hp-key-2-0000000000" {
		t.Fatalf("last Save not durable: key = %q", reloaded.Models["hamrpass"].Key)
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

// TestSetActiveRevertsOnSaveFailure guards in-memory/on-disk drift on Save
// failure. If SetActive mutates Active before a failed Save, ActiveProfile()
// reads the wrong endpoint while config.yaml still names the old profile, and
// restart silently undoes the switch. SetActive must roll back on Save failure
// so both views stay in lockstep.
func TestSetActiveRevertsOnSaveFailure(t *testing.T) {
	cfg := &Config{
		Active: "a",
		Models: map[string]*Profile{
			"a": {LLM: "ma"},
			"b": {LLM: "mb"},
		},
		// Dir intentionally empty so Save() fails with "Dir not set".
	}
	err := cfg.SetActive("b")
	if err == nil {
		t.Fatal("precondition: Save with empty Dir must fail")
	}
	if cfg.Active != "a" {
		t.Fatalf("Active mutated to %q despite Save failure - in-memory state diverges from on-disk", cfg.Active)
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

// TestBootstrapCoercesUnknownActive: an unknown `active:` is coerced to the
// first profile in sorted order so ActiveProfile never returns nil.
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

// TestBootstrapRejectsEmptyModels: an empty `models:` block leaves Active
// nothing to point at; Bootstrap must error readably (how to recover) rather
// than panic in the Active coercer.
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
// must fail loud, not be silently ignored: surfaces typos immediately.
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

// TestBootstrapCoercesBogusContextSize: context_size 0 (or missing) is coerced
// to the default rather than degrading Pack() to "newest message only".
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

// TestBootstrapRejectsNilProfile: `models: { local: ~ }` decodes to a nil
// *Profile the ContextSize coercion loop would deref-panic on. Bootstrap must
// reject it with a readable error instead.
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

// TestBootstrapRefusesSymlinkedDir: a co-tenant could plant .codehamr → an
// attacker-controlled dir before first run. Bootstrap must Lstat (not Stat) and
// refuse any symlink: even with a 0o600 config.yaml, the attacker owns the
// parent and can swap or read what codehamr writes. Same defence for a planted
// config.yaml symlink.
func TestBootstrapRefusesSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, DirName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, _, err := Bootstrap(root)
	if err == nil {
		t.Fatal("Bootstrap accepted a symlinked .codehamr - config-injection vector left open")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should name the symlink defence: %v", err)
	}
	// Target must stay untouched, nothing dropped into the attacker-controlled dir.
	if _, err := os.Stat(filepath.Join(target, "config.yaml")); err == nil {
		t.Fatal("Bootstrap wrote into the symlink target despite the rejection")
	}
}

func TestBootstrapRefusesSymlinkedConfigYAML(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant config.yaml as a symlink pointing outside the project.
	target := filepath.Join(t.TempDir(), "external.yaml")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, cfgPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, _, err := Bootstrap(root)
	if err == nil {
		t.Fatal("Bootstrap accepted a symlinked config.yaml")
	}
	// Attacker target must not be clobbered with the seed.
	if _, err := os.Stat(target); err == nil {
		t.Fatal("Bootstrap wrote through the config.yaml symlink - seed bytes landed at attacker target")
	}
}

func TestBootstrapRefusesNonDirectoryAtCodehamrPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DirName), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Bootstrap(root)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Bootstrap should refuse a regular-file at .codehamr, got %v", err)
	}
}

// TestURLOverrideDoesNotPersist: a CODEHAMR_URL override lives in
// cfg.URLOverride and ActiveURL reflects it, but Save writes only the stored
// URL, so re-bootstrapping without the env var restores the original endpoint.
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

// TestSaveTightensPreexistingLoosePerms covers the upgrade path fresh-bootstrap
// misses: a config.yaml from an older world-readable codehamr (or a hand-edit)
// starts at 0o644, and os.WriteFile preserves an existing file's mode, so Save
// would rewrite the bytes (including a fresh hamrpass token) while leaving it
// world-readable. Save must tighten a pre-existing loose file to 0o600.
func TestSaveTightensPreexistingLoosePerms(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cdir, "config.yaml")
	// World-readable file an older codehamr would have written.
	loose := []byte("active: local\nmodels:\n  local:\n    llm: m\n    url: http://x\n    key: \"\"\n    context_size: 1\n")
	if err := os.WriteFile(cfgPath, loose, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A /hamrpass write lands the bearer token in this very file.
	cfg.EnsureHamrpass().Key = "hp-secret-1234567890abcdef"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("Save() must tighten a pre-existing 0o644 config.yaml to 0o600, got %v - hamrpass key stays world-readable across an upgrade", got)
	}
}
