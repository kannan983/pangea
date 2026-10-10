// Package vault stores host passwords encrypted (XChaCha20-Poly1305) in
// vault.bin. The key lives in the OS keyring (Secret Service / KWallet,
// Windows Credential Manager, macOS Keychain).
package vault

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	service = "pangea"
	account = "vault-key"
)

type Vault struct {
	path string
	key  []byte
	pw   map[string]string // host ID -> password
}

// Open loads the vault in dir. warn is non-empty when the OS keyring was
// unavailable and the key had to be kept in a file next to the vault.
func Open(dir string) (v *Vault, warn string, err error) {
	key, warn, err := loadKey(dir)
	if err != nil {
		return nil, "", err
	}
	v = &Vault{path: filepath.Join(dir, "vault.bin"), key: key, pw: map[string]string{}}
	b, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return v, warn, nil
	}
	if err != nil {
		return nil, "", err
	}
	plain, err := Unseal(key, b)
	if err != nil {
		return nil, "", fmt.Errorf("vault: %w (was it created with another key?)", err)
	}
	return v, warn, json.Unmarshal(plain, &v.pw)
}

// loadKey gets the vault key from the keyring, creating it on first use.
// ponytail: without a keyring the key sits in vault.key (0600) — protects
// against reading hosts/vault files alone, not against full account access.
func loadKey(dir string) ([]byte, string, error) {
	s, err := keyring.Get(service, account)
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(s)
		return key, "", err
	}
	keyFile := filepath.Join(dir, "vault.key")
	if !errors.Is(err, keyring.ErrNotFound) {
		warn := "OS keyring unavailable (" + err.Error() + "); vault key kept in " + keyFile
		if b, ferr := os.ReadFile(keyFile); ferr == nil {
			return b, warn, nil
		}
		key := newKey()
		return key, warn, os.WriteFile(keyFile, key, 0o600)
	}
	key := newKey()
	if err := keyring.Set(service, account, base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, "", err
	}
	return key, "", nil
}

func newKey() []byte {
	k := make([]byte, chacha20poly1305.KeySize)
	rand.Read(k)
	return k
}

func (v *Vault) Get(id string) string { return v.pw[id] }

func (v *Vault) Set(id, pw string) {
	if pw == "" {
		delete(v.pw, id)
		return
	}
	v.pw[id] = pw
}

// All returns a copy of every saved password, by host ID.
func (v *Vault) All() map[string]string {
	out := make(map[string]string, len(v.pw))
	for k, p := range v.pw {
		out[k] = p
	}
	return out
}

// Replace swaps in a full set of passwords (used by restore).
func (v *Vault) Replace(pw map[string]string) { v.pw = pw }

func (v *Vault) Save() error {
	plain, err := json.Marshal(v.pw)
	if err != nil {
		return err
	}
	b, err := Seal(v.key, plain)
	if err != nil {
		return err
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.path)
}

// Seal encrypts plain with a 32-byte key; the random nonce is prepended.
func Seal(key, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, nil), nil
}

// Unseal decrypts what Seal produced.
func Unseal(key, data []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
}
