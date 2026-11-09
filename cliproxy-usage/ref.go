package main

import (
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

// makeRefKey writes a new key, readable by the proxy's user only. The file is
// created exclusively, so a key once made is never overwritten.
func makeRefKey(file string) ([]byte, error) {
	key := make([]byte, refKeySize)
	rand.Read(key)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return key, dir.Sync()
}

// loginRef is the login's ref, or "" when there is no key or no account id.
func loginRef(key []byte, provider, account string) string {
	if key == nil || account == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(provider + "\n" + account))
	return hex.EncodeToString(mac.Sum(nil))
}

// accountID is the provider's id for the account a login file signs in as:
// for Codex the id_token's account claim, else the file's account_id (the
// pair a poll sends as ChatGPT-Account-Id); for Claude the account_uuid
// upstream writes at sign-in and at each refresh.
func accountID(provider string, cred credential) string {
	if provider == "codex" {
		if account := chatgptAccountID(cred.IDToken); account != "" {
			return account
		}
		return cred.AccountID
	}
	return cred.AccountUUID
}
