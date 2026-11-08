package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signed builds a signed bundle dir with a fresh key and returns the dir,
// the pinned key set's path and the seed.
func signed(t *testing.T) (dir, keys, seed string) {
	t.Helper()
	dir, scratch := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLIProxyAPI_8.0.60_linux_amd64.tar.gz"), []byte("upstream"), 0o644)
	os.WriteFile(filepath.Join(dir, pluginFile), []byte("plugin"), 0o644)
	if err := writeManifest(dir, "8.0.60", "CLIProxyAPI_8.0.60_linux_amd64.tar.gz", "0.2.0", "abc", "https://example.invalid/run", "", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	var entry bytes.Buffer
	seedPath := filepath.Join(scratch, "seed")
	if err := keygen(seedPath, &entry); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(seedPath)
	if err := sign(dir, string(raw)); err != nil {
		t.Fatal(err)
	}
	keys = filepath.Join(scratch, "keys.json")
	os.WriteFile(keys, []byte(`{"keys":`+strings.TrimSpace(entry.String())+`}`), 0o644)
	return dir, keys, string(raw)
}

func TestAGoodBundleVerifies(t *testing.T) {
	dir, keys, _ := signed(t)
	m, err := verify(dir, keys)
	if err != nil || m.Bundle != "8.0.60-0.2.0" {
		t.Fatalf("%+v %v", m, err)
	}
}

func rewriteManifest(t *testing.T, dir string, change func(map[string]any)) {
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m map[string]any
	json.Unmarshal(raw, &m)
	change(m)
	raw, _ = json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644)
}

func TestVerifyRefuses(t *testing.T) {
	cases := map[string]struct {
		spoil func(t *testing.T, dir, keys, seed string)
		want  string
	}{
		"an edited manifest": {func(t *testing.T, dir, _, _ string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["bundle"] = "9.9.9-0.2.0" })
		}, "does not verify"},
		"a key not pinned": {func(t *testing.T, _, keys, _ string) {
			os.WriteFile(keys, []byte(`{"keys":{}}`), 0o644)
		}, "not pinned"},
		"a swapped plugin": {func(t *testing.T, dir, _, _ string) {
			os.WriteFile(filepath.Join(dir, pluginFile), []byte("other"), 0o644)
		}, "has sha256"},
		"a swapped upstream archive": {func(t *testing.T, dir, _, _ string) {
			os.WriteFile(filepath.Join(dir, "CLIProxyAPI_8.0.60_linux_amd64.tar.gz"), []byte("other"), 0o644)
		}, "has sha256"},
		"another kind, re-signed": {func(t *testing.T, dir, _, seed string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["kind"] = "desk-host" })
			sign(dir, seed)
		}, "not gateway"},
		"an unknown format, re-signed": {func(t *testing.T, dir, _, seed string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["format"] = 2 })
			sign(dir, seed)
		}, "unknown"},
		"a path for a file name, re-signed": {func(t *testing.T, dir, _, seed string) {
			rewriteManifest(t, dir, func(m map[string]any) { m["plugin"].(map[string]any)["file"] = "../gentility-usage.so" })
			sign(dir, seed)
		}, "not a bare name"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, keys, seed := signed(t)
			c.spoil(t, dir, keys, seed)
			if _, err := verify(dir, keys); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestKeygenNeverPrintsTheSeed(t *testing.T) {
	var out bytes.Buffer
	seedPath := filepath.Join(t.TempDir(), "seed")
	if err := keygen(seedPath, &out); err != nil {
		t.Fatal(err)
	}
	seed, _ := os.ReadFile(seedPath)
	if strings.Contains(out.String(), string(seed)) {
		t.Fatal("the seed reached stdout")
	}
	if info, _ := os.Stat(seedPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("seed mode %v", info.Mode().Perm())
	}
}
