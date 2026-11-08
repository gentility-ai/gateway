package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// record is one call as the server reads it. The key that made the call is
// hashed here, so its value never leaves the proxy node.
type record struct {
	RequestID           string    `json:"request_id"`
	KeyHash             string    `json:"key_hash"`
	Model               string    `json:"model"`
	Alias               string    `json:"alias"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	ReasoningTokens     int64     `json:"reasoning_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	RequestedAt         time.Time `json:"requested_at"`
	Failed              bool      `json:"failed"`
}

// hostRecord is the part of pluginapi.UsageRecord this plugin reads. The host
// marshals that struct with its Go field names; it is mirrored, not imported,
// so the plugin depends on nothing of CLIProxyAPI's but sdk/pluginabi.
type hostRecord struct {
	RequestID   string
	APIKey      string
	Model       string
	Alias       string
	Failed      bool
	RequestedAt time.Time
	Detail      struct {
		InputTokens         int64
		OutputTokens        int64
		ReasoningTokens     int64
		CacheReadTokens     int64
		CacheCreationTokens int64
	}
}

func fromHost(raw []byte) (record, bool) {
	var host hostRecord
	if err := json.Unmarshal(raw, &host); err != nil || host.APIKey == "" || !usableID(host.RequestID) {
		return record{}, false
	}
	sum := sha256.Sum256([]byte(host.APIKey))
	return record{
		RequestID:           host.RequestID,
		KeyHash:             hex.EncodeToString(sum[:]),
		Model:               host.Model,
		Alias:               host.Alias,
		InputTokens:         host.Detail.InputTokens,
		OutputTokens:        host.Detail.OutputTokens,
		ReasoningTokens:     host.Detail.ReasoningTokens,
		CacheReadTokens:     host.Detail.CacheReadTokens,
		CacheCreationTokens: host.Detail.CacheCreationTokens,
		RequestedAt:         host.RequestedAt,
		Failed:              host.Failed,
	}, true
}

// usableID is a request id the server takes (at most 255 bytes, valid text, no
// NUL) and the spool can name a file by. Any other id would be refused with a
// 422 on every retry and hold back every record behind it.
func usableID(id string) bool {
	return id != "" && len(id) <= 255 && utf8.ValidString(id) && !strings.ContainsAny(id, "/\\\x00") &&
		id != "." && id != ".."
}
