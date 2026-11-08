package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// keyPrefixes is contract/key_prefixes.json: the prefix the Gentility server
// mints on every gateway key, and the harness it names. The proxy matches a
// key exactly, so a client cannot change its key's prefix without holding a
// valid key of that harness. A test holds this table to the file.
var keyPrefixes = map[string]string{"omp_": "omp", "claude_": "claude", "codex_": "codex"}

// The host's login list is trusted for a minute. A login file's auth_index
// comes from its path, not its provider, so a path can come to hold another
// provider's login; a minute bounds how long a stale answer is believed.
const loginsFresh = time.Minute

// scopeSeam runs at the start of every check. Tests make it panic.
var scopeSeam = func() {}

// logins knows the harness of every login the host holds, by auth_index. A
// login with no harness is kept as "" so it is refused without a refresh.
type logins struct {
	mu       sync.Mutex
	harness  map[string]string
	listedAt time.Time
	now      func() time.Time
}

var scoped = &logins{now: time.Now}

// hostCredential is the part of a host.auth.models entry the check reads.
// host.auth.list is not used: it leaves out logins from config, such as the
// openai-compatibility ones omp is served by.
type hostCredential struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
}

// loginHarness names the harness a login serves.
//   - claude and codex are the provider names of both an OAuth login and a
//     claude-api-key or codex-api-key entry.
//   - An openai-compatibility entry from config has the provider key
//     "openai-compatible-<name>", or "openai-compatibility" when unnamed
//     (internal/util/provider.go:15-27), and an id of the kind
//     "openai-compatibility:<name>" (internal/watcher/synthesizer/config.go:388
//     and :440). Both must hold, so a login file that calls its type
//     openai-compatible-something is not taken for one.
//   - Anything else (gemini, vertex, antigravity, xai, ...) serves no harness.
func loginHarness(c hostCredential) string {
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	switch {
	case provider == "claude":
		return "claude"
	case provider == "codex":
		return "codex"
	case (provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-")) &&
		strings.HasPrefix(c.ID, "openai-compatibility:"):
		return "omp"
	}
	return ""
}

// refresh reads the host's logins again. The ids are read for their kind and
// dropped: a login file's id is its name, which can carry its email.
func (l *logins) refresh() error {
	var listed struct{ Credentials []hostCredential }
	if err := callHost(pluginabi.MethodHostAuthModels, map[string]string{"auth_index": ""}, &listed); err != nil {
		return errors.New("the host did not list its logins")
	}
	l.harness = map[string]string{}
	for _, c := range listed.Credentials {
		if c.AuthIndex != "" {
			l.harness[c.AuthIndex] = loginHarness(c)
		}
	}
	l.listedAt = l.now()
	return nil
}

// harnessOf answers from the list while it is fresh, and otherwise, or when
// the login is not in it, reads the list once more.
func (l *logins) harnessOf(index string) (harness string, known bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.harness != nil && l.now().Sub(l.listedAt) < loginsFresh {
		if h, found := l.harness[index]; found {
			return h, true, nil
		}
	}
	if err := l.refresh(); err != nil {
		return "", false, err
	}
	harness, known = l.harness[index]
	return harness, known, nil
}

// callerScope is upstream's handle for the key that authenticated a request
// (sdk/cliproxy/session/identity.go:210), which the host hands every
// interceptor. A test holds it to upstream's.
func callerScope(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + key))
	return hex.EncodeToString(sum[:])
}

