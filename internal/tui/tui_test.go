package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/zalando/go-keyring"

	"github.com/kannan983/pangea/internal/fsx"
	"github.com/kannan983/pangea/internal/store"
	"github.com/kannan983/pangea/internal/vault"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, v, "")
	m.w, m.h = 140, 40
	return m
}

func TestSelectRegion(t *testing.T) {
	lines := []string{"hello world   ", "second line   ", "世界 wide     "}
	cases := []struct {
		sel  selection
		want string
	}{
		{selection{ax: 0, ay: 0, bx: 4, by: 0}, "hello"},
		{selection{ax: 6, ay: 0, bx: 5, by: 1}, "world\nsecond"},
		{selection{ax: 5, ay: 1, bx: 6, by: 0}, "world\nsecond"}, // dragged backwards
		{selection{ax: 0, ay: 2, bx: 3, by: 2}, "世界"},            // wide runes take 2 cells
		{selection{ax: 0, ay: 1, bx: 13, by: 9}, "second line\n世界 wide"},
	}
	for _, c := range cases {
		if got := selectRegion(lines, &c.sel, 14); got != c.want {
			t.Errorf("%+v: got %q want %q", c.sel, got, c.want)
		}
	}
}

func TestHighlightKeepsWidth(t *testing.T) {
	plain := []string{"abcdef", "ghijkl"}
	out := highlight([]string{"\x1b[31mabcdef\x1b[0m", "ghijkl"}, plain, &selection{ax: 2, ay: 0, bx: 1, by: 1}, 6)
	for i, l := range out {
		if w := ansi.StringWidth(l); w != 6 {
			t.Errorf("row %d width %d: %q", i, w, l)
		}
	}
	if !strings.Contains(out[0], "\x1b[7mcdef") || !strings.Contains(out[1], "\x1b[7mgh") {
		t.Errorf("selection not reversed: %q", out)
	}
}

func TestPaneRects(t *testing.T) {
	m := &Model{w: 160, h: 41}
	r := m.paneRects(2)
	if r[0].x != 0 || r[1].x != 80 || r[0].w+r[1].w != 160 || r[0].h != 40 {
		t.Fatalf("side by side: %+v", r)
	}
	m.w = 100
	r = m.paneRects(2)
	if r[1].y != 20 || r[0].h+r[1].h != 40 || r[1].w != 100 {
		t.Fatalf("stacked: %+v", r)
	}
	if in := (rect{0, 0, 80, 40}).inner(); in.w != 76 || in.h != 38 || in.x != 2 || in.y != 1 {
		t.Fatalf("inner %+v", in)
	}
}

func TestTransferLineStates(t *testing.T) {
	tr := &transfer{name: "release.tar.gz", start: time.Now().Add(-2 * time.Second)}
	tr.total.Store(1000)
	tr.pr.Bytes.Store(500)
	if l := ansi.Strip(transferLine(tr, 140)); !strings.Contains(l, " 50%") || !strings.Contains(l, "ETA") {
		t.Errorf("running: %q", l)
	}
	tr.finished, tr.err = time.Second, nil
	tr.pr.Bytes.Store(1000)
	if l := ansi.Strip(transferLine(tr, 140)); !strings.Contains(l, "done ✓") || strings.Contains(l, "ETA") {
		t.Errorf("done: %q", l)
	}
	tr.err = context.Canceled
	if l := ansi.Strip(transferLine(tr, 140)); !strings.Contains(l, "canceled") {
		t.Errorf("canceled: %q", l)
	}
	tr.err = errors.New("boom")
	if l := ansi.Strip(transferLine(tr, 140)); !strings.Contains(l, "failed") {
		t.Errorf("failed: %q", l)
	}
}

func TestSparkline(t *testing.T) {
	if s := sparkline([]float64{0, 50, 100}, 10); s != "▁▅█" {
		t.Errorf("got %q", s)
	}
	if s := sparkline(make([]float64, 100), 10); len([]rune(s)) != 10 {
		t.Errorf("not clipped: %q", s)
	}
}

func TestEggCode(t *testing.T) {
	m := newTestModel(t)
	for _, k := range []string{"1", "2", "x", "1", "2", "0"} {
		if m.eggKey(k); m.mission != nil {
			t.Fatal("opened too early")
		}
	}
	m.eggKey("2")
	if m.mission == nil {
		t.Fatal("1202 did not open the panel")
	}
	if lines := strings.Join(m.viewMission(), "\n"); !strings.Contains(lines, "Margaret Hamilton") {
		t.Fatal("dedication missing")
	}
}

