package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const (
	pollEvery  = 10 * time.Minute
	pollJitter = time.Minute
	pollFloor  = time.Minute
	backoffCap = time.Hour
)

var (
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
)

// A poll says what it is. Both usage endpoints answer it as they answer
// their own clients (checked against Codex's on 2026-11-08).
const pollUserAgent = "gentility-gateway (+https://github.com/gentility-ai/gateway)"

// watcher keeps the quota record of every Codex and Claude login: polled from
// each login's usage endpoint, updated from real replies' quota headers, and
// written through to a file so a restart does not start blind.
type watcher struct {
	mu     sync.Mutex
	logins map[string]*login
	// serving maps a request id to the login the host chose for it, from the
	// request's after-auth call until its reply's headers arrive.
	serving map[string]serving
	dirty   bool
	// saving orders writes of the file, which happen outside mu so that no
	// request waits on a disk.
	saving sync.Mutex
	file   string
	// refKey makes each login's ref; nil leaves every login without one.
	refKey []byte
	client *http.Client
	now    func() time.Time
}

func newWatcher(file string) *watcher {
	w := &watcher{logins: map[string]*login{}, serving: map[string]serving{}, file: file, client: &http.Client{Timeout: 20 * time.Second}, now: time.Now}
	key, err := loadRefKey(filepath.Join(filepath.Dir(file), refKeyFile))
	if err != nil {
		log.Printf("gentility-quota: no login_ref key, so no login carries a login_ref: %v", err)
	}
	w.refKey = key
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return w
	}
	var saved struct{ Logins []*login }
	if err == nil {
		err = json.Unmarshal(raw, &saved)
	}
	if err != nil {
		log.Printf("gentility-quota: starting without the saved record: %v", err)
		return w
	}
	for _, l := range saved.Logins {
		w.logins[l.AuthIndex] = l
	}
	return w
}

func (w *watcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			w.save()
			return
		case <-time.After(30 * time.Second):
		}
		w.sweep(ctx)
	}
}

// hostLogin is the part of a host.auth.list entry the watcher reads. The
// entry also carries the login's email and account; they are never decoded.
type hostLogin struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Disabled  bool   `json:"disabled"`
}

// credential is the part of a login file a poll needs. LastRefresh is the
// file's own refresh stamp, which changes when upstream refreshes the login
// or it is signed in again.
type credential struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	AccountID   string `json:"account_id"`
	AccountUUID string `json:"account_uuid"`
	LastRefresh string `json:"last_refresh"`
}

// readCredential reads a login file at the moment it is needed. The host's
// error is not passed on: it can name the file, and the file's name the
// login's email.
func readCredential(authIndex string) (credential, error) {
	var got struct{ JSON json.RawMessage }
	var cred credential
	if err := callHost(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": authIndex}, &got); err != nil {
		return cred, errors.New("the host did not hand over the login file")
	}
	if json.Unmarshal(got.JSON, &cred) != nil || cred.AccessToken == "" {
		return cred, errors.New("the login file holds no access token")
	}
	return cred, nil
}

// sweep follows the host's logins and polls each one that is due.
func (w *watcher) sweep(ctx context.Context) {
	var listed struct{ Files []hostLogin }
	if err := callHost(pluginabi.MethodHostAuthList, struct{}{}, &listed); err != nil {
		log.Printf("gentility-quota: the host did not list its logins")
		return
	}
	now := w.now()
	due := []hostLogin{}
	w.mu.Lock()
	seen := map[string]bool{}
	for _, h := range listed.Files {
		if (h.Provider != "codex" && h.Provider != "claude") || h.Disabled || h.AuthIndex == "" {
			continue
		}
		seen[h.AuthIndex] = true
		l := w.logins[h.AuthIndex]
		if l == nil {
			l = &login{AuthIndex: h.AuthIndex, Provider: h.Provider, Windows: []window{}, Poll: poll{State: pollOK}}
			w.logins[h.AuthIndex], w.dirty = l, true
		}
		if l.Poll.State == stoppedUnauthorized || l.due(now) {
			due = append(due, h)
		}
	}
	for id, s := range w.serving {
		if now.Sub(s.at) > pollEvery {
			delete(w.serving, id)
		}
	}
	// The host lists no logins while it is still loading them; keep the
	// record until it lists some.
	for index := range w.logins {
		if len(seen) > 0 && !seen[index] {
			delete(w.logins, index)
			w.dirty = true
		}
	}
	w.mu.Unlock()
	for _, h := range due {
		cred, err := readCredential(h.AuthIndex)
		w.mu.Lock()
		l := w.logins[h.AuthIndex]
		if l != nil && err == nil {
			w.setRef(l, cred)
		}
		ready := l != nil && l.ready(w.now(), cred.LastRefresh)
		w.mu.Unlock()
		if !ready {
			continue
		}
		status, retryAfter, body := 0, time.Duration(0), []byte(nil)
		if err == nil {
			status, retryAfter, body, err = w.ask(ctx, h.Provider, cred)
		}
		w.mu.Lock()
		if l := w.logins[h.AuthIndex]; l != nil {
			l.settle(status, retryAfter, body, err, cred.LastRefresh, w.now())
			w.dirty = true
		}
		w.mu.Unlock()
	}
	w.save()
}

