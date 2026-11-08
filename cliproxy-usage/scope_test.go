package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const seededKey = "omp_SEEDED-client-key-0001"

// loginsHost answers host.auth.models with every kind of login the gateway
// can hold. The ids are as upstream makes them, a login file's carrying the
// seeded email.
type loginsHost struct {
	mu    sync.Mutex
	fail  bool
	asked int
	creds []hostCredential
}

func newLoginsHost() *loginsHost {
	return &loginsHost{creds: []hostCredential{
		{ID: "claude-" + seededEmail + ".json", AuthIndex: "claude-oauth", Provider: "claude"},
		{ID: "claude:apikey:0123456789ab", AuthIndex: "claude-key", Provider: "claude"},
		{ID: "codex-" + seededEmail + "-pro.json", AuthIndex: "codex-oauth", Provider: "codex"},
		{ID: "codex:apikey:0123456789ab", AuthIndex: "codex-key", Provider: "codex"},
		{ID: "openai-compatibility:zai:0123456789ab", AuthIndex: "zai", Provider: "openai-compatible-zai"},
		{ID: "openai-compatibility:openai-compatibility:0123456789ab", AuthIndex: "unnamed", Provider: "openai-compatibility"},
		{ID: "gemini:apikey:0123456789ab", AuthIndex: "gemini", Provider: "gemini"},
		{ID: "antigravity-" + seededEmail + ".json", AuthIndex: "antigravity", Provider: "antigravity"},
		{ID: "xai:apikey:0123456789ab", AuthIndex: "xai", Provider: "xai"},
		// A login file that names itself a compat provider is not one.
		{ID: "openai-compatible-zai.json", AuthIndex: "fake-compat", Provider: "openai-compatible-zai"},
	}}
}

func (h *loginsHost) call(method string, request []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if method != pluginabi.MethodHostAuthModels {
		return nil, nil
	}
	h.asked++
	if h.fail {
		return json.Marshal(pluginabi.Envelope{Error: &pluginabi.Error{Code: "host_call_failed", Message: "auth manager unavailable"}})
	}
	raw, _ := json.Marshal(map[string]any{"credentials": h.creds})
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func (h *loginsHost) times() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked
}

type scopeRig struct {
	host  *loginsHost
	logs  *bytes.Buffer
	clock time.Time
}

func newScopeRig(t *testing.T) *scopeRig {
	r := &scopeRig{host: newLoginsHost(), logs: &bytes.Buffer{}, clock: observed}
	prevHost, prevScoped, prevWatched := hostCall, scoped, watched
	hostCall, scoped, watched = r.host.call, &logins{now: func() time.Time { return r.clock }}, nil
	log.SetOutput(r.logs)
	t.Cleanup(func() {
		hostCall, scoped, watched = prevHost, prevScoped, prevWatched
		scopeSeam = func() {}
		log.SetOutput(os.Stderr)
	})
	return r
}

// asked is an after-auth call as the host makes it.
type asked struct {
	headers http.Header
	scope   string
	index   string
}

func keyed(key, index string) asked {
	return asked{headers: http.Header{"Authorization": {"Bearer " + key}}, scope: callerScope(key), index: index}
}

func (a asked) raw() []byte {
	meta := map[string]any{}
	if a.index != "" {
		meta["selected_auth_index"] = a.index
	}
	raw, _ := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "req-1", Model: "upstream-model",
		RequestedModel: "asked-model", Headers: a.headers, CallerScope: a.scope, Metadata: meta})
	return raw
}

// send makes the after-auth call through the plugin's method switch, and
// fails the test on an error envelope, which would let the request through.
func send(t *testing.T, raw []byte) pluginapi.RequestInterceptResponse {
	t.Helper()
	answer, err := handleMethod(pluginabi.MethodRequestInterceptAfter, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(answer, &envelope); err != nil || !envelope.OK || envelope.Error != nil {
		t.Fatalf("not an ok envelope: %s", answer)
	}
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("result %s: %v", envelope.Result, err)
	}
	return resp
}

// refused checks a refusal's shape and that its log line says why.
func (r *scopeRig) refused(t *testing.T, resp pluginapi.RequestInterceptResponse, why string) {
	t.Helper()
	var body struct {
		Error struct{ Type, Message string }
	}
	if !resp.Terminate || resp.StatusCode != http.StatusForbidden || resp.ResponseHeaders.Get("Content-Type") != "application/json" ||
		json.Unmarshal(resp.ResponseBody, &body) != nil || body.Error.Type != "key_scope" {
		t.Fatalf("not a key_scope refusal: %+v %s", resp, resp.ResponseBody)
	}
	if !strings.Contains(r.logs.String(), "gentility-usage: refused request") || !strings.Contains(r.logs.String(), why) {
		t.Fatalf("log %q does not say %q", r.logs.String(), why)
	}
	for _, secret := range []string{seededKey, seededEmail, "SEEDED"} {
		if bytes.Contains(resp.ResponseBody, []byte(secret)) || strings.Contains(r.logs.String(), secret) {
			t.Fatalf("%q leaked: body %s, log %q", secret, resp.ResponseBody, r.logs.String())
		}
	}
}

