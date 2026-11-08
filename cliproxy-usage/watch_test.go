package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const (
	seededToken   = "tok-SEEDED-bearer"
	seededAccount = "acct-SEEDED-0001"
	seededEmail   = "seeded@example.com"
)

var refreshed = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// fakeHost answers host.auth.list and host.auth.get for one Codex and one
// Claude login, each file carrying the seeded secrets.
type fakeHost struct {
	mu          sync.Mutex
	lastRefresh time.Time
	// refuseGet makes host.auth.get fail the way upstream's file store does,
	// naming the login file.
	refuseGet bool
}

func (f *fakeHost) call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any
	switch method {
	case pluginabi.MethodHostAuthList:
		entry := func(index, provider string) map[string]any {
			return map[string]any{"auth_index": index, "provider": provider, "email": seededEmail, "account": seededAccount,
				"updated_at": time.Now()}
		}
		result = map[string]any{"files": []any{entry("cx1", "codex"), entry("cl1", "claude"), entry("k1", "openai-compatibility")}}
	case pluginabi.MethodHostAuthGet:
		if f.refuseGet {
			return json.Marshal(pluginabi.Envelope{Error: &pluginabi.Error{Code: "host_call_failed",
				Message: "failed to read auth file: open /var/lib/cliproxyapi/auth/codex-" + seededEmail + "-pro.json: permission denied"}})
		}
		var asked struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(request, &asked)
		claims, _ := json.Marshal(map[string]any{"email": seededEmail, "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": seededAccount}})
		idToken := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
		file, _ := json.Marshal(map[string]string{"access_token": seededToken, "refresh_token": "refresh-" + seededToken,
			"id_token": idToken, "account_id": seededAccount, "email": seededEmail, "last_refresh": f.lastRefresh.Format(time.RFC3339)})
		result = map[string]any{"auth_index": asked.AuthIndex, "json": json.RawMessage(file)}
	default:
		return nil, nil
	}
	raw, _ := json.Marshal(result)
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// usageEndpoint stands in for both usage endpoints: it answers with the
// fixtures, their redactions replaced by the seeded secrets, unless a test
// sets another status.
type usageEndpoint struct {
	mu         sync.Mutex
	status     map[string]int
	retryAfter string
	asked      map[string]int
	headers    map[string]http.Header
}

func (u *usageEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	provider := strings.TrimPrefix(r.URL.Path, "/")
	u.asked[provider]++
	u.headers[provider] = r.Header.Clone()
	if status := u.status[provider]; status != 0 && status != http.StatusOK {
		if u.retryAfter != "" {
			w.Header().Set("Retry-After", u.retryAfter)
		}
		w.WriteHeader(status)
		return
	}
	name := map[string]string{"codex": "codex-wham-usage.json", "claude": "claude-oauth-usage.json"}[provider]
	raw, _ := os.ReadFile("testdata/" + name)
	raw = bytes.Replace(raw, []byte(`"user_id":"REDACTED"`), []byte(`"user_id":"`+seededAccount+`"`), 1)
	raw = bytes.Replace(raw, []byte(`"account_id":"REDACTED"`), []byte(`"account_id":"`+seededAccount+`"`), 1)
	raw = bytes.Replace(raw, []byte(`"email":"REDACTED"`), []byte(`"email":"`+seededEmail+`"`), 1)
	w.Write(raw)
}

func (u *usageEndpoint) times(provider string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.asked[provider]
}

type rig struct {
	host  *fakeHost
	usage *usageEndpoint
	w     *watcher
	clock time.Time
	logs  *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	r := &rig{host: &fakeHost{lastRefresh: refreshed}, clock: observed, logs: &bytes.Buffer{},
		usage: &usageEndpoint{status: map[string]int{}, asked: map[string]int{}, headers: map[string]http.Header{}}}
	server := httptest.NewServer(r.usage)
	t.Cleanup(server.Close)
	prevHost, prevCodex, prevClaude := hostCall, codexUsageURL, claudeUsageURL
	hostCall, codexUsageURL, claudeUsageURL = r.host.call, server.URL+"/codex", server.URL+"/claude"
	log.SetOutput(r.logs)
	t.Cleanup(func() {
		hostCall, codexUsageURL, claudeUsageURL = prevHost, prevCodex, prevClaude
		log.SetOutput(os.Stderr)
	})
	r.w = r.watcher(t, filepath.Join(t.TempDir(), "gentility-quota.json"))
	return r
}

func (r *rig) watcher(t *testing.T, file string) *watcher {
	w := newWatcher(file)
	w.now = func() time.Time { return r.clock }
	return w
}

func (r *rig) sweepAt(after time.Duration) {
	r.clock = observed.Add(after)
	r.w.sweep(context.Background())
}

func (r *rig) poll(index string) poll {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	return r.w.logins[index].Poll
}

