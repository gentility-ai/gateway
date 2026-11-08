package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const batch = 50

type poster struct {
	url    string
	spool  *spool
	client *http.Client
	wake   chan struct{}
}

func newPoster(url string, s *spool) *poster {
	return &poster{url: url, spool: s, client: &http.Client{Timeout: 15 * time.Second}, wake: make(chan struct{}, 1)}
}

// flush posts unsent records in batches and forgets those the server
// accepted, until none are left or a post fails.
func (p *poster) flush(ctx context.Context) error {
	for {
		records, err := p.spool.unsent(batch)
		if err != nil || len(records) == 0 {
			return err
		}
		body, _ := json.Marshal(map[string][]record{"records": records})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			return err
		}
		var answer struct{ Accepted []string }
		decodeErr := json.NewDecoder(resp.Body).Decode(&answer)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("usage post answered %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return decodeErr
		}
		if err := p.spool.sent(answer.Accepted); err != nil {
			return err
		}
		// The server accepts a whole batch or answers 503; a shorter answer
		// ends this flush, and the rest is offered again on the next.
		if len(answer.Accepted) < len(records) {
			return nil
		}
	}
}

func (p *poster) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run flushes on every new record and on a timer, backing off after a failure.
func (p *poster) run(ctx context.Context) {
	b := backoff{}
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-time.After(wait):
		}
		if err := p.flush(ctx); err != nil {
			wait = b.next()
			continue
		}
		b.reset()
		wait = time.Minute
	}
}

type backoff struct{ n int }

func (b *backoff) next() time.Duration {
	d := time.Second << b.n
	if d > time.Minute || d <= 0 {
		return time.Minute
	}
	b.n++
	return d
}

func (b *backoff) reset() { b.n = 0 }