// setRef sets a login's ref from the file it was just read from. A login
// signed in again as another account takes that account's ref, and drops
// the numbers it held for the last one until its next poll.
func (w *watcher) setRef(l *login, cred credential) {
	ref := loginRef(w.refKey, l.Provider, identity(l.Provider, cred))
	if ref == "" && w.refKey != nil && !l.unnamed {
		log.Printf("gentility-quota: %s login %s names no account, so it has no login_ref", l.Provider, l.AuthIndex)
	}
	l.unnamed = ref == ""
	if ref == l.LoginRef {
		return
	}
	if l.LoginRef != "" {
		l.Plan, l.Windows, l.Resets, l.Credits = "", []window{}, nil, nil
	}
	l.LoginRef, w.dirty = ref, true
}

// due reports whether a login's next poll has come, and a minute has passed
// since its last.
func (l *login) due(now time.Time) bool {
	return !now.Before(l.Poll.NextAt) && now.Sub(l.Poll.LastAt) >= pollFloor
}

// ready reports whether a login is to be polled now, given its login file's
// refresh stamp. A login stopped on a refused credential stays stopped until
// the stamp changes.
func (l *login) ready(now time.Time, lastRefresh string) bool {
	if l.Poll.State == stoppedUnauthorized {
		if lastRefresh == "" || lastRefresh == l.Poll.Credential {
			return false
		}
		l.Poll = poll{State: pollOK, LastAt: l.Poll.LastAt}
	}
	return l.due(now)
}

