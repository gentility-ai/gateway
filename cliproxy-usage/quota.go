package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// login is the quota record of one Codex or Claude login. It names the login
// by auth_index and plan only: no token, account id or email is ever kept.
type login struct {
	AuthIndex string   `json:"auth_index"`
	Provider  string   `json:"provider"`
	Plan      string   `json:"plan,omitempty"`
	Windows   []window `json:"windows"`
	Resets    *int     `json:"resets,omitempty"`
	Credits   *credits `json:"credits,omitempty"`
	Poll      poll     `json:"poll"`
}

type window struct {
	Kind         string    `json:"kind"`
	Model        string    `json:"model,omitempty"`
	UsedFraction float64   `json:"used_fraction"`
	ResetsAt     time.Time `json:"resets_at"`
	Source       string    `json:"source"`
	ObservedAt   time.Time `json:"observed_at"`
}

type credits struct {
	Enabled bool   `json:"enabled"`
	Balance string `json:"balance,omitempty"`
}

type poll struct {
	State  string    `json:"state"`
	LastAt time.Time `json:"last_at,omitzero"`
	NextAt time.Time `json:"next_at,omitzero"`
	// Failures counts consecutive failed polls, for the back-off.
	Failures int `json:"failures,omitempty"`
	// Credential is the login file's last_refresh when polling stopped on a
	// 401 or 403; polling resumes once it changes.
	Credential string `json:"credential,omitempty"`
}

const (
	fiveHour    = "five_hour"
	weekly      = "weekly"
	weeklyModel = "weekly_model"

	pollOK              = "ok"
	backingOff          = "backing_off"
	stoppedUnauthorized = "stopped_unauthorized"
)