func TestAPollSendsTheBearerAndTheAccountAndKeepsOnlyCodexAndClaude(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	codex, claude := r.usage.headers["codex"], r.usage.headers["claude"]
	if codex.Get("Authorization") != "Bearer "+seededToken || codex.Get("ChatGPT-Account-Id") != seededAccount ||
		codex.Get("Originator") != "" || codex.Get("User-Agent") != pollUserAgent {
		t.Fatalf("codex poll headers %v", codex)
	}
	if claude.Get("Authorization") != "Bearer "+seededToken || claude.Get("anthropic-beta") != "oauth-2025-04-20" ||
		claude.Get("User-Agent") != pollUserAgent {
		t.Fatalf("claude poll headers %v", claude)
	}
	if len(r.w.logins) != 2 || r.w.logins["k1"] != nil {
		t.Fatalf("logins %v", r.w.logins)
	}
	next := r.poll("cx1").NextAt.Sub(observed)
	if r.poll("cx1").State != pollOK || next < 9*time.Minute || next > 11*time.Minute {
		t.Fatalf("poll %+v", r.poll("cx1"))
	}
}

func TestA429BacksOffAsRetryAfterAsks(t *testing.T) {
	r := newRig(t)
	r.usage.status["codex"], r.usage.retryAfter = http.StatusTooManyRequests, "1800"
	r.sweepAt(0)
	if p := r.poll("cx1"); p.State != backingOff || !p.NextAt.Equal(observed.Add(30*time.Minute)) {
		t.Fatalf("poll %+v", p)
	}
	r.sweepAt(29 * time.Minute)
	if n := r.usage.times("codex"); n != 1 {
		t.Fatalf("asked %d times inside Retry-After", n)
	}
	r.usage.retryAfter = "7200"
	r.sweepAt(30 * time.Minute)
	if p := r.poll("cx1"); r.usage.times("codex") != 2 || !p.NextAt.Equal(observed.Add(30*time.Minute+time.Hour)) {
		t.Fatalf("a Retry-After past an hour waited until %v", p.NextAt)
	}
}

func TestA429WithoutRetryAfterBacksOffExponentiallyToAnHour(t *testing.T) {
	r := newRig(t)
	r.usage.status["claude"] = http.StatusTooManyRequests
	at := time.Duration(0)
	for _, want := range []time.Duration{20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		r.sweepAt(at)
		p := r.poll("cl1")
		if p.State != backingOff || p.NextAt.Sub(observed.Add(at)) != want {
			t.Fatalf("after %s: poll %+v, want a wait of %s", at, p, want)
		}
		at = p.NextAt.Sub(observed)
	}
	r.usage.status["claude"] = http.StatusOK
	r.sweepAt(at)
	if p := r.poll("cl1"); p.State != pollOK || p.Failures != 0 {
		t.Fatalf("a good poll left %+v", p)
	}
}

func TestA401StopsPollingUntilTheCredentialChanges(t *testing.T) {
	r := newRig(t)
	r.usage.status["codex"] = http.StatusUnauthorized
	r.sweepAt(0)
	if p := r.poll("cx1"); p.State != stoppedUnauthorized || p.Credential != refreshed.Format(time.RFC3339) {
		t.Fatalf("poll %+v", p)
	}
	r.sweepAt(3 * time.Hour)
	if n := r.usage.times("codex"); n != 1 {
		t.Fatalf("a refused login was asked %d times", n)
	}
	r.w.dirty = true
	r.w.save()
	r.w = r.watcher(t, r.w.file)
	r.sweepAt(3*time.Hour + time.Minute)
	if n := r.usage.times("codex"); n != 1 {
		t.Fatalf("after a restart a refused login was asked again (%d times)", n)
	}
	r.usage.status["codex"] = http.StatusOK
	r.host.lastRefresh = refreshed.Add(4 * time.Hour)
	r.sweepAt(4 * time.Hour)
	if p := r.poll("cx1"); r.usage.times("codex") != 2 || p.State != pollOK {
		t.Fatalf("a refreshed login was not asked again: %+v", p)
	}
}

func TestNoPollComesSoonerThanAMinute(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	r.w.logins["cx1"].Poll.NextAt = observed
	r.sweepAt(30 * time.Second)
	if n := r.usage.times("codex"); n != 1 {
		t.Fatalf("asked %d times within a minute", n)
	}
}

func TestARestartAnswersFromTheFileBeforeThePollLands(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	before := r.w.fetch("cx1")

	hostCall = func(string, []byte) ([]byte, error) {
		t.Fatal("the restarted record called the host before its first sweep")
		return nil, nil
	}
	restarted := r.watcher(t, r.w.file)
	after := restarted.fetch("cx1")
	if len(after.Groups) != 1 || len(after.Groups[0].Buckets) != 1 || after.Groups[0].Buckets[0] != before.Groups[0].Buckets[0] {
		t.Fatalf("after a restart the record answered %+v, before it %+v", after, before)
	}
	if p := restarted.logins["cx1"].Poll; p.NextAt.Before(observed.Add(9 * time.Minute)) {
		t.Fatalf("the restart brought the next poll forward: %+v", p)
	}
}

