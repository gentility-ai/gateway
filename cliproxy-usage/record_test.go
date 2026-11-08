package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestFromHostHashesTheKeyAndKeepsTheTokens(t *testing.T) {
	raw := []byte(`{"RequestID":"r1","APIKey":"secret","Model":"glm-up","Alias":"glm-5.3-flash",
	  "RequestedAt":"2026-10-30T10:00:00Z","Failed":false,
	  "Detail":{"InputTokens":10,"OutputTokens":20,"ReasoningTokens":3,"CacheReadTokens":4,"CacheCreationTokens":5}}`)
	rec, ok := fromHost(raw)
	if !ok {
		t.Fatal("a record with a key is kept")
	}
	sum := sha256.Sum256([]byte("secret"))
	if rec.KeyHash != hex.EncodeToString(sum[:]) || rec.InputTokens != 10 || rec.OutputTokens != 20 ||
		rec.ReasoningTokens != 3 || rec.CacheReadTokens != 4 || rec.CacheCreationTokens != 5 || rec.Alias != "glm-5.3-flash" {
		t.Fatalf("got %+v", rec)
	}
}

func TestFromHostDropsAKeylessRecord(t *testing.T) {
	if _, ok := fromHost([]byte(`{"RequestID":"r1"}`)); ok {
		t.Fatal("a record no key made is nobody's to bill")
	}
}

func TestFromHostDropsARequestIDTheServerWouldRefuse(t *testing.T) {
	long := `{"RequestID":"` + strings.Repeat("r", 256) + `","APIKey":"secret"}`
	if _, ok := fromHost([]byte(long)); ok {
		t.Fatal("an id the server answers 422 would wedge the spool")
	}
}
