package tui

import "testing"

func TestFuzzyScore(t *testing.T) {
	if fuzzyScore("xyz", "build server") != -1 {
		t.Error("non-match should be -1")
	}
	if fuzzyScore("bs", "build server") <= fuzzyScore("bs", "xbxxsx") {
		t.Error("word-start matches should beat mid-word ones")
	}
	if fuzzyScore("serv", "build server") <= fuzzyScore("bsrv", "build server") {
		t.Error("substring should beat scattered")
	}
	if fuzzyScore("PROX", "proxmox 2") < 0 {
		t.Error("case-insensitive")
	}
}

func TestSplitDropped(t *testing.T) {
	cases := map[string][]string{
		`/tmp/a\ b.txt /tmp/c`:         {"/tmp/a b.txt", "/tmp/c"},
		`'/tmp/x y' "/tmp/z"`:          {"/tmp/x y", "/tmp/z"},
		"file:///tmp/q\nfile:///tmp/r": {"/tmp/q", "/tmp/r"},
	}
	for in, want := range cases {
		got := splitDropped(in)
		if len(got) != len(want) {
			t.Fatalf("%q: got %q", in, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: got %q want %q", in, got, want)
			}
		}
	}
}

func TestTildify(t *testing.T) {
	for _, c := range [][3]string{
		{"/home/k", "/home/k", "~"},
		{"/home/k/app", "/home/k", "~/app"},
		{"/home/kx", "/home/k", "/home/kx"},
		{"/etc", "/", "/etc"},
	} {
		if got := tildify(c[0], c[1]); got != c[2] {
			t.Errorf("tildify(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