// codexWindows reads a wham/usage reply. Fields it does not name are ignored.
func codexWindows(raw []byte, now time.Time) (login, error) {
	type slot struct {
		UsedPercent        float64 `json:"used_percent"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
		ResetAt            int64   `json:"reset_at"`
	}
	var reply struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *slot `json:"primary_window"`
			Secondary *slot `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits *struct {
			HasCredits bool            `json:"has_credits"`
			Balance    json.RawMessage `json:"balance"`
		} `json:"credits"`
		ResetCredits *struct {
			AvailableCount int `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return login{}, err
	}
	got := login{Plan: reply.PlanType, Windows: []window{}}
	for _, s := range []*slot{reply.RateLimit.Primary, reply.RateLimit.Secondary} {
		if s == nil {
			continue
		}
		if kind := kindForMinutes(s.LimitWindowSeconds / 60); kind != "" {
			got.Windows = append(got.Windows, window{Kind: kind, UsedFraction: s.UsedPercent / 100,
				ResetsAt: time.Unix(s.ResetAt, 0).UTC(), Source: "poll", ObservedAt: now})
		}
	}
	if reply.ResetCredits != nil {
		got.Resets = &reply.ResetCredits.AvailableCount
	}
	if reply.Credits != nil {
		got.Credits = &credits{Enabled: reply.Credits.HasCredits, Balance: strings.Trim(string(reply.Credits.Balance), `"`)}
	}
	return got, nil
}

// claudeWindows reads an oauth/usage reply from its limits[], the general
// form, which carries the per-model weekly cap. The top-level five_hour
// stands in when limits[] has no five-hour entry. Claude sends about twenty
// code-named fields besides; they are ignored.
func claudeWindows(raw []byte, now time.Time) (login, error) {
	var reply struct {
		FiveHour *struct {
			Utilization float64   `json:"utilization"`
			ResetsAt    time.Time `json:"resets_at"`
		} `json:"five_hour"`
		Limits []struct {
			Group    string    `json:"group"`
			Percent  float64   `json:"percent"`
			ResetsAt time.Time `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					ID          *string `json:"id"`
					DisplayName string  `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
		ExtraUsage *struct {
			IsEnabled bool `json:"is_enabled"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return login{}, err
	}
	got := login{Windows: []window{}}
	for _, l := range reply.Limits {
		w := window{UsedFraction: l.Percent / 100, ResetsAt: l.ResetsAt.UTC(), Source: "poll", ObservedAt: now}
		switch {
		case l.Group == "weekly" && l.Scope == nil:
			w.Kind = weekly
		case l.Group == "weekly" && l.Scope.Model != nil:
			w.Kind, w.Model = weeklyModel, l.Scope.Model.DisplayName
			if id := l.Scope.Model.ID; id != nil && *id != "" {
				w.Model = *id
			}
		case l.Group == "session" || l.Group == fiveHour:
			w.Kind = fiveHour
		default:
			continue
		}
		got.Windows = append(got.Windows, w)
	}
	if reply.FiveHour != nil && !hasKind(got.Windows, fiveHour) {
		got.Windows = append(got.Windows, window{Kind: fiveHour, UsedFraction: reply.FiveHour.Utilization / 100,
			ResetsAt: reply.FiveHour.ResetsAt.UTC(), Source: "poll", ObservedAt: now})
	}
	if reply.ExtraUsage != nil {
		got.Credits = &credits{Enabled: reply.ExtraUsage.IsEnabled}
	}
	return got, nil
}

// headerWindows reads the quota headers of a real Codex or Claude reply,
// the same headers upstream's passive snapshot collects.
func headerWindows(provider string, h http.Header, now time.Time) []window {
	ws := []window{}
	add := func(kind, used string, scale float64, resetsAt time.Time) {
		fraction, err := strconv.ParseFloat(used, 64)
		if kind == "" || err != nil || resetsAt.IsZero() {
			return
		}
		ws = append(ws, window{Kind: kind, UsedFraction: fraction / scale, ResetsAt: resetsAt, Source: "response", ObservedAt: now})
	}
	switch provider {
	case "codex":
		for _, slot := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + slot + "-"
			minutes, _ := strconv.ParseInt(h.Get(prefix+"Window-Minutes"), 10, 64)
			resetsAt := unix(h.Get(prefix + "Reset-At"))
			if after, err := strconv.ParseInt(h.Get(prefix+"Reset-After-Seconds"), 10, 64); resetsAt.IsZero() && err == nil {
				resetsAt = now.Add(time.Duration(after) * time.Second).Truncate(time.Second)
			}
			add(kindForMinutes(minutes), h.Get(prefix+"Used-Percent"), 100, resetsAt)
		}
	case "claude":
		for claim, kind := range map[string]string{"5h": fiveHour, "7d": weekly} {
			prefix := "Anthropic-Ratelimit-Unified-" + claim + "-"
			add(kind, h.Get(prefix+"Utilization"), 1, unix(h.Get(prefix+"Reset")))
		}
	}
	return ws
}

func kindForMinutes(minutes int64) string {
	switch minutes {
	case 5 * 60:
		return fiveHour
	case 7 * 24 * 60:
		return weekly
	}
	return ""
}

func unix(s string) time.Time {
	seconds, err := strconv.ParseInt(s, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func hasKind(ws []window, kind string) bool {
	for _, w := range ws {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

// merge replaces each window of the same kind and model, and adds the rest.
func (l *login) merge(ws []window) {
	for _, w := range ws {
		replaced := false
		for i := range l.Windows {
			if l.Windows[i].Kind == w.Kind && l.Windows[i].Model == w.Model {
				l.Windows[i], replaced = w, true
			}
		}
		if !replaced {
			l.Windows = append(l.Windows, w)
		}
	}
}

// quotaFetch is upstream's normalized quota shape (pluginapi.QuotaFetchResponse),
// mirrored as the host decodes it.
type quotaFetch struct {
	Subscription *struct {
		Plan string `json:"plan,omitempty"`
	} `json:"subscription,omitempty"`
	Summary []quotaMetric `json:"summary,omitempty"`
	Groups  []quotaGroup  `json:"groups,omitempty"`
}

type quotaMetric struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Value  float64 `json:"value"`
	Format string  `json:"format,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// fetch is the record as the proxy's quota page and route show it.
func (l login) fetch() quotaFetch {
	out := quotaFetch{}
	if l.Plan != "" {
		out.Subscription = &struct {
			Plan string `json:"plan,omitempty"`
		}{l.Plan}
	}
	if l.Resets != nil {
		out.Summary = append(out.Summary, quotaMetric{Key: "resets", Label: "Resets held", Value: float64(*l.Resets), Format: "number"})
	}
	if l.Credits != nil {
		if balance, err := strconv.ParseFloat(l.Credits.Balance, 64); err == nil {
			out.Summary = append(out.Summary, quotaMetric{Key: "credits", Label: "Credits", Value: balance, Format: "number"})
		}
	}
	name := map[string]string{"codex": "Codex", "claude": "Claude"}[l.Provider]
	switch l.Poll.State {
	case backingOff:
		name += fmt.Sprintf(" (polling backs off until %s)", l.Poll.NextAt.Format(time.RFC3339))
	case stoppedUnauthorized:
		name += " (polling stopped: the login was refused)"
	}
	group := quotaGroup{DisplayName: name, Buckets: []quotaBucket{}}
	for _, w := range l.Windows {
		label := map[string]string{fiveHour: "5-hour", weekly: "Weekly", weeklyModel: "Weekly · " + w.Model}[w.Kind]
		group.Buckets = append(group.Buckets, quotaBucket{
			Window:            label,
			RemainingFraction: max(0, 1-w.UsedFraction),
			ResetTime:         w.ResetsAt.Format(time.RFC3339),
			Description:       fmt.Sprintf("%.0f%% used, from the %s at %s", w.UsedFraction*100, w.Source, w.ObservedAt.Format(time.RFC3339)),
		})
	}
	out.Groups = []quotaGroup{group}
	return out
}

// shown is a login as the record route shows it. Its poll leaves out the
// failure count and the credential stamp, which only the watcher reads.
type shown struct {
	login
	Poll shownPoll `json:"poll"`
}

type shownPoll struct {
	State  string    `json:"state"`
	LastAt time.Time `json:"last_at,omitzero"`
	NextAt time.Time `json:"next_at,omitzero"`
}

func (l login) shown() shown {
	return shown{login: l, Poll: shownPoll{State: l.Poll.State, LastAt: l.Poll.LastAt, NextAt: l.Poll.NextAt}}
}
