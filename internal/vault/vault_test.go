package vault

import (
	"testing"

	"github.com/zalando/go-keyring"
)

func TestVaultRoundTrip(t *testing.T) {
	keyring.MockInit() // never touch the real keyring in tests
	dir := t.TempDir()
	v, warn, err := Open(dir)
	if err != nil || warn != "" {
		t.Fatal(err, warn)
	}
	v.Set("h1", "s3cret")
	v.Set("h2", "x")
	v.Set("h2", "") // empty removes
	if err := v.Save(); err != nil {
		t.Fatal(err)
	}
	v2, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Get("h1") != "s3cret" || len(v2.All()) != 1 {
		t.Fatalf("got %v", v2.All())
	}
}

func TestUnsealRejectsTampering(t *testing.T) {
	key := newKey()
	b, _ := Seal(key, []byte("hello"))
	b[len(b)-1] ^= 1
	if _, err := Unseal(key, b); err == nil {
		t.Fatal("tampered ciphertext must not decrypt")
	}
	if _, err := Unseal(newKey(), b[:10]); err == nil {
		t.Fatal("short input must fail")
	}
}
