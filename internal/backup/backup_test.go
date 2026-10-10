package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/kannan983/pangea/internal/store"
	"github.com/kannan983/pangea/internal/vault"
)

func TestBackupRestore(t *testing.T) {
	keyring.MockInit()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	keyPath := filepath.Join(tmp, "id_test")
	os.WriteFile(keyPath, []byte("PRIVATE KEY"), 0o600)

	st, _ := store.Open(src)
	id := st.Put(store.Host{Label: "web", Hostname: "w", KeyPath: keyPath, Mode: "Office"})
	st.Put(store.Host{Label: "web2", Hostname: "w2", KeyPath: keyPath}) // same key, stored once
	st.Save()
	os.WriteFile(filepath.Join(src, "known_hosts"), []byte("w ssh-ed25519 AAAA\n"), 0o600)
	v, _, _ := vault.Open(src)
	v.Set(id, "pw1")

	file := filepath.Join(tmp, "b.pgb")
	n, err := Create(file, "correct horse", st, v)
	if err != nil || n != 1 {
		t.Fatalf("create: %d %v", n, err)
	}
	if _, err := Create(file, "x", st, v); err == nil {
		t.Fatal("must not overwrite an existing backup")
	}

	dst := filepath.Join(tmp, "dst")
	os.MkdirAll(dst, 0o700)
	os.WriteFile(filepath.Join(dst, "hosts.json"), []byte("[]"), 0o600)
	if _, err := Restore(file, "wrong", dst); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
	moved, err := Restore(file, "correct horse", dst)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(moved, "hosts.json")); string(b) != "[]" {
		t.Fatal("existing config was not kept aside")
	}

	st2, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dst, "keys", "id_test")
	if st2.Hosts[0].KeyPath != want || st2.Hosts[1].KeyPath != want {
		t.Fatalf("key paths not rewritten: %q %q", st2.Hosts[0].KeyPath, st2.Hosts[1].KeyPath)
	}
	if b, _ := os.ReadFile(want); string(b) != "PRIVATE KEY" {
		t.Fatal("key file not restored")
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key perms %v", fi.Mode().Perm())
	}
	v2, _, err := vault.Open(dst)
	if err != nil || v2.Get(id) != "pw1" {
		t.Fatalf("password not restored: %v", err)
	}
	if st2.Settings.DefaultMode != "Office" {
		t.Fatalf("settings: %+v", st2.Settings)
	}
}