func allowed(t *testing.T, resp pluginapi.RequestInterceptResponse) {
	t.Helper()
	if resp.Terminate || resp.StatusCode != 0 || len(resp.ResponseBody) > 0 {
		t.Fatalf("refused: %+v %s", resp, resp.ResponseBody)
	}
}

func TestKeyPrefixesAreTheContractsTable(t *testing.T) {
	raw, err := os.ReadFile("../contract/key_prefixes.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Format   int
		Prefixes map[string]string
	}
	if err := json.Unmarshal(raw, &file); err != nil || file.Format != 1 {
		t.Fatalf("key_prefixes.json: format %d, %v", file.Format, err)
	}
	if !reflect.DeepEqual(file.Prefixes, keyPrefixes) {
		t.Fatalf("the plugin's table %v is not contract/key_prefixes.json's %v", keyPrefixes, file.Prefixes)
	}
	for a := range keyPrefixes {
		for b := range keyPrefixes {
			if a != b && strings.HasPrefix(a, b) {
				t.Fatalf("%s starts with %s, so a key's harness is ambiguous", a, b)
			}
		}
	}
}

// The vector is upstream's session.CallerScope of the seeded key at v8.0.61.
// The contract test proves it against each release's binary.
func TestCallerScopeIsUpstreams(t *testing.T) {
	const upstream = "e6e8f06a8778ae4dc17042ee8a71291dd00dfb1454f4397ad3e1e38ee583e169"
	if callerScope(seededKey) != upstream || callerScope(" "+seededKey+" ") != upstream || callerScope("") != "" {
		t.Fatalf("the caller scope differs from upstream's")
	}
}

// Every key harness against every login that serves one: allowed on the
// diagonal, refused off it.
func TestAKeyReachesOnlyItsOwnHarnesssLogins(t *testing.T) {
	logins := map[string]string{"claude-oauth": "claude", "claude-key": "claude", "codex-oauth": "codex",
		"codex-key": "codex", "zai": "omp", "unnamed": "omp"}
	for _, key := range []string{"omp_k", "claude_k", "codex_k"} {
		for index, harness := range logins {
			r := newScopeRig(t)
			resp := send(t, keyed(key, index).raw())
			if keyHarness(key) == harness {
				allowed(t, resp)
			} else {
				r.refused(t, resp, "another harness's login")
				want := "this gateway key is for " + keyHarness(key) + "; asked-model is served by a " + harness + " login"
				if !bytes.Contains(resp.ResponseBody, []byte(want)) {
					t.Fatalf("%s at %s: body %s", key, index, resp.ResponseBody)
				}
			}
		}
	}
}

func TestARequestWithNoKeyIsRefused(t *testing.T) {
	r := newScopeRig(t)
	r.refused(t, send(t, asked{headers: http.Header{}, index: "zai"}.raw()), "no key")
}

func TestAKeyWithAnUnknownPrefixIsRefused(t *testing.T) {
	r := newScopeRig(t)
	r.refused(t, send(t, keyed("gemini_k", "zai").raw()), "a key with no known prefix")
}

// The sentinel and the keys minted before prefixes carry none.
func TestAnUnprefixedKeyIsRefused(t *testing.T) {
	r := newScopeRig(t)
	resp := send(t, keyed("contract-sentinel", "zai").raw())
	r.refused(t, resp, "a key with no known prefix")
	if !bytes.Contains(resp.ResponseBody, []byte("this gateway key is for no harness; asked-model is served by a omp login")) {
		t.Fatalf("body %s", resp.ResponseBody)
	}
}

func TestUnreadableLoginMetadataIsRefused(t *testing.T) {
	r := newScopeRig(t)
	r.refused(t, send(t, []byte(`{"RequestID":`)), "unreadable login metadata")
	r.logs.Reset()
	r.refused(t, send(t, keyed("omp_k", "").raw()), "unreadable login metadata")
}