func TestReplyHeadersUpdateTheLoginThatServedThem(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	watched = r.w
	t.Cleanup(func() { watched = nil })
	call := func(method string, body map[string]any) {
		raw, _ := json.Marshal(body)
		if _, err := handleMethod(method, raw); err != nil {
			t.Fatal(err)
		}
	}
	headers := http.Header{"X-Codex-Primary-Used-Percent": {"99"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-At": {"1794543707"}}
	r.clock = observed.Add(time.Minute)
	call(pluginabi.MethodResponseInterceptStreamChunk, map[string]any{"RequestID": "r0", "ChunkIndex": -1, "ResponseHeaders": headers})
	call(pluginabi.MethodRequestInterceptAfter, map[string]any{"RequestID": "r1", "Metadata": map[string]any{"selected_auth_index": "cx1"}})
	call(pluginabi.MethodResponseInterceptStreamChunk, map[string]any{"RequestID": "r1", "ChunkIndex": 3, "ResponseHeaders": headers})
	if w := r.w.logins["cx1"].Windows[0]; w.Source != "poll" {
		t.Fatalf("a reply with no login, or a payload chunk, updated the record: %+v", w)
	}
	call(pluginabi.MethodResponseInterceptStreamChunk, map[string]any{"RequestID": "r1", "ChunkIndex": -1, "ResponseHeaders": headers})
	if w := r.w.logins["cx1"].Windows[0]; w.Source != "response" || w.UsedFraction != 0.99 || !w.ObservedAt.Equal(r.clock) {
		t.Fatalf("the header call left %+v", w)
	}
	if len(r.w.serving) != 0 {
		t.Fatalf("a read reply was kept: %v", r.w.serving)
	}
	call(pluginabi.MethodRequestInterceptAfter, map[string]any{"RequestID": "r2", "Metadata": map[string]any{"selected_auth_index": "k1"}})
	call(pluginabi.MethodRequestInterceptAfter, map[string]any{"RequestID": "r3", "Metadata": map[string]any{"selected_auth_index": "cl1"}})
	r.sweepAt(12 * time.Minute)
	if len(r.w.serving) != 0 {
		t.Fatalf("an untracked login or an unanswered request was kept: %v", r.w.serving)
	}
}

func TestNoSecretReachesTheRecordTheFileOrTheLog(t *testing.T) {
	r := newRig(t)
	r.sweepAt(0)
	watched = r.w
	t.Cleanup(func() { watched = nil })
	selected, _ := json.Marshal(map[string]any{"RequestID": "r1", "Headers": http.Header{"Authorization": {"Bearer " + seededToken}},
		"Metadata": map[string]any{"selected_auth_index": "cl1", "email": seededEmail}})
	r.w.noteServing(selected)
	reply, _ := json.Marshal(map[string]any{
		"RequestID":       "r1",
		"RequestHeaders":  http.Header{"Authorization": {"Bearer " + seededToken}},
		"ResponseHeaders": http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.9"}, "Anthropic-Ratelimit-Unified-7d-Reset": {"1794643200"}},
	})
	r.w.observeReply(reply)
	if w := r.w.logins["cl1"].Windows; w[0].Source != "response" {
		t.Fatalf("the reply did not reach the record: %+v", w)
	}
	r.usage.status["codex"], r.usage.status["claude"] = http.StatusUnauthorized, http.StatusInternalServerError
	r.host.lastRefresh = refreshed.Add(time.Hour)
	r.sweepAt(time.Hour)
	r.host.refuseGet = true
	r.sweepAt(3 * time.Hour)
	if !strings.Contains(r.logs.String(), "did not hand over the login file") {
		t.Fatalf("the refused read was not logged:\n%s", r.logs)
	}
	r.w.dirty = true
	r.w.save()

	record, _ := json.Marshal(r.w.logins)
	fetched, _ := json.Marshal([]quotaFetch{r.w.fetch("cx1"), r.w.fetch("cl1")})
	file, err := os.ReadFile(r.w.file)
	if err != nil || r.logs.Len() == 0 {
		t.Fatalf("file %v, log %q: the test exercised neither", err, r.logs)
	}
	for name, got := range map[string][]byte{"record": record, "quota route": fetched, "file": file, "log": r.logs.Bytes()} {
		for _, secret := range []string{seededToken, seededAccount, seededEmail} {
			if bytes.Contains(got, []byte(secret)) {
				t.Fatalf("the %s holds %q:\n%s", name, secret, got)
			}
		}
	}
}
