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
)

const management = "contract-management-key"
const sentinel = "contract-sentinel"

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

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "fake-up"}}})
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"fake-up","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"fake-up","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

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
  api-keys: [%q]
openai-compatibility:
  - name: fake
    base-url: %q
    keys:
      - api-key: "upstream-key"
    models:
      - name: "fake-up"
        alias: "glm-5.3-flash"
plugins:
  enabled: true
  dir: %q
  configs:
    gentility-usage:
      enabled: true
`, port, filepath.Join(dir, "auths"), management, sentinel, upstream.URL+"/v1", pluginDir)
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
		resp := server("add_key", map[string]string{"key": "k1"})
		if resp.StatusCode/100 != 2 {
			t.Fatalf("PATCH answered %d", resp.StatusCode)
		}
	})
	t.Run("probe", func(t *testing.T) {
		if !waitFor(true, "k1") {
			t.Fatal("an added key was not accepted within 2 s")
		}
	})
	t.Run("usage_once", func(t *testing.T) {
		resp := call(t, "POST", base+"/v1/chat/completions", "k1", nil, map[string]any{
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
		if hash, _ := rec["key_hash"].(string); len(hash) != 64 || strings.Contains(hash, "k1") {
			t.Fatalf("key hash %v", rec["key_hash"])
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
		resp := server("delete_key", map[string]string{"key": "k1"})
		if resp.StatusCode/100 != 2 {
			t.Fatalf("DELETE answered %d", resp.StatusCode)
		}
		if !waitFor(false, "k1") {
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

func (w *watched) has(line string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Contains(w.out.Bytes(), []byte(line))
}