func TestALoginListTheHostCannotGiveIsRefused(t *testing.T) {
	r := newScopeRig(t)
	r.host.fail = true
	resp := send(t, keyed("omp_k", "zai").raw())
	r.refused(t, resp, "the host did not list its logins")
	if !bytes.Contains(resp.ResponseBody, []byte("served by a unscoped login")) {
		t.Fatalf("body %s", resp.ResponseBody)
	}
}

func TestALoginOfNoHarnessIsRefusedForEveryKey(t *testing.T) {
	for _, index := range []string{"gemini", "antigravity", "xai", "fake-compat"} {
		for _, key := range []string{"omp_k", "claude_k", "codex_k"} {
			r := newScopeRig(t)
			r.refused(t, send(t, keyed(key, index).raw()), "a login of no harness")
		}
	}
}

func TestALoginTheHostDoesNotListIsRefusedAfterOneRefresh(t *testing.T) {
	r := newScopeRig(t)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "a login the host does not list")
	if n := r.host.times(); n != 2 {
		t.Fatalf("the host was asked %d times, want once to fill the list and once on the miss", n)
	}
}

func TestAPanicInTheHookIsRefused(t *testing.T) {
	r := newScopeRig(t)
	scopeSeam = func() { panic("injected: " + seededKey) }
	r.refused(t, send(t, keyed(seededKey, "zai").raw()), "a panic")
}

func TestASeededKeyAppearsInNoRefusal(t *testing.T) {
	r := newScopeRig(t)
	r.refused(t, send(t, keyed(seededKey, "claude-oauth").raw()), "another harness's login")
}

// The proxy takes the first configured key among the request's headers, so a
// request can carry a valid omp key in X-Api-Key and a made-up Claude one in
// Authorization. The caller scope says which one authenticated.
func TestTheKeyThatAuthenticatedDecidesNotTheFirstHeader(t *testing.T) {
	mixed := asked{headers: http.Header{"Authorization": {"Bearer claude_madeup"}, "X-Api-Key": {"omp_k"}},
		scope: callerScope("omp_k")}
	r := newScopeRig(t)
	mixed.index = "claude-oauth"
	r.refused(t, send(t, mixed.raw()), "another harness's login")
	mixed.index = "zai"
	allowed(t, send(t, mixed.raw()))

	// A key from the query matches no header.
	r.logs.Reset()
	query := keyed("claude_madeup", "claude-oauth")
	query.scope = callerScope("omp_k")
	r.refused(t, send(t, query.raw()), "no key")

	// Without a scope, every header that carries a key must agree.
	r.logs.Reset()
	unscoped := asked{headers: mixed.headers, index: "claude-oauth"}
	r.refused(t, send(t, unscoped.raw()), "no key")
	agreeing := asked{headers: http.Header{"Authorization": {"Bearer omp_a"}, "X-Goog-Api-Key": {"omp_b"}}, index: "zai"}
	allowed(t, send(t, agreeing.raw()))
}

func TestTheOtherKeyHeadersCarryAKey(t *testing.T) {
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		newScopeRig(t)
		allowed(t, send(t, asked{headers: http.Header{header: {"claude_k"}}, scope: callerScope("claude_k"), index: "claude-key"}.raw()))
	}
	// An Authorization header that is not "Bearer <key>" is the key whole, as
	// the proxy reads it.
	newScopeRig(t)
	allowed(t, send(t, asked{headers: http.Header{"Authorization": {"codex_k"}}, scope: callerScope("codex_k"), index: "codex-key"}.raw()))
}

func TestTheLoginListIsTrustedForAMinute(t *testing.T) {
	r := newScopeRig(t)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	r.clock = observed.Add(59 * time.Second)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	if n := r.host.times(); n != 1 {
		t.Fatalf("asked the host %d times inside a minute", n)
	}
	// The login's path now holds a Claude login.
	r.host.mu.Lock()
	r.host.creds = []hostCredential{{ID: "zai.json", AuthIndex: "zai", Provider: "claude"}}
	r.host.mu.Unlock()
	r.clock = observed.Add(time.Minute)
	r.refused(t, send(t, keyed("omp_k", "zai").raw()), "another harness's login")
}

// The quota watch still learns which login serves a request, allowed or not.
func TestTheAfterAuthCallStillNotesTheServingLogin(t *testing.T) {
	r := newScopeRig(t)
	watched = &watcher{logins: map[string]*login{"codex-oauth": {AuthIndex: "codex-oauth", Provider: "codex"}},
		serving: map[string]serving{}, now: func() time.Time { return r.clock }}
	allowed(t, send(t, keyed("codex_k", "codex-oauth").raw()))
	if s, found := watched.serving["req-1"]; !found || s.authIndex != "codex-oauth" {
		t.Fatalf("serving %v", watched.serving)
	}
}
