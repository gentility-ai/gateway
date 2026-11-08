package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

var observed = time.Date(2026, 11, 7, 14, 48, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCodexReplyGivesTheWeeklyWindowResetsAndCredits(t *testing.T) {
	got, err := codexWindows(fixture(t, "codex-wham-usage.json"), observed)
	if err != nil {
		t.Fatal(err)
	}
	want := []window{{Kind: weekly, UsedFraction: 0.97, ResetsAt: time.Unix(1794543707, 0).UTC(), Source: "poll", ObservedAt: observed}}
	if got.Plan != "pro" || !reflect.DeepEqual(got.Windows, want) {
		t.Fatalf("got plan %q windows %+v", got.Plan, got.Windows)
	}
	if got.Resets == nil || *got.Resets != 0 || got.Credits == nil || got.Credits.Enabled || got.Credits.Balance != "0" {
		t.Fatalf("resets %v credits %+v", got.Resets, got.Credits)
	}
}

func TestClaudeReplyGivesItsLimits(t *testing.T) {
	got, err := claudeWindows(fixture(t, "claude-oauth-usage.json"), observed)
	if err != nil {
		t.Fatal(err)
	}
	want := []window{
		{Kind: weekly, UsedFraction: 1, ResetsAt: time.Date(2026, 11, 8, 8, 0, 0, 203505000, time.UTC), Source: "poll", ObservedAt: observed},
		{Kind: weeklyModel, Model: "Fable", UsedFraction: 1, ResetsAt: time.Date(2026, 11, 8, 8, 0, 0, 203666000, time.UTC), Source: "poll", ObservedAt: observed},
	}
	if !reflect.DeepEqual(got.Windows, want) {
		t.Fatalf("windows %+v", got.Windows)
	}
	if got.Credits == nil || got.Credits.Enabled || got.Resets != nil {
		t.Fatalf("credits %+v resets %v", got.Credits, got.Resets)
	}
}

func TestClaudeFiveHourStandsInWhenLimitsHaveNone(t *testing.T) {
	raw := []byte(`{"five_hour":{"utilization":42.0,"resets_at":"2026-11-07T18:00:00+00:00"},"limits":[]}`)
	got, err := claudeWindows(raw, observed)
	if err != nil || len(got.Windows) != 1 || got.Windows[0].Kind != fiveHour || got.Windows[0].UsedFraction != 0.42 {
		t.Fatalf("got %+v, %v", got.Windows, err)
	}
}

func TestUnknownFieldsAreIgnoredInBothReplies(t *testing.T) {
	extra := []byte(`{"brand_new_field":{"nested":[1,"two",null]},"another_code_name":null,`)
	for name, parse := range map[string]func([]byte, time.Time) (login, error){
		"codex-wham-usage.json":   codexWindows,
		"claude-oauth-usage.json": claudeWindows,
	} {
		raw := fixture(t, name)
		plain, err := parse(raw, observed)
		if err != nil {
			t.Fatal(err)
		}
		grown, err := parse(append(extra, bytes.TrimPrefix(raw, []byte("{"))...), observed)
		if err != nil {
			t.Fatalf("%s: an unknown field failed the parse: %v", name, err)
		}
		if !reflect.DeepEqual(plain, grown) {
			t.Fatalf("%s: an unknown field changed the record", name)
		}
	}
}

func TestReplyHeadersGiveWindows(t *testing.T) {
	codex := http.Header{}
	codex.Set("X-Codex-Primary-Used-Percent", "98")
	codex.Set("X-Codex-Primary-Window-Minutes", "10080")
	codex.Set("X-Codex-Primary-Reset-At", "1794543707")
	codex.Set("X-Codex-Secondary-Used-Percent", "12.5")
	codex.Set("X-Codex-Secondary-Window-Minutes", "300")
	codex.Set("X-Codex-Secondary-Reset-After-Seconds", "600")
	got := headerWindows("codex", codex, observed)
	want := []window{
		{Kind: weekly, UsedFraction: 0.98, ResetsAt: time.Unix(1794543707, 0).UTC(), Source: "response", ObservedAt: observed},
		{Kind: fiveHour, UsedFraction: 0.125, ResetsAt: observed.Add(10 * time.Minute), Source: "response", ObservedAt: observed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex %+v", got)
	}

	claude := http.Header{}
	claude.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	claude.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1794643200")
	got = headerWindows("claude", claude, observed)
	if len(got) != 1 || got[0].Kind != weekly || got[0].UsedFraction != 0.5 || !got[0].ResetsAt.Equal(time.Unix(1794643200, 0)) {
		t.Fatalf("claude %+v", got)
	}
	if len(headerWindows("codex", claude, observed)) != 0 {
		t.Fatal("one provider's headers updated another's windows")
	}
}

func TestFetchShowsEachWindowAndWhatIsHeld(t *testing.T) {
	two := 2
	l := login{AuthIndex: "a1", Provider: "codex", Plan: "pro", Resets: &two, Credits: &credits{Enabled: true, Balance: "12.5"},
		Windows: []window{{Kind: weekly, UsedFraction: 0.97, ResetsAt: time.Unix(1794543707, 0).UTC(), Source: "poll", ObservedAt: observed}},
		Poll:    poll{State: pollOK}}
	got := l.fetch()
	if got.Subscription == nil || got.Subscription.Plan != "pro" || len(got.Summary) != 2 || got.Summary[0].Value != 2 || got.Summary[1].Value != 12.5 {
		t.Fatalf("got %+v", got)
	}
	b := got.Groups[0].Buckets[0]
	if got.Groups[0].DisplayName != "Codex" || b.Window != "Weekly" || b.ResetTime != "2026-11-13T04:21:47Z" || b.RemainingFraction < 0.0299 || b.RemainingFraction > 0.0301 {
		t.Fatalf("group %+v", got.Groups[0])
	}
}

func TestTheRecordRouteShowsEveryLoginInItsWireShape(t *testing.T) {
	three, later := 3, observed.Add(10*time.Minute)
	resetsAt := time.Unix(1794543707, 0).UTC()
	w := newWatcher(t.TempDir() + "/gentility-quota.json")
	w.logins = map[string]*login{
		"b2": {AuthIndex: "b2", Provider: "claude", Credits: &credits{Enabled: false},
			Windows: []window{{Kind: weeklyModel, Model: "claude-opus-4", UsedFraction: 1, ResetsAt: resetsAt, Source: "poll", ObservedAt: observed}},
			Poll:    poll{State: stoppedUnauthorized, LastAt: observed, Credential: "2026-11-07T12:00:00Z"}},
		"a1": {AuthIndex: "a1", Provider: "codex", Plan: "pro", Resets: &three, Credits: &credits{Enabled: true, Balance: "12.50"},
			Windows: []window{
				{Kind: fiveHour, UsedFraction: 0.12, ResetsAt: resetsAt, Source: "response", ObservedAt: observed},
				{Kind: weekly, UsedFraction: 0.97, ResetsAt: resetsAt, Source: "poll", ObservedAt: observed},
			},
			Poll: poll{State: pollOK, LastAt: observed, NextAt: later}},
		"c3": {AuthIndex: "c3", Provider: "codex", Windows: []window{}, Poll: poll{State: backingOff, LastAt: observed, NextAt: later, Failures: 2}},
	}
	want := `{"format":1,"logins":[
		{"auth_index":"a1","provider":"codex","plan":"pro","resets":3,"credits":{"enabled":true,"balance":"12.50"},
		 "windows":[
			{"kind":"five_hour","used_fraction":0.12,"resets_at":"2026-11-13T04:21:47Z","source":"response","observed_at":"2026-11-07T14:48:00Z"},
			{"kind":"weekly","used_fraction":0.97,"resets_at":"2026-11-13T04:21:47Z","source":"poll","observed_at":"2026-11-07T14:48:00Z"}],
		 "poll":{"state":"ok","last_at":"2026-11-07T14:48:00Z","next_at":"2026-11-07T14:58:00Z"}},
		{"auth_index":"b2","provider":"claude","credits":{"enabled":false},
		 "windows":[{"kind":"weekly_model","model":"claude-opus-4","used_fraction":1,"resets_at":"2026-11-13T04:21:47Z","source":"poll","observed_at":"2026-11-07T14:48:00Z"}],
		 "poll":{"state":"stopped_unauthorized","last_at":"2026-11-07T14:48:00Z"}},
		{"auth_index":"c3","provider":"codex","windows":[],
		 "poll":{"state":"backing_off","last_at":"2026-11-07T14:48:00Z","next_at":"2026-11-07T14:58:00Z"}}]}`
	var got, expected any
	if err := json.Unmarshal(w.record(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %s", w.record())
	}
}

func TestTheRecordRouteIsEmptyWithoutLogins(t *testing.T) {
	w := newWatcher(t.TempDir() + "/gentility-quota.json")
	if got := string(w.record()); got != `{"format":1,"logins":[]}` {
		t.Fatalf("got %s", got)
	}
}
