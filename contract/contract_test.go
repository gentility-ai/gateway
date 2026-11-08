package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const management = "contract-management-key"
const sentinel = "contract-sentinel"

// A key's prefix names its harness (key_prefixes.json). The omp key is added
// and deleted through the server's calls; the other two are in the config.
const (
	ompKey    = "omp_k1"
	claudeKey = "claude_contract"
	codexKey  = "codex_contract"
)

const (
	compatModel = "glm-5.3-flash"
	claudeModel = "claude-contract"
	codexModel  = "codex-contract"
)

type usage struct {
	mu      sync.Mutex
	records []map[string]any
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func call(t *testing.T, method, url, bearer string, header map[string]string, body any) *http.Response {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// wire is one management call the server makes, as contract/calls.json
// names it. The test sends each one exactly as the file says, so a server
// change to those calls must change the file, and a proxy release that
// answers them differently goes red here.
type wire struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

func loadCalls(t *testing.T) map[string]wire {
	raw, err := os.ReadFile("calls.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Format int    `json:"format"`
		Calls  []wire `json:"calls"`
	}
	if err := json.Unmarshal(raw, &file); err != nil || file.Format != 1 {
		t.Fatalf("calls.json: format %d, %v", file.Format, err)
	}
	calls := map[string]wire{}
	for _, c := range file.Calls {
		calls[c.Name] = c
	}
	return calls
}

// send fills the placeholders from vars and sends the call.
func (c wire) send(t *testing.T, base string, vars map[string]string) *http.Response {
	fill := func(s string) string {
		for k, v := range vars {
			s = strings.ReplaceAll(s, "<"+k+">", v)
		}
		return s
	}
	url := base + fill(c.Path)
	if len(c.Query) > 0 {
		q := neturl.Values{}
		for k, v := range c.Query {
			q.Set(k, fill(v))
		}
		url += "?" + q.Encode()
	}
	var body io.Reader
	if len(c.Body) > 0 && string(c.Body) != "null" {
		body = strings.NewReader(fill(string(c.Body)))
	}
	req, _ := http.NewRequest(c.Method, url, body)
	for k, v := range c.Headers {
		req.Header.Set(k, fill(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", c.Name, err)
	}
	return resp
}

func TestTheCallsTheServerMakes(t *testing.T) {
	bin, plugin := os.Getenv("CLIPROXY_BIN"), os.Getenv("USAGE_PLUGIN")
	if bin == "" || plugin == "" {
		t.Skip("CLIPROXY_BIN and USAGE_PLUGIN name the pinned binary and the built plugin")
	}

	// One fake upstream per login type, each counting the requests that reach
	// it: the key scope check must refuse before any upstream call.
	compat := newFake(func(w http.ResponseWriter, r *http.Request, stream bool) {
		// The compat executor sends /v1/responses/compact on to the
		// upstream's own compact route; everything else is a chat completion.
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			_ = json.NewEncoder(w).Encode(compaction("fake-up"))
			return
		}
		if !stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": "fake-up",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": "hi"}}},
				"usage":   map[string]int{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}})
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"fake-up","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"fake-up","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer compat.Close()
	// The Claude executor posts to <base-url>/v1/messages, streaming whenever
	// the client's format is not Claude's.
	claude := newFake(func(w http.ResponseWriter, r *http.Request, stream bool) {
		if !stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-up",
				"content": []any{map[string]string{"type": "text", "text": "hi"}}, "stop_reason": "end_turn",
				"usage": map[string]int{"input_tokens": 7, "output_tokens": 3}})
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-up","content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		} {
			var typed struct{ Type string }
			_ = json.Unmarshal([]byte(event), &typed)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
		}
	})
	defer claude.Close()
	// The Codex executor always streams from <base-url>/responses, and posts a
	// compaction to <base-url>/responses/compact, which answers whole.
	codex := newFake(func(w http.ResponseWriter, r *http.Request, stream bool) {
		if r.URL.Path == "/responses/compact" {
			_ = json.NewEncoder(w).Encode(compaction("gpt-up"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		item := `{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}`
		fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress","model":"gpt-up","output":[]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","output_index":0,"item":`+item+`}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"r1","object":"response","created_at":1,"status":"completed","model":"gpt-up","output":[`+item+`],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`+"\n\n")
	})
	defer codex.Close()

	got := &usage{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Records []map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		ids := []string{}
		got.mu.Lock()
		for _, rec := range body.Records {
			got.records = append(got.records, rec)
			ids = append(ids, rec["request_id"].(string))
		}
		got.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": ids})
	}))
	defer receiver.Close()

	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")
	_ = os.MkdirAll(pluginDir, 0o755)
	raw, _ := os.ReadFile(plugin)
	// The host loads only its own platform's extension.
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	_ = os.WriteFile(filepath.Join(pluginDir, "gentility-usage"+ext), raw, 0o755)
	port := freePort(t)
	config := fmt.Sprintf(`server:
  host: "127.0.0.1"
  port: %d
auth-dir: %q
management:
  allow-remote: false
  secret-key: %q
access:
  api-keys: [%q, %q, %q]
openai-compatibility:
  - name: fake
    base-url: %q
    keys:
      - api-key: "upstream-key"
    models:
      - name: "fake-up"
        alias: "glm-5.3-flash"
claude-api-key:
  - api-key: "upstream-claude-key"
    base-url: %q
    models:
      - name: "claude-up"
        alias: %q
codex-api-key:
  - api-key: "upstream-codex-key"
    base-url: %q
    models:
      - name: "gpt-up"
        alias: %q
plugins:
  enabled: true
  dir: %q
  configs:
    gentility-usage:
      enabled: true
`, port, filepath.Join(dir, "auths"), management, sentinel, claudeKey, codexKey, compat.URL+"/v1",
		claude.URL, claudeModel, codex.URL, codexModel, pluginDir)
	configPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(configPath, []byte(config), 0o600)

	proxy := exec.Command(bin, "--config", configPath)
	proxy.Env = append(os.Environ(), "GENTILITY_USAGE_URL="+receiver.URL+"/desk/gateway/usage", "GENTILITY_USAGE_SPOOL="+filepath.Join(dir, "spool"))
	var log watched
	proxy.Stdout, proxy.Stderr = io.MultiWriter(os.Stdout, &log), io.MultiWriter(os.Stderr, &log)
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer proxy.Process.Kill()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	mgmt := map[string]string{"X-Management-Key": management}
	calls, used := loadCalls(t), map[string]bool{}
	server := func(name string, vars map[string]string) *http.Response {
		c, ok := calls[name]
		if !ok {
			t.Fatalf("calls.json has no %s", name)
		}
		used[name] = true
		vars["management_key"], vars["sentinel"] = management, sentinel
		return c.send(t, base, vars)
	}
	deadline := time.Now().Add(20 * time.Second)
	up := func() bool {
		req, _ := http.NewRequest("GET", base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	}
	for !up() {
		if time.Now().After(deadline) {
			t.Fatal("the proxy never answered /v1/models with the sentinel")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Key changes reach the live proxy only through its config file watcher,
	// which starts after the proxy first answers.
	for !log.has("file watcher started") {
		if time.Now().After(deadline) {
			t.Fatal("the proxy never started its config file watcher")
		}
		time.Sleep(50 * time.Millisecond)
	}

	accepted := func(key string) bool { return server("probe_key", map[string]string{"key": key}).StatusCode == 200 }
	waitFor := func(want bool, key string) bool {
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			if accepted(key) == want {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	t.Run("add", func(t *testing.T) {
		resp := server("add_key", map[string]string{"key": ompKey})
		if resp.StatusCode/100 != 2 {
			t.Fatalf("PATCH answered %d", resp.StatusCode)
		}
	})
	t.Run("probe", func(t *testing.T) {
		if !waitFor(true, ompKey) {
			t.Fatal("an added key was not accepted within 2 s")
		}
	})
	t.Run("usage_once", func(t *testing.T) {
		resp := call(t, "POST", base+"/v1/chat/completions", ompKey, nil, map[string]any{
			"model": "glm-5.3-flash", "stream": true, "stream_options": map[string]bool{"include_usage": true},
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			got.mu.Lock()
			n := len(got.records)
			got.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(time.Second)
		got.mu.Lock()
		defer got.mu.Unlock()
		if len(got.records) != 1 {
			t.Fatalf("want one usage record, got %d", len(got.records))
		}
		rec := got.records[0]
		if rec["alias"] != "glm-5.3-flash" || rec["input_tokens"].(float64) != 7 || rec["output_tokens"].(float64) != 3 {
			t.Fatalf("record %v", rec)
		}
		if hash, _ := rec["key_hash"].(string); len(hash) != 64 || strings.Contains(hash, ompKey) {
			t.Fatalf("key hash %v", rec["key_hash"])
		}
	})
	// Every key class against every login type, on every route a client can
	// reach a model by. The requests carry their key in Authorization, or in
	// X-Goog-Api-Key on Gemini's routes, so an allowed cell also proves the
	// header is still there when the plugin's after-auth hook reads it. A
	// release that moves the hook off a route, or past the upstream call,
	// goes red on that route's row.
	t.Run("key_scope", func(t *testing.T) {
		fakes := map[string]*fake{compatModel: compat, claudeModel: claude, codexModel: codex}
		harness := map[string]string{ompKey: compatModel, claudeKey: claudeModel, codexKey: codexModel, sentinel: ""}
		counts := func() map[string]int {
			n := map[string]int{}
			for m, f := range fakes {
				n[m] = f.count()
			}
			return n
		}
		reachedSince := func(before map[string]int) map[string]int {
			reached := map[string]int{}
			for m, f := range fakes {
				if n := f.count() - before[m]; n > 0 {
					reached[m] = n
				}
			}
			return reached
		}
		namesAKey := func(body string) bool {
			for _, key := range []string{ompKey, claudeKey, codexKey, sentinel} {
				if strings.Contains(body, key) {
					return true
				}
			}
			return false
		}
		short := func(body string) string {
			body = strings.Join(strings.Fields(body), " ")
			if len(body) > 110 {
				body = body[:110] + "..."
			}
			return body
		}
		messages := []map[string]string{{"role": "user", "content": "hi"}}
		contents := []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}
		// local routes are answered by the proxy itself: for a login with a
		// custom base URL upstream counts tokens locally (Claude's at
		// internal/runtime/executor/claude_executor_tokens.go:27-32), so the
		// fakes cannot serve them. Their allowed cells reach no upstream;
		// their refusals still prove the hook runs.
		routes := []struct {
			name, path, header string
			body               func(model string) map[string]any
			local              bool
		}{
			{"POST /v1/chat/completions", "/v1/chat/completions", "",
				func(m string) map[string]any { return map[string]any{"model": m, "messages": messages} }, false},
			{"POST /v1/chat/completions stream", "/v1/chat/completions", "",
				func(m string) map[string]any { return map[string]any{"model": m, "messages": messages, "stream": true} }, false},
			{"POST /v1/completions", "/v1/completions", "",
				func(m string) map[string]any { return map[string]any{"model": m, "prompt": "hi"} }, false},
			{"POST /v1/messages", "/v1/messages", "",
				func(m string) map[string]any {
					return map[string]any{"model": m, "max_tokens": 16, "messages": messages}
				}, false},
			{"POST /v1/messages stream", "/v1/messages", "",
				func(m string) map[string]any {
					return map[string]any{"model": m, "max_tokens": 16, "messages": messages, "stream": true}
				}, false},
			{"POST /v1/messages/count_tokens", "/v1/messages/count_tokens", "",
				func(m string) map[string]any { return map[string]any{"model": m, "messages": messages} }, true},
			{"POST /v1/responses", "/v1/responses", "",
				func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }, false},
			{"POST /v1/responses stream", "/v1/responses", "",
				func(m string) map[string]any { return map[string]any{"model": m, "input": "hi", "stream": true} }, false},
			{"POST /v1/responses/compact", "/v1/responses/compact", "",
				func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }, false},
			{"POST /backend-api/codex/responses", "/backend-api/codex/responses", "",
				func(m string) map[string]any { return map[string]any{"model": m, "input": "hi", "stream": true} }, false},
			{"POST /backend-api/codex/responses/compact", "/backend-api/codex/responses/compact", "",
				func(m string) map[string]any { return map[string]any{"model": m, "input": "hi"} }, false},
			// There is no Gemini login: a Gemini route asking for another
			// login's model is served by that login, so it is scoped by that
			// login's harness.
			{"POST /v1beta/models/<model>:generateContent", "/v1beta/models/<model>:generateContent", "X-Goog-Api-Key",
				func(string) map[string]any { return map[string]any{"contents": contents} }, false},
			{"POST /v1beta/models/<model>:countTokens", "/v1beta/models/<model>:countTokens", "X-Goog-Api-Key",
				func(string) map[string]any { return map[string]any{"contents": contents} }, true},
		}
		for _, route := range routes {
			for _, key := range []string{ompKey, claudeKey, codexKey, sentinel} {
				for _, model := range []string{compatModel, claudeModel, codexModel} {
					name := fmt.Sprintf("%-44s %-15s -> %s", route.name, key, model)
					bearer, header := key, map[string]string(nil)
					if route.header != "" {
						bearer, header = "", map[string]string{route.header: key}
					}
					before := counts()
					resp := call(t, "POST", base+strings.ReplaceAll(route.path, "<model>", model), bearer, header, route.body(model))
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					status, body, reached := resp.StatusCode, string(raw), reachedSince(before)
					var refusal struct {
						Error struct{ Type, Message string }
					}
					_ = json.Unmarshal(raw, &refusal)
					hits := 1
					if route.local {
						hits = 0
					}
					if harness[key] == model {
						if status != http.StatusOK || len(reached) > 1 || reached[model] != hits {
							t.Errorf("%s: want allowed with %d upstream call, got %d %s, upstreams reached %v", name, hits, status, short(body), reached)
						}
						t.Logf("%s allowed %d %s", name, status, short(body))
						continue
					}
					if status != http.StatusForbidden || refusal.Error.Type != "key_scope" || len(reached) != 0 {
						t.Errorf("%s: want a key_scope 403 and no upstream call, got %d %s, upstreams reached %v", name, status, short(body), reached)
					} else if namesAKey(body) {
						t.Errorf("%s: the refusal names a key: %s", name, body)
					}
					t.Logf("%s refused %d %s", name, status, strings.TrimSpace(body))
				}
			}
		}
		// The proxy takes the first configured key among the headers. A
		// made-up Claude key in Authorization beside a valid omp key in
		// X-Api-Key is an omp request: the host's caller scope says so.
		for _, model := range []string{compatModel, claudeModel} {
			before := counts()
			resp := call(t, "POST", base+"/v1/chat/completions", "claude_madeup", map[string]string{"X-Api-Key": ompKey},
				map[string]any{"model": model, "messages": messages})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			reached := reachedSince(before)
			name := "Bearer claude_madeup + X-Api-Key " + ompKey + " -> " + model
			switch {
			case model == compatModel && (resp.StatusCode != http.StatusOK || reached[model] != 1 || len(reached) != 1):
				t.Errorf("%s: want allowed, got %d %s, upstreams reached %v", name, resp.StatusCode, short(string(raw)), reached)
			case model != compatModel && (resp.StatusCode != http.StatusForbidden || !bytes.Contains(raw, []byte(`"key_scope"`)) || len(reached) != 0):
				t.Errorf("%s: want a key_scope 403 and no upstream call, got %d %s, upstreams reached %v", name, resp.StatusCode, short(string(raw)), reached)
			}
			t.Logf("%s %d %s", name, resp.StatusCode, short(string(raw)))
		}

		// GET /v1/responses, and Codex's own GET /backend-api/codex/responses,
		// are a websocket, its key checked once at the upgrade and its login
		// chosen on every turn. Each cell is two turns: the same model twice,
		// and, from a key's own model, a turn to another harness's. Upstream
		// writes a 403 from the hook as no frame at all, only a closed socket
		// (sdk/api/handlers/openai/openai_responses_websocket_forward.go:
		// 398-410 and 459-470), so a refused turn is a close with no
		// response frame, no upstream call and the plugin's refusal in the
		// proxy's log. An error frame that says key_scope is taken too.
		type wsCell struct {
			path, key string
			turns     [2]string
		}
		cells := []wsCell{}
		for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
			for _, key := range []string{ompKey, claudeKey, codexKey, sentinel} {
				for _, model := range []string{compatModel, claudeModel, codexModel} {
					cells = append(cells, wsCell{path, key, [2]string{model, model}})
				}
			}
			cells = append(cells, wsCell{path, ompKey, [2]string{compatModel, codexModel}},
				wsCell{path, claudeKey, [2]string{claudeModel, compatModel}}, wsCell{path, codexKey, [2]string{codexModel, claudeModel}})
		}
		for _, cell := range cells {
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+cell.path,
				http.Header{"Authorization": {"Bearer " + cell.key}})
			if err != nil {
				t.Errorf("ws %s %s: dial: %v", cell.path, cell.key, err)
				continue
			}
			for turn, model := range cell.turns {
				name := fmt.Sprintf("%-44s %-15s -> %s", fmt.Sprintf("GET %s websocket, turn %d", cell.path, turn+1), cell.key, model)
				before, refusalsBefore := counts(), log.count("gentility-usage: refused request")
				frame := map[string]any{"type": "response.create", "model": model, "input": []any{map[string]any{
					"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "hi"}}}}}
				if err := conn.WriteJSON(frame); err != nil {
					t.Errorf("%s: write: %v", name, err)
					break
				}
				// Read to the turn's end: response.completed, an error frame
				// or the socket closing.
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				var ended, closed string
				for ended == "" && closed == "" {
					_, msg, err := conn.ReadMessage()
					if err != nil {
						closed = err.Error()
						break
					}
					var typed struct{ Type string }
					_ = json.Unmarshal(msg, &typed)
					if typed.Type == "response.completed" || typed.Type == "error" {
						ended = string(msg)
					} else if !strings.HasPrefix(typed.Type, "response.") {
						ended = string(msg)
					}
				}
				reached := reachedSince(before)
				if harness[cell.key] == model {
					if !strings.Contains(ended, `"response.completed"`) || len(reached) != 1 || reached[model] != 1 {
						t.Errorf("%s: want allowed, got %q %s, upstreams reached %v", name, short(ended), closed, reached)
					}
					t.Logf("%s allowed %s", name, short(ended))
					continue
				}
				until := time.Now().Add(2 * time.Second)
				for log.count("gentility-usage: refused request") == refusalsBefore && time.Now().Before(until) {
					time.Sleep(20 * time.Millisecond)
				}
				refusalLogged := log.count("gentility-usage: refused request") > refusalsBefore
				keyScopeFrame := strings.Contains(ended, `"key_scope"`)
				if (closed == "" && !keyScopeFrame) || strings.Contains(ended, `"response.`) || len(reached) != 0 || !refusalLogged || namesAKey(ended) {
					t.Errorf("%s: want the socket refused with no upstream call, got frame %q, closed %q, upstreams reached %v, refusal logged %v",
						name, short(ended), closed, reached, refusalLogged)
				}
				t.Logf("%s refused, %s", name, orElse(ended, closed))
				// The proxy has closed the socket: there is no next turn.
				break
			}
			conn.Close()
		}
		if !log.has("gentility-usage: refused request") {
			t.Error("no refusal reached the proxy's log")
		}
	})
	// The server's canary: the sentinel, unprefixed, asking for a model an
	// openai-compatibility login serves. A live key scope refuses it before
	// any upstream call; anything else means the scope is not enforced.
	t.Run("scope_canary", func(t *testing.T) {
		before := compat.count()
		resp := server("scope_canary", map[string]string{})
		var refused struct{ Error struct{ Type string } }
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = json.Unmarshal(body, &refused)
		if resp.StatusCode != http.StatusForbidden || refused.Error.Type != "key_scope" {
			t.Fatalf("the canary answered %d %s, want 403 key_scope", resp.StatusCode, body)
		}
		if bytes.Contains(body, []byte(sentinel)) {
			t.Fatal("the canary's refusal names the sentinel")
		}
		if n := compat.count() - before; n != 0 {
			t.Fatalf("the canary reached the upstream %d times", n)
		}
	})
	t.Run("quota_provider", func(t *testing.T) {
		resp := call(t, "GET", base+"/v0/management/quota/providers", "", mgmt, nil)
		var listed struct {
			Providers []struct {
				PluginID           string   `json:"plugin_id"`
				SupportedProviders []string `json:"supported_providers"`
			}
		}
		_ = json.NewDecoder(resp.Body).Decode(&listed)
		resp.Body.Close()
		if len(listed.Providers) != 1 || listed.Providers[0].PluginID != "gentility-usage" ||
			strings.Join(listed.Providers[0].SupportedProviders, ",") != "codex,claude" {
			t.Fatalf("quota providers %+v", listed.Providers)
		}
	})
	t.Run("read_quota", func(t *testing.T) {
		resp := server("read_quota", map[string]string{})
		var record struct {
			Format int
			Logins []any
		}
		err := json.NewDecoder(resp.Body).Decode(&record)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || record.Format != 1 || record.Logins == nil {
			t.Fatalf("the record route answered %d, format %d, logins %v, %v", resp.StatusCode, record.Format, record.Logins, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		resp := server("delete_key", map[string]string{"key": ompKey})
		if resp.StatusCode/100 != 2 {
			t.Fatalf("DELETE answered %d", resp.StatusCode)
		}
		if !waitFor(false, ompKey) {
			t.Fatal("a deleted key was still accepted after 2 s")
		}
	})
	t.Run("reconcile_stale_etag", func(t *testing.T) {
		read := server("read_keys", map[string]string{})
		etag := read.Header.Get("ETag")
		if etag == "" {
			t.Fatal("no ETag on the v8 read")
		}
		server("add_key", map[string]string{"key": "k2"})
		stale := server("write_keys", map[string]string{"etag": etag, "key": "k3"})
		if stale.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("a stale If-Match answered %d, want 412", stale.StatusCode)
		}
		fresh := server("read_keys", map[string]string{})
		ok := server("write_keys", map[string]string{"etag": fresh.Header.Get("ETag"), "key": "k3"})
		if ok.StatusCode/100 != 2 {
			t.Fatalf("a fresh If-Match answered %d", ok.StatusCode)
		}
	})
	// The record route is served from the proxy's NoRoute fallback, where a
	// gap in management auth would hide. Each refusal counts towards upstream's
	// lockout of five failed keys per address, so these two run last.
	t.Run("read_quota_refuses_without_the_key", func(t *testing.T) {
		path := calls["read_quota"].Path
		for name, header := range map[string]map[string]string{"no key": nil, "a wrong key": {"X-Management-Key": "not-" + management}} {
			resp := call(t, "GET", base+path, "", header, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Logf("with %s the record route answered %d %s", name, resp.StatusCode, body)
			if resp.StatusCode/100 != 4 || bytes.Contains(body, []byte("logins")) {
				t.Errorf("with %s the record route did not refuse", name)
			}
		}
	})
	t.Run("every_call_sent", func(t *testing.T) {
		for name := range calls {
			if !used[name] {
				t.Errorf("calls.json names %s, which no step sent", name)
			}
		}
	})
}

// watched keeps the proxy's output so a test can wait for a line in it.
type watched struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (w *watched) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

func (w *watched) has(line string) bool { return w.count(line) > 0 }

func (w *watched) count(line string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Count(w.out.Bytes(), []byte(line))
}

func orElse(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

// fake is an upstream that counts the model requests reaching it.
type fake struct {
	*httptest.Server
	mu sync.Mutex
	n  int
}

func newFake(answer func(w http.ResponseWriter, r *http.Request, stream bool)) *fake {
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "fake-up"}}})
			return
		}
		f.mu.Lock()
		f.n++
		f.mu.Unlock()
		var asked struct{ Stream bool }
		_ = json.NewDecoder(r.Body).Decode(&asked)
		answer(w, r, asked.Stream)
	}))
	return f
}

// compaction is a /responses/compact answer.
func compaction(model string) map[string]any {
	return map[string]any{"id": "cmp1", "object": "response.compaction", "created_at": 1, "model": model,
		"output": []any{map[string]string{"type": "compaction", "encrypted_content": "opaque"}},
		"usage":  map[string]int{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}}
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}
