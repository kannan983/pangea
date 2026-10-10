package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSSHConfig(t *testing.T) {
	cfg := `
# comment
Host web1 web2
    HostName 10.0.0.5
    User deploy
    Port 2200
Host *.internal
    User nobody
Host db
  HostName=db.example.com
  IdentityFile "~/.ssh/db key"
Match host foo
  User ignored
`
	hosts, err := ParseSSHConfig(strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts: %+v", len(hosts), hosts)
	}
	w := hosts[1]
	if w.Label != "web2" || w.Hostname != "10.0.0.5" || w.User != "deploy" || w.Port != 2200 {
		t.Errorf("web2: %+v", w)
	}
	d := hosts[2]
	if d.Hostname != "db.example.com" || d.KeyPath != "~/.ssh/db key" || d.User != "" {
		t.Errorf("db: %+v", d)
	}
}

func TestStoreRoundTripAndImportDedup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Put(Host{Label: "web1", Hostname: "x"})
	n, err := s.Import(strings.NewReader("Host web1\nHost new1\n  User u\n"), "Office")
	if err != nil || n != 1 {
		t.Fatalf("import added %d, err %v", n, err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Hosts) != 2 || s2.Hosts[1].Label != "new1" || s2.Hosts[1].Mode != "Office" {
		t.Fatalf("reloaded: %+v", s2.Hosts)
	}
	s2.Delete(s2.Hosts[0].ID)
	if len(s2.Hosts) != 1 {
		t.Fatalf("delete failed: %+v", s2.Hosts)
	}
}

func TestModesMigrationAndOps(t *testing.T) {
	dir := t.TempDir()
	old := `[{"id":"a","label":"qa","group":"TTSQA","hostname":"x"},{"id":"b","label":"p","hostname":"y"}]`
	if err := os.WriteFile(filepath.Join(dir, "hosts.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Hosts[0].Mode != "TTSQA" || s.Hosts[0].Group != "" {
		t.Fatalf("group not migrated: %+v", s.Hosts[0])
	}
	if s.Settings.DefaultMode != "TTSQA" || s.Hosts[1].Mode != "TTSQA" {
		t.Fatalf("ungrouped host should get the default mode: %+v %+v", s.Settings, s.Hosts[1])
	}
	if err := s.AddMode("Office"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameMode("TTSQA", "Office"); err == nil {
		t.Fatal("rename onto an existing mode must fail")
	}
	if err := s.RenameMode("TTSQA", "Work"); err != nil || s.Hosts[0].Mode != "Work" || s.Settings.DefaultMode != "Work" {
		t.Fatalf("rename: %v %+v %+v", err, s.Hosts[0], s.Settings)
	}
	if err := s.DeleteMode("Work"); err == nil {
		t.Fatal("deleting a non-empty mode must fail")
	}
	if err := s.DeleteMode("Office"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	if len(s2.Settings.Modes) != 1 || s2.Settings.Modes[0] != "Work" || s2.ModeCount("Work") != 2 {
		t.Fatalf("reload: %+v", s2.Settings)
	}
}