// ask calls a usage endpoint with the login's current bearer, which is
// dropped after the call.
func (w *watcher) ask(ctx context.Context, provider string, cred credential) (int, time.Duration, []byte, error) {
	url := claudeUsageURL
	if provider == "codex" {
		url = codexUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", pollUserAgent)
	if provider == "codex" {
		account, _ := codexClaims(cred.IDToken)
		if account = cmp.Or(account, cred.AccountID); account != "" {
			req.Header.Set("ChatGPT-Account-Id", account)
		}
	} else {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// A transport error names the URL, never the headers.
		return 0, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, retryAfter(resp.Header.Get("Retry-After"), w.now()), body, err
}

// codexClaims reads a Codex id_token's account and user claims. The token is
// not verified: it only names the account to ask about and the login's ref.
// The account is the ChatGPT workspace, which every seat of a Team plan
// shares; the user is the person.
func codexClaims(idToken string) (account, user string) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Sub  string `json:"sub"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			UserID    string `json:"chatgpt_user_id"`
			OldUserID string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	_ = json.Unmarshal(raw, &claims)
	return claims.Auth.AccountID, cmp.Or(claims.Auth.UserID, claims.Auth.OldUserID, claims.Sub)
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.Sub(now)
	}
	return 0
}

// settle records one poll's outcome. A 401 or 403 stops polling until the
// credential changes; any other failure backs off exponentially from twice
// the normal interval, waits at least as long as Retry-After asks, and never
// more than an hour.
func (l *login) settle(status int, retryAfter time.Duration, body []byte, err error, lastRefresh string, now time.Time) {
	l.Poll.LastAt = now
	if err == nil && status == http.StatusOK {
		parse := claudeWindows
		if l.Provider == "codex" {
			parse = codexWindows
		}
		var got login
		if got, err = parse(body, now); err == nil {
			l.Plan, l.Windows, l.Resets, l.Credits = got.Plan, got.Windows, got.Resets, got.Credits
			l.Poll = poll{State: pollOK, LastAt: now, NextAt: now.Add(pollEvery - pollJitter + rand.N(2*pollJitter))}
			return
		}
		err = fmt.Errorf("unreadable reply: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		l.Poll = poll{State: stoppedUnauthorized, LastAt: now, Credential: lastRefresh}
		log.Printf("gentility-quota: %s login %s answered %d; polling stopped until its credential changes", l.Provider, l.AuthIndex, status)
		return
	}
	l.Poll.Failures++
	wait := min(backoffCap, max(pollEvery<<min(l.Poll.Failures, 3), retryAfter))
	l.Poll.State, l.Poll.NextAt = backingOff, now.Add(wait)
	if err == nil {
		err = fmt.Errorf("answered %d", status)
	}
	log.Printf("gentility-quota: %s login %s: %v; next poll in %s", l.Provider, l.AuthIndex, err, wait)
}

type serving struct {
	authIndex string
	at        time.Time
}

// noteServing remembers which login the host chose for a request, from the
// request's after-auth call: the replies that follow name only the request.
func (w *watcher) noteServing(raw []byte) {
	var req struct {
		RequestID string
		Metadata  map[string]any
	}
	if json.Unmarshal(raw, &req) != nil || req.RequestID == "" {
		return
	}
	index, _ := req.Metadata["selected_auth_index"].(string)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.logins[index] != nil {
		w.serving[req.RequestID] = serving{authIndex: index, at: w.now()}
	}
}

// observeReply updates the windows of the login that served a reply from the
// reply's quota headers. A stream is read at its header call (chunk -1); its
// payload chunks carry nothing new.
func (w *watcher) observeReply(raw []byte) {
	var reply struct {
		RequestID       string
		ChunkIndex      *int
		ResponseHeaders http.Header
	}
	if json.Unmarshal(raw, &reply) != nil || (reply.ChunkIndex != nil && *reply.ChunkIndex != -1) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s, found := w.serving[reply.RequestID]
	if !found {
		return
	}
	delete(w.serving, reply.RequestID)
	if l := w.logins[s.authIndex]; l != nil {
		if ws := headerWindows(l.Provider, reply.ResponseHeaders, w.now()); len(ws) > 0 {
			l.merge(ws)
			w.dirty = true
		}
	}
}

func (w *watcher) fetch(authIndex string) quotaFetch {
	w.mu.Lock()
	defer w.mu.Unlock()
	if l := w.logins[authIndex]; l != nil {
		return l.fetch()
	}
	return quotaFetch{}
}

// record is every login's quota record as the record route answers it, in
// auth_index order.
func (w *watcher) record() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := struct {
		Format int     `json:"format"`
		Logins []shown `json:"logins"`
	}{Format: 1, Logins: []shown{}}
	for _, l := range w.logins {
		out.Logins = append(out.Logins, l.shown())
	}
	sort.Slice(out.Logins, func(i, j int) bool { return out.Logins[i].AuthIndex < out.Logins[j].AuthIndex })
	raw, _ := json.Marshal(out)
	return raw
}

// save writes the record through to its file when it has changed.
func (w *watcher) save() {
	w.saving.Lock()
	defer w.saving.Unlock()
	w.mu.Lock()
	if !w.dirty {
		w.mu.Unlock()
		return
	}
	saved := struct {
		Logins []*login `json:"logins"`
	}{Logins: []*login{}}
	for _, l := range w.logins {
		saved.Logins = append(saved.Logins, l)
	}
	sort.Slice(saved.Logins, func(i, j int) bool { return saved.Logins[i].AuthIndex < saved.Logins[j].AuthIndex })
	raw, _ := json.MarshalIndent(saved, "", "  ")
	w.dirty = false
	w.mu.Unlock()
	if err := writeDurably(w.file, raw); err != nil {
		log.Printf("gentility-quota: saving the record: %v", err)
		w.mu.Lock()
		w.dirty = true
		w.mu.Unlock()
	}
}
