// Command bundle writes, signs and verifies a gateway bundle's manifest, as
// docs/bundle-contract.md describes it. The standard library only.
//
//	bundle manifest -dir D -upstream-version V -upstream-archive A -plugin-version V -commit C -build URL [-changelog F]
//	bundle sign -dir D                      (key: base64 seed in $GATEWAY_SIGNING_KEY)
//	bundle verify -dir D -keys keys.json
//	bundle keygen -private F                (seed to F, mode 0600; public entry to stdout)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const pluginFile = "gentility-usage.so"

type manifest struct {
	Kind     string `json:"kind"`
	Format   int    `json:"format"`
	Bundle   string `json:"bundle"`
	Upstream struct {
		Repo    string `json:"repo"`
		Version string `json:"version"`
		Archive string `json:"archive"`
		SHA256  string `json:"sha256"`
	} `json:"upstream"`
	Plugin struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
		File    string `json:"file"`
		SHA256  string `json:"sha256"`
	} `json:"plugin"`
	BuiltAt   string          `json:"built_at"`
	Build     string          `json:"build"`
	Changelog json.RawMessage `json:"changelog"`
}

type signature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: bundle manifest|sign|verify|keygen …"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("dir", "", "the bundle's directory")
	var err error
	switch os.Args[1] {
	case "manifest":
		uv := fs.String("upstream-version", "", "")
		ua := fs.String("upstream-archive", "", "")
		pv := fs.String("plugin-version", "", "")
		commit := fs.String("commit", "", "")
		build := fs.String("build", "", "")
		changelog := fs.String("changelog", "", "a JSON file; empty for none")
		fs.Parse(os.Args[2:])
		err = writeManifest(*dir, *uv, *ua, *pv, *commit, *build, *changelog, time.Now().UTC())
	case "sign":
		fs.Parse(os.Args[2:])
		err = sign(*dir, os.Getenv("GATEWAY_SIGNING_KEY"))
	case "verify":
		keys := fs.String("keys", "", "the pinned key set")
		fs.Parse(os.Args[2:])
		var m manifest
		if m, err = verify(*dir, *keys); err == nil {
			fmt.Printf("verified %s\n", m.Bundle)
		}
	case "keygen":
		private := fs.String("private", "", "where to write the seed")
		fs.Parse(os.Args[2:])
		err = keygen(*private, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bundle:", err)
	os.Exit(1)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeManifest(dir, upstreamVersion, archive, pluginVersion, commit, build, changelog string, now time.Time) error {
	m := manifest{Kind: "gateway", Format: 1, Bundle: upstreamVersion + "-" + pluginVersion, BuiltAt: now.Format(time.RFC3339), Build: build}
	m.Upstream.Repo, m.Upstream.Version, m.Upstream.Archive = "router-for-me/CLIProxyAPI", upstreamVersion, archive
	m.Plugin.ID, m.Plugin.Version, m.Plugin.Commit, m.Plugin.File = "gentility-usage", pluginVersion, commit, pluginFile
	var err error
	if m.Upstream.SHA256, err = fileSHA(filepath.Join(dir, archive)); err != nil {
		return err
	}
	if m.Plugin.SHA256, err = fileSHA(filepath.Join(dir, pluginFile)); err != nil {
		return err
	}
	m.Changelog = json.RawMessage("[]")
	if changelog != "" {
		raw, err := os.ReadFile(changelog)
		if err != nil {
			return err
		}
		if !json.Valid(raw) {
			return errors.New("the changelog is not JSON")
		}
		m.Changelog = raw
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644)
}

func keyID(public ed25519.PublicKey) string { return hex.EncodeToString(public[:8]) }

func sign(dir, seed string) error {
	raw, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return errors.New("GATEWAY_SIGNING_KEY is not a base64 Ed25519 seed")
	}
	key := ed25519.NewKeyFromSeed(raw)
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	sig, _ := json.Marshal(signature{KeyID: keyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))})
	return os.WriteFile(filepath.Join(dir, "manifest.sig"), append(sig, '\n'), 0o644)
}

// verify checks the signature against the pinned set, then the kind and
// format, then every file's sha256: the contract's order.
func verify(dir, keysPath string) (manifest, error) {
	var m manifest
	var set struct {
		Keys map[string]string `json:"keys"`
	}
	raw, err := os.ReadFile(keysPath)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return m, fmt.Errorf("the key set: %w", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	var sig signature
	raw, err = os.ReadFile(filepath.Join(dir, "manifest.sig"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &sig); err != nil {
		return m, fmt.Errorf("manifest.sig: %w", err)
	}
	encoded, ok := set.Keys[sig.KeyID]
	if !ok {
		return m, fmt.Errorf("signed by key %q, which is not pinned", sig.KeyID)
	}
	public, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return m, fmt.Errorf("pinned key %q is not an Ed25519 public key", sig.KeyID)
	}
	signed, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil || !ed25519.Verify(public, body, signed) {
		return m, errors.New("the signature does not verify")
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, err
	}
	if m.Kind != "gateway" {
		return m, fmt.Errorf("kind %q, not gateway", m.Kind)
	}
	if m.Format != 1 {
		return m, fmt.Errorf("format %d is unknown", m.Format)
	}
	for _, f := range []struct{ name, want string }{{m.Upstream.Archive, m.Upstream.SHA256}, {m.Plugin.File, m.Plugin.SHA256}} {
		if filepath.Base(f.name) != f.name {
			return m, fmt.Errorf("file %q is not a bare name", f.name)
		}
		got, err := fileSHA(filepath.Join(dir, f.name))
		if err != nil {
			return m, err
		}
		if got != f.want {
			return m, fmt.Errorf("%s has sha256 %s, the manifest says %s", f.name, got, f.want)
		}
	}
	return m, nil
}

func keygen(privatePath string, out io.Writer) error {
	if privatePath == "" {
		return errors.New("-private names where the seed goes; it is never printed")
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(key.Seed())), 0o600); err != nil {
		return err
	}
	entry, _ := json.Marshal(map[string]string{keyID(public): base64.StdEncoding.EncodeToString(public)})
	_, err = fmt.Fprintln(out, string(entry))
	return err
}