func TestChoicePrompt(t *testing.T) {
	m := newTestModel(t)
	var got string
	m.askChoice("t", "x", []choice{{"o", "overwrite"}, {"k", "keep both"}}, func(k string) tea.Cmd { got = k; return nil })
	m.promptKey(tea.KeyPressMsg{Code: 'z', Text: "z"}) // not an option: ignored
	if len(m.prompts) != 1 {
		t.Fatal("unknown key closed the prompt")
	}
	m.promptKey(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if got != "k" || len(m.prompts) != 0 {
		t.Fatalf("got %q, prompts %d", got, len(m.prompts))
	}
	called := false
	m.askConfirm("t", "x", func() tea.Cmd { called = true; return nil })
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if called {
		t.Fatal("esc confirmed")
	}
}

func TestDropTargetAndHistory(t *testing.T) {
	local, r1, r2 := &fpane{fs: fsx.Local{}}, &fpane{remote: true, fs: fsx.Local{}}, &fpane{remote: true, fs: fsx.Local{}}
	if got := dropTarget(&sftpView{panes: [2]*fpane{local, r1}}); got != 1 {
		t.Errorf("local+remote: %d", got)
	}
	if got := dropTarget(&sftpView{panes: [2]*fpane{r1, local}}); got != 0 {
		t.Errorf("remote+local: %d", got)
	}
	if got := dropTarget(&sftpView{panes: [2]*fpane{r1, r2}, active: 1}); got != 1 {
		t.Errorf("two remotes follow the active pane: %d", got)
	}

	v := &sftpView{panes: [2]*fpane{local, r1}}
	mk := func(err error, running bool) *transfer {
		tr := &transfer{name: "f", start: time.Now(), err: err, toFS: r1.fs, fromFS: local.fs}
		if !running {
			tr.finished = time.Second
		}
		return tr
	}
	v.transfers = []*transfer{mk(nil, false), mk(nil, false), mk(errors.New("x"), false), mk(context.Canceled, false), mk(nil, true)}
	if s := ansi.Strip(transferSummary(v)); s != " ✓ 2 done   ✗ 1 failed   ⊘ 1 canceled" {
		t.Errorf("summary %q", s)
	}
	if !busy(v, local) {
		t.Error("pane with a running transfer must be busy")
	}
	clearFinished(v)
	if len(v.transfers) != 1 || !v.transfers[0].running() {
		t.Fatalf("clear kept %d", len(v.transfers))
	}
	if transferSummary(v) != "" {
		t.Error("summary should be empty with only running transfers")
	}
	v.transfers[0].finished = time.Second
	if busy(v, local) {
		t.Error("finished transfers don't make a pane busy")
	}
	line := ansi.Strip(historyLine(&transfer{name: "app.tar.gz", fromLabel: "Local", fromDir: "~/x", toLabel: "build", toDir: "/opt",
		start: time.Now(), finished: time.Second}, 140))
	if !strings.Contains(line, "✓") || !strings.Contains(line, "Local ~/x → build /opt") {
		t.Errorf("history line %q", line)
	}
}

func TestPickPrompt(t *testing.T) {
	items := []pickItem{{label: "Local"}, {label: "build-server", detail: "Office"}, {label: "app-server", detail: "Office"}}
	if f := filterPick(items, "app"); len(f) != 1 || f[0].label != "app-server" {
		t.Fatalf("filter: %+v", f)
	}
	if len(filterPick(items, "")) != 3 {
		t.Fatal("empty query must list all")
	}
	m := newTestModel(t)
	picked := ""
	for i := range items {
		it := items[i]
		items[i].run = func() tea.Cmd { picked = it.label; return nil }
	}
	m.askPick("t", "x", items)
	for _, r := range "bld" {
		m.promptKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if picked != "build-server" || len(m.prompts) != 0 {
		t.Fatalf("picked %q, prompts %d", picked, len(m.prompts))
	}
	m.askPick("t", "x", items)
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.promptKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if picked != "app-server" {
		t.Fatalf("arrow pick %q", picked)
	}
}
