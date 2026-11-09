package main

import (
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A login's ref names it across restarts and auth_index changes without
// carrying its account id: HMAC-SHA256 over the provider and the account id,
// under a key only this node holds. The plugin makes the key itself, on its
// first start, in a file beside the quota record. Deleting the file rotates
// the key, and every login starts a new ref.
const refKeySize = 32

const refKeyFile = "gentility-quota-ref.key"

// loadRefKey reads the key, making it when there is none. A key that cannot
// be read or made leaves every login without a ref; it never stops the
// plugin.
func loadRefKey(file string) ([]byte, error) {
	key, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		key, err = makeRefKey(file)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != refKeySize {
		return nil, fmt.Errorf("%s holds %d bytes, not %d", file, len(key), refKeySize)
	}
	return key, nil
}

// makeRefKey writes a new key, readable by the proxy's user only. It is
// written whole beside the key file and linked into place, which fails when a
// key is already there: the file appears whole or not at all, and a key once
// made is never overwritten.
func makeRefKey(file string) ([]byte, error) {
	key := make([]byte, refKeySize)
	rand.Read(key)
	pending := file + ".pending"
	if err := writeDurably(pending, key); err != nil {
		return nil, err
	}
	defer os.Remove(pending)
	if err := os.Link(pending, file); errors.Is(err, os.ErrExist) {
		return os.ReadFile(file)
	} else if err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return key, dir.Sync()
}

// loginRef is the login's ref, or "" when there is no key or no identity.
func loginRef(key []byte, provider, identity string) string {
	if key == nil || identity == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(provider + "\n" + identity))
	return hex.EncodeToString(mac.Sum(nil))
}

// identity names the person a login file signs in as, or "" when the file
// does not say: for Codex the ChatGPT account (the id_token's claim, else the
// file's account_id) and the user within it, since every seat of a Team plan
// shares the account; for Claude the account_uuid upstream writes at sign-in
// and keeps through each refresh. Where Anthropic's profile does not answer,
// upstream makes that uuid up from the login file, so such a ref names the
// file rather than the account.
func identity(provider string, cred credential) string {
	if provider != "codex" {
		return cred.AccountUUID
	}
	account, user := codexClaims(cred.IDToken)
	account = cmp.Or(account, cred.AccountID)
	if account == "" || user == "" {
		return ""
	}
	return account + "\n" + user
}