// bearer reads an Authorization header the way the proxy's key check does
// (internal/access/config_access/provider.go:106-118): a value that is not
// "Bearer <key>" is taken whole.
func bearer(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

// clientKey finds the key that authenticated a request among its headers.
// The proxy accepts the first configured key among Authorization,
// X-Goog-Api-Key, X-Api-Key and two query parameters, so a request can carry
// a valid key in one place and any string in another. The host's caller
// scope names the one that matched; a request whose scope matches no header
// (its key came in the query) has no key here. Without a scope, every header
// that carries a key must agree on its harness.
func clientKey(headers http.Header, scope string) (key string, found bool) {
	candidates := []string{bearer(headers.Get("Authorization")), headers.Get("X-Goog-Api-Key"), headers.Get("X-Api-Key")}
	if scope != "" {
		for _, c := range candidates {
			if c != "" && callerScope(c) == scope {
				return c, true
			}
		}
		return "", false
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if found && keyHarness(c) != keyHarness(key) {
			return "", false
		}
		key, found = c, true
	}
	return key, found
}

// keyHarness names the harness a key's prefix marks, or "" for none. No
// prefix in the table is a prefix of another.
func keyHarness(key string) string {
	for prefix, harness := range keyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return harness
		}
	}
	return ""
}

// verdict is one request's check: the two harnesses, and why it was refused
// when it was.
type verdict struct {
	requestID, model string
	key, login       string
	refused          string
}

// judge decides whether a request's key may reach the login the host chose
// for it. Every path that cannot show the two harnesses match refuses.
func judge(raw []byte) verdict {
	var req struct {
		RequestID      string
		Model          string
		RequestedModel string
		Headers        http.Header
		CallerScope    string
		Metadata       map[string]any
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return verdict{refused: "unreadable login metadata"}
	}
	v := verdict{requestID: req.RequestID, model: req.RequestedModel}
	if v.model == "" {
		v.model = req.Model
	}
	scope := req.CallerScope
	if scope == "" {
		scope, _ = req.Metadata[pluginapi.CallerScopeMetadataKey].(string)
	}
	key, found := clientKey(req.Headers, strings.TrimSpace(scope))
	switch {
	case !found:
		v.refused = "no key"
	case keyHarness(key) == "":
		v.refused = "a key with no known prefix"
	}
	v.key = keyHarness(key)
	index, _ := req.Metadata["selected_auth_index"].(string)
	if index == "" {
		if v.refused == "" {
			v.refused = "unreadable login metadata"
		}
		return v
	}
	harness, known, err := scoped.harnessOf(index)
	v.login = harness
	switch {
	case v.refused != "":
	case err != nil:
		v.refused = err.Error()
	case !known:
		v.refused = "a login the host does not list"
	case harness == "":
		v.refused = "a login of no harness"
	case harness != v.key:
		v.refused = "another harness's login"
	}
	return v
}

// afterAuth answers a request's after-auth call, once the host has chosen
// its login: the quota watch notes the login, and the key scope check
// answers {} to let the request on or a refusal the host sends to the client
// as it is. It never answers with an error, and recovers its own panics,
// because the host lets a request through on either.
func afterAuth(raw []byte) (answer json.RawMessage) {
	defer func() {
		if p := recover(); p != nil {
			// The panic's value is not logged: it could carry what the
			// request did.
			log.Printf("gentility-usage: the after-auth call panicked (%T); refusing", p)
			answer = refusal(verdict{refused: "a panic"})
		}
	}()
	scopeSeam()
	if watched != nil {
		watched.noteServing(raw)
	}
	v := judge(raw)
	if v.refused == "" {
		return json.RawMessage(`{}`)
	}
	return refusal(v)
}

// refusal is a 403 that names the two harnesses and the model, never the key
// or the login's account.
func refusal(v verdict) json.RawMessage {
	log.Printf("gentility-usage: refused request %q: key harness %s, login harness %s, model %q: %s",
		v.requestID, orNone(v.key, "none"), orNone(v.login, "none"), v.model, v.refused)
	model := v.model
	if model == "" {
		model = "this model"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "key_scope",
		"message": fmt.Sprintf("this gateway key is for %s; %s is served by a %s login",
			orNone(v.key, "no harness"), model, orNone(v.login, "unscoped")),
	}})
	raw, _ := json.Marshal(pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusForbidden,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: body})
	return raw
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}
