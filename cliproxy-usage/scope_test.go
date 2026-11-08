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
	mu     sync.Mutex
	fail   bool
	panics bool
	asked  int
	creds  []hostCredential
	// When held is set, every list call says on calling that it has begun,
	// then waits for held to close.
	held, calling chan struct{}
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
	if method == pluginabi.MethodHostAuthModels && h.held != nil {
		h.calling <- struct{}{}
		<-h.held
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if method != pluginabi.MethodHostAuthModels {
		return nil, nil
	}
	h.asked++
	if h.panics {
		panic("injected host panic")
	}
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

// hold makes the host's list calls wait until the returned func is called.
func (r *scopeRig) hold() (release func()) {
	r.host.held, r.host.calling = make(chan struct{}), make(chan struct{}, 64)
	return func() { close(r.host.held) }
}

// begun waits for a held list call to begin.
func (r *scopeRig) begun(t *testing.T) {
	t.Helper()
	select {
	case <-r.host.calling:
	case <-time.After(5 * time.Second):
		t.Fatal("no list call began")
	}
}

// allJoined waits for checks to join the read under way.
func allJoined(t *testing.T, joined *sync.WaitGroup) {
	t.Helper()
	all := make(chan struct{})
	go func() { joined.Wait(); close(all) }()
	select {
	case <-all:
	case <-time.After(5 * time.Second):
		t.Fatal("the checks did not join the read under way")
	}
}

func newScopeRig(t *testing.T) *scopeRig {
	r := &scopeRig{host: newLoginsHost(), logs: &bytes.Buffer{}, clock: observed}
	prevHost, prevScoped, prevWatched := hostCall, scoped, watched
	hostCall, scoped, watched = r.host.call, &logins{now: func() time.Time { return r.clock }}, nil
	log.SetOutput(r.logs)
	t.Cleanup(func() {
		hostCall, scoped, watched = prevHost, prevScoped, prevWatched
		scopeSeam, joinedListing = func() {}, func() {}
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
	return answered(t, answer, err)
}

// sendLater makes the after-auth call on its own goroutine.
func sendLater(raw []byte) <-chan []byte {
	answer := make(chan []byte, 1)
	go func() {
		got, err := handleMethod(pluginabi.MethodRequestInterceptAfter, raw)
		if err != nil {
			got = []byte(err.Error())
		}
		answer <- got
	}()
	return answer
}

// await reads a sendLater answer, failing the test if it does not come.
func await(t *testing.T, answer <-chan []byte) pluginapi.RequestInterceptResponse {
	t.Helper()
	select {
	case got := <-answer:
		return answered(t, got, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("the after-auth call did not answer within 5 s")
		return pluginapi.RequestInterceptResponse{}
	}
}

func answered(t *testing.T, answer []byte, err error) pluginapi.RequestInterceptResponse {
	t.Helper()
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

// The vector is upstream's session.CallerScope of the seeded key. The function
// is unchanged from v8.0.0 to v8.0.61, so it holds for go.mod's v8.0.60 and the
// contract test's v8.0.61 alike; the contract test proves it against each
// release's binary.
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
				want := "this gateway key is for " + keyHarness(key) + "; asked-model is served by a login for " + harness
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
	if !bytes.Contains(resp.ResponseBody, []byte("this gateway key is for no harness; asked-model is served by a login for omp")) {
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
	if !bytes.Contains(resp.ResponseBody, []byte("served by a login for no harness")) {
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

	// Without a scope there is no telling which key the proxy accepted: an
	// omp key in the query and a made-up Claude one in Authorization would
	// read as Claude. So no scope is no key, even when the headers agree.
	r.logs.Reset()
	unscoped := asked{headers: http.Header{"Authorization": {"Bearer claude_madeup"}}, index: "claude-oauth"}
	r.refused(t, send(t, unscoped.raw()), "no key")
	r.logs.Reset()
	agreeing := asked{headers: http.Header{"Authorization": {"Bearer omp_a"}, "X-Goog-Api-Key": {"omp_a"}}, index: "zai"}
	r.refused(t, send(t, agreeing.raw()), "no key")
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

// Checks that need the list while it is being read wait on that read: the
// host is called once for all of them. Were the host called under the lock,
// the later checks would wait on the lock and never join.
func TestChecksDuringARefreshShareOneHostCall(t *testing.T) {
	r := newScopeRig(t)
	release := r.hold()
	var joined sync.WaitGroup
	joined.Add(5)
	joinedListing = joined.Done
	first := sendLater(keyed("omp_k", "zai").raw())
	r.begun(t)
	later := []<-chan []byte{}
	for _, a := range []asked{keyed("omp_k", "zai"), keyed("claude_k", "claude-oauth"), keyed("codex_k", "codex-key"),
		keyed("omp_k", "gone"), keyed("omp_k", "claude-oauth")} {
		later = append(later, sendLater(a.raw()))
	}
	allJoined(t, &joined)
	release()
	for _, answer := range append([]<-chan []byte{first}, later[:3]...) {
		allowed(t, await(t, answer))
	}
	gone, crossed := await(t, later[3]), await(t, later[4])
	r.refused(t, gone, "a login the host does not list")
	r.refused(t, crossed, "another harness's login")
	if n := r.host.times(); n != 1 {
		t.Fatalf("the host was asked %d times, want once for all six checks", n)
	}
}

// A check the list answers does not wait behind a read another check needs.
func TestAListedLoginIsAnsweredDuringARefresh(t *testing.T) {
	r := newScopeRig(t)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	release := r.hold()
	gone := sendLater(keyed("omp_k", "gone").raw())
	r.begun(t)
	allowed(t, await(t, sendLater(keyed("omp_k", "zai").raw())))
	release()
	r.refused(t, await(t, gone), "a login the host does not list")
}

// A login the host does not list is refused for five seconds without asking
// the host again; after that the list is read once more, and a login that has
// appeared meanwhile is served.
func TestAMissedLoginIsNotAskedForAgainForFiveSeconds(t *testing.T) {
	r := newScopeRig(t)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "a login the host does not list")
	r.clock = observed.Add(missFresh - time.Millisecond)
	r.logs.Reset()
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "a login the host does not list")
	if n := r.host.times(); n != 2 {
		t.Fatalf("the host was asked %d times, want 2: the miss inside its window asks again", n)
	}
	r.clock = observed.Add(missFresh)
	r.logs.Reset()
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "a login the host does not list")
	if n := r.host.times(); n != 3 {
		t.Fatalf("the host was asked %d times, want 3: once more after the window", n)
	}
	r.host.mu.Lock()
	r.host.creds = append(r.host.creds, hostCredential{ID: "openai-compatibility:gone:0123456789ab", AuthIndex: "gone", Provider: "openai-compatible-gone"})
	r.host.mu.Unlock()
	r.clock = observed.Add(2*missFresh - time.Millisecond)
	r.logs.Reset()
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "a login the host does not list")
	r.clock = observed.Add(2 * missFresh)
	allowed(t, send(t, keyed("omp_k", "gone").raw()))
	if n := r.host.times(); n != 4 {
		t.Fatalf("the host was asked %d times, want 4", n)
	}
}

// A read that fails refuses, whether the list was stale, the login unknown
// or the check waited on another's read, and is not taken for a miss.
func TestARefreshThatFailsStillRefuses(t *testing.T) {
	r := newScopeRig(t)
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	r.host.mu.Lock()
	r.host.fail = true
	r.host.mu.Unlock()
	r.clock = observed.Add(loginsFresh)
	r.refused(t, send(t, keyed("omp_k", "zai").raw()), "the host did not list its logins")
	r.logs.Reset()
	r.refused(t, send(t, keyed("omp_k", "gone").raw()), "the host did not list its logins")

	r.logs.Reset()
	release := r.hold()
	var joined sync.WaitGroup
	joined.Add(1)
	joinedListing = joined.Done
	first := sendLater(keyed("omp_k", "zai").raw())
	r.begun(t)
	second := sendLater(keyed("omp_k", "zai").raw())
	allJoined(t, &joined)
	release()
	firstAnswer, secondAnswer := await(t, first), await(t, second)
	r.refused(t, firstAnswer, "the host did not list its logins")
	r.refused(t, secondAnswer, "the host did not list its logins")

	// The failures left no miss behind: the host answers again, and is asked.
	r.host.mu.Lock()
	r.host.fail = false
	r.host.mu.Unlock()
	r.host.held = nil
	asked := r.host.times()
	allowed(t, send(t, keyed("omp_k", "zai").raw()))
	if n := r.host.times(); n != asked+1 {
		t.Fatalf("the host was asked %d times, want %d", n, asked+1)
	}
}

// A host call that panics refuses the check that made it and leaves no read
// for the next check to wait on.
func TestAHostCallThatPanicsLeavesNoCheckWaiting(t *testing.T) {
	r := newScopeRig(t)
	r.host.panics = true
	r.refused(t, send(t, keyed("omp_k", "zai").raw()), "a panic")
	r.host.mu.Lock()
	r.host.panics = false
	r.host.mu.Unlock()
	allowed(t, await(t, sendLater(keyed("omp_k", "zai").raw())))
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
