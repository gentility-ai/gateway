package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFlushPostsAndForgetsOnlyWhatTheServerAccepted(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct{ Records []record }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Records) != 2 {
			t.Errorf("posted %d", len(body.Records))
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"accepted": {"a"}})
	}))
	defer server.Close()
	s := &spool{dir: t.TempDir()}
	_ = s.append(record{RequestID: "a", KeyHash: "h"})
	_ = s.append(record{RequestID: "b", KeyHash: "h"})
	p := newPoster(server.URL, s)
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	left, _ := s.unsent(10)
	if len(left) != 1 || left[0].RequestID != "b" {
		t.Fatalf("left %v", left)
	}
}

func TestFlushKeepsEverythingOnAServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	s := &spool{dir: t.TempDir()}
	_ = s.append(record{RequestID: "a", KeyHash: "h"})
	if err := newPoster(server.URL, s).flush(context.Background()); err == nil {
		t.Fatal("a 503 is an error to retry")
	}
	left, _ := s.unsent(10)
	if len(left) != 1 {
		t.Fatalf("left %v", left)
	}
}

func TestBackoffDoublesToAMinute(t *testing.T) {
	b := backoff{}
	got := []int64{}
	for i := 0; i < 8; i++ {
		got = append(got, int64(b.next().Seconds()))
	}
	want := []int64{1, 2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
	b.reset()
	if b.next().Seconds() != 1 {
		t.Fatal("a success starts again at a second")
	}
}
