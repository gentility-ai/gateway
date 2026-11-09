package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var refShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (r *rig) ref(index string) string {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	return r.w.logins[index].LoginRef
}

func keyFile(w *watcher) string { return filepath.Join(filepath.Dir(w.file), refKeyFile) }

func TestEachLoginCarriesAnOpaqueRefInTheRecord(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	codex, claude := r.ref("cx1"), r.ref("cl1")
	if !refShape.MatchString(codex) || !refShape.MatchString(claude) {
		t.Fatalf("refs %q %q", codex, claude)
	}
	// Both files sign in as the same seeded account: the provider tells them apart.
	if codex == claude {
		t.Fatalf("a Codex and a Claude login share the ref %s", codex)
	}
	var record struct {
		Logins []struct {
			AuthIndex string `json:"auth_index"`
			LoginRef  string `json:"login_ref"`
		}
	}
	if err := json.Unmarshal(r.w.record(), &record); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range record.Logins {
		got[l.AuthIndex] = l.LoginRef
	}
	if got["cx1"] != codex || got["cl1"] != claude {
		t.Fatalf("the record route shows %v", got)
	}
}

func TestARefFollowsTheAccountNotTheAuthIndex(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	before := r.ref("cx1")
	r.host.codexIndex = "cx7"
	r.sweepAt(time.Minute)
	if r.w.logins["cx1"] != nil || r.ref("cx7") != before {
		t.Fatalf("after the auth_index moved the ref is %q, was %q", r.ref("cx7"), before)
	}
}

func TestTwoSeatsOfOneCodexWorkspaceHaveTwoRefs(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	alice := r.ref("cx1")
	r.host.user = "user-OTHER-0005"
	r.sweepAt(12 * time.Minute)
	if bob := r.ref("cx1"); bob == alice || !refShape.MatchString(bob) {
		t.Fatalf("another seat on the same workspace has the ref %q, the first %q", bob, alice)
	}
}

func TestACodexRefReadsTheTokensAccountBeforeTheFiles(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	before := r.ref("cx1")
	r.host.fileAccount = "acct-OTHER-0006"
	r.sweepAt(12 * time.Minute)
	if r.ref("cx1") != before {
		t.Fatalf("the file's account_id moved the ref while the token's claim held")
	}
	r.host.account = "-"
	r.sweepAt(24 * time.Minute)
	if after := r.ref("cx1"); after == before || !refShape.MatchString(after) {
		t.Fatalf("without the claim the file's account_id was not read: %q", after)
	}
}

func TestAClaudeRefReadsTheAccountUUID(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	codex, claude := r.ref("cx1"), r.ref("cl1")
	r.host.claudeAccount = "uuid-OTHER-0007"
	r.sweepAt(12 * time.Minute)
	if r.ref("cl1") == claude || r.ref("cx1") != codex {
		t.Fatalf("a new account_uuid moved Claude's ref to %q (was %q) and Codex's to %q (was %q)",
			r.ref("cl1"), claude, r.ref("cx1"), codex)
	}
}

func TestAFailedReadKeepsTheRef(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	before := r.ref("cx1")
	r.host.refuseGet = true
	r.sweepAt(12 * time.Minute)
	if r.ref("cx1") != before {
		t.Fatalf("a refused read moved the ref to %q", r.ref("cx1"))
	}
}

func TestAnotherAccountDropsTheLastOnesNumbers(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	if len(r.w.logins["cx1"].Windows) == 0 {
		t.Fatal("the first poll kept no windows")
	}
	r.host.user = "user-OTHER-0005"
	r.usage.status["codex"] = http.StatusInternalServerError
	r.sweepAt(12 * time.Minute)
	if l := r.w.logins["cx1"]; len(l.Windows) != 0 || l.Plan != "" || l.Credits != nil || l.Resets != nil {
		t.Fatalf("the new account carries the last one's numbers: %+v", l)
	}
}

func TestTheKeyIsMadeOnceKeptPrivateAndReadAgainOnRestart(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	info, err := os.Stat(keyFile(r.w))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != refKeySize {
		t.Fatalf("key file %v, %v", info, err)
	}
	before := r.ref("cx1")
	r.w = r.watcher(t, r.w.file)
	r.sweepAt(time.Hour)
	if r.ref("cx1") != before {
		t.Fatalf("after a restart the ref is %q, was %q", r.ref("cx1"), before)
	}
}

func TestDeletingTheKeyStartsNewRefs(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	before := r.ref("cx1")
	if err := os.Remove(keyFile(r.w)); err != nil {
		t.Fatal(err)
	}
	r.w = r.watcher(t, r.w.file)
	r.sweepAt(time.Hour)
	if after := r.ref("cx1"); after == before || !refShape.MatchString(after) {
		t.Fatalf("with a new key the ref is %q, was %q", after, before)
	}
}

func TestWithoutAUsableKeyNoLoginHasARef(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(keyFile(r.w), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.w = r.watcher(t, r.w.file)
	r.sweepAt(0)
	if r.w.refKey != nil || r.ref("cx1") != "" || r.ref("cl1") != "" {
		t.Fatalf("key %x, refs %q %q", r.w.refKey, r.ref("cx1"), r.ref("cl1"))
	}
	if !strings.Contains(r.logs.String(), "no login_ref key") {
		t.Fatalf("the missing key was not logged:\n%s", r.logs)
	}
	if bytes.Contains(r.w.record(), []byte("login_ref")) {
		t.Fatalf("the record carries a login_ref:\n%s", r.w.record())
	}
	if r.poll("cx1").State != pollOK {
		t.Fatalf("the login was not polled without a key: %+v", r.poll("cx1"))
	}
}

func TestALoginFileWithoutAnAccountHasNoRefAndSaysSoOnce(t *testing.T) {
	r := newRig(t)
	r.host.account, r.host.fileAccount, r.host.claudeAccount = "-", "-", "-"
	r.sweepAt(0)
	r.sweepAt(12 * time.Minute)
	if r.ref("cx1") != "" || r.ref("cl1") != "" {
		t.Fatalf("refs %q %q", r.ref("cx1"), r.ref("cl1"))
	}
	if n := strings.Count(r.logs.String(), "codex login cx1 names no account"); n != 1 {
		t.Fatalf("the missing account was logged %d times:\n%s", n, r.logs)
	}
}
