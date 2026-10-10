// Spike 3: two-pane local/remote SFTP browser.
//
// Keys: tab switch pane, up/down move, enter open dir, backspace parent,
// c copy selection to other pane, q quit.
// Mouse: click selects, wheel scrolls, drag a file onto the other pane copies.
// Paste (or drop a file from the desktop into the terminal): uploads the
// pasted local paths into the remote directory.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/pkg/sftp"

	"github.com/kannan983/pangea/spikes/sshx"
)

type pane struct {
	remote  bool
	dir     string
	entries []fs.FileInfo
	sel     int
	top     int
}

type transfer struct {
	name     string
	total    int64
	done     atomic.Int64
	start    time.Time
	finished time.Duration
	err      error
}

type tickMsg struct{}
type doneMsg struct{ t *transfer }

type model struct {
	sc        *sftp.Client
	panes     [2]*pane
	active    int
	w, h      int
	transfers []*transfer
	drag      *dragState
	status    string
}

type dragState struct {
	from int
	name string
}

func (m *model) load(p *pane) {
	var ents []fs.FileInfo
	var err error
	if p.remote {
		ents, err = m.sc.ReadDir(p.dir)
	} else {
		var des []os.DirEntry
		des, err = os.ReadDir(p.dir)
		for _, d := range des {
			if fi, e := d.Info(); e == nil {
				ents = append(ents, fi)
			}
		}
	}
	if err != nil {
		m.status = err.Error()
		return
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return ents[i].Name() < ents[j].Name()
	})
	p.entries, p.sel, p.top = ents, 0, 0
}

func (p *pane) join(name string) string {
	if p.remote {
		return path.Join(p.dir, name)
	}
	return filepath.Join(p.dir, name)
}

// copyFile copies src (in pane from) into the other pane's directory.
func (m *model) copyFile(from int, srcPath string, size int64) tea.Cmd {
	t := &transfer{name: filepath.Base(srcPath), total: size, start: time.Now()}
	m.transfers = append(m.transfers, t)
	upload := from == -1 || !m.panes[from].remote // -1: desktop drop
	dst := m.panes[0]
	if upload {
		dst = m.panes[1]
	}
	dstPath := dst.join(t.name)
	return func() tea.Msg {
		var closers []io.Closer
		open := func() error {
			if upload {
				lf, err := os.Open(srcPath)
				if err != nil {
					return err
				}
				rf, err := m.sc.Create(dstPath)
				if err != nil {
					lf.Close()
					return err
				}
				closers = []io.Closer{lf, rf}
				// *sftp.File.ReadFrom pipelines writes only when it knows the size.
				_, err = rf.ReadFrom(&io.LimitedReader{R: &counter{r: lf, n: &t.done}, N: size}) // size lets it pipeline
				return err
			}
			rf, err := m.sc.Open(srcPath)
			if err != nil {
				return err
			}
			lf, err := os.Create(dstPath)
			if err != nil {
				rf.Close()
				return err
			}
			closers = []io.Closer{rf, lf}
			// *sftp.File.WriteTo pipelines reads.
			_, err = rf.WriteTo(&counterW{w: lf, n: &t.done})
			return err
		}
		t.err = open()
		for _, c := range closers {
			if err := c.Close(); err != nil && t.err == nil {
				t.err = err
			}
		}
		t.finished = time.Since(t.start)
		return doneMsg{t}
	}
}

type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type counterW struct {
	w io.Writer
	n *atomic.Int64
}

func (c *counterW) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd { return tick() }

// paneAt maps a screen column to a pane index.
func (m *model) paneAt(x int) int {
	if x < m.w/2 {
		return 0
	}
	return 1
}

func (m *model) listHeight() int { return max(1, m.h-4-len(m.transfers)) }

// rowAt maps a screen row to an entry index in pane p, or -1.
func (m *model) rowAt(p *pane, y int) int {
	i := p.top + y - 2 // title row + header row
	if y < 2 || i >= len(p.entries) {
		return -1
	}
	return i
}

func (m *model) open(p *pane) {
	if len(p.entries) == 0 {
		return
	}
	if e := p.entries[p.sel]; e.IsDir() {
		p.dir = p.join(e.Name())
		m.load(p)
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	p := m.panes[m.active]
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		return m, tick()
	case doneMsg:
		m.load(m.panes[0])
		m.load(m.panes[1])
	case tea.PasteMsg:
		// Terminals paste dropped files as (possibly quoted) paths.
		var cmds []tea.Cmd
		for _, f := range strings.Fields(msg.Content) {
			f = strings.Trim(f, `'"`)
			f = strings.TrimPrefix(f, "file://")
			if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
				cmds = append(cmds, m.copyFile(-1, f, fi.Size()))
			}
		}
		m.status = fmt.Sprintf("drop: %d file(s) queued for upload", len(cmds))
		return m, tea.Batch(cmds...)
	case tea.MouseClickMsg:
		m.active = m.paneAt(msg.X)
		p = m.panes[m.active]
		if i := m.rowAt(p, msg.Y); i >= 0 {
			p.sel = i
			if e := p.entries[i]; !e.IsDir() {
				m.drag = &dragState{from: m.active, name: e.Name()}
			}
		}
	case tea.MouseReleaseMsg:
		d := m.drag
		m.drag, m.status = nil, ""
		if d != nil && m.paneAt(msg.X) != d.from {
			src := m.panes[d.from]
			for _, e := range src.entries {
				if e.Name() == d.name {
					return m, m.copyFile(d.from, src.join(e.Name()), e.Size())
				}
			}
		}
	case tea.MouseWheelMsg:
		wp := m.panes[m.paneAt(msg.X)]
		if msg.Button == tea.MouseWheelDown {
			wp.top = min(wp.top+3, max(0, len(wp.entries)-m.listHeight()))
		} else {
			wp.top = max(0, wp.top-3)
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.active = 1 - m.active
		case "up", "k":
			p.sel = max(0, p.sel-1)
		case "down", "j":
			p.sel = min(len(p.entries)-1, p.sel+1)
		case "enter":
			m.open(p)
		case "backspace":
			if p.remote {
				p.dir = path.Dir(p.dir)
			} else {
				p.dir = filepath.Dir(p.dir)
			}
			m.load(p)
		case "c":
			if len(p.entries) > 0 && !p.entries[p.sel].IsDir() {
				e := p.entries[p.sel]
				return m, m.copyFile(m.active, p.join(e.Name()), e.Size())
			}
		}
		// keep selection visible
		lh := m.listHeight()
		if p.sel < p.top {
			p.top = p.sel
		} else if p.sel >= p.top+lh {
			p.top = p.sel - lh + 1
		}
	}
	return m, nil
}

func (m *model) View() tea.View {
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion}
	if m.w == 0 {
		return v
	}
	half := m.w / 2
	cols := [2][]string{}
	for i, p := range m.panes {
		title := " local: " + p.dir
		if p.remote {
			title = " remote: " + p.dir
		}
		if i == m.active {
			title = "\x1b[7m" + pad(title, half-1) + "\x1b[0m"
		} else {
			title = pad(title, half-1)
		}
		lines := []string{title, pad(fmt.Sprintf(" %-*s %10s", half-14, "name", "size"), half-1)}
		for j := p.top; j < len(p.entries) && j < p.top+m.listHeight(); j++ {
			e := p.entries[j]
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			l := pad(fmt.Sprintf(" %-*s %10s", half-14, trunc(name, half-14), human(e.Size())), half-1)
			if j == p.sel {
				l = "\x1b[44m" + l + "\x1b[0m"
			}
			lines = append(lines, l)
		}
		cols[i] = lines
	}
	var b strings.Builder
	for r := 0; r < m.listHeight()+2; r++ {
		for i := range 2 {
			if r < len(cols[i]) {
				b.WriteString(cols[i][r])
			} else {
				b.WriteString(strings.Repeat(" ", half-1))
			}
			b.WriteString("│")
		}
		b.WriteString("\n")
	}
	for _, t := range m.transfers {
		d := t.done.Load()
		el := time.Since(t.start)
		state := fmt.Sprintf("%3d%%", 100*d/max(1, t.total))
		if t.finished > 0 {
			el, state = t.finished, "done"
		}
		if t.err != nil {
			state = "ERR " + t.err.Error()
		}
		fmt.Fprintf(&b, " %-30s %8s/%-8s %6.1f MB/s %s\n", trunc(t.name, 30), human(d), human(t.total),
			float64(d)/1e6/max(el.Seconds(), 0.001), state)
	}
	if m.drag != nil {
		m.status = "dragging " + m.drag.name + " — release on the other pane to copy"
	}
	b.WriteString(" " + m.status)
	v.SetContent(b.String())
	return v
}

func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return trunc(s, n)
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprint(n)
}

func main() {
	client, err := sshx.Dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()
	sc, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true), sftp.UseConcurrentReads(true))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer sc.Close()
	cwd, _ := os.Getwd()
	home, _ := sc.Getwd()
	m := &model{sc: sc, panes: [2]*pane{{dir: cwd}, {remote: true, dir: home}}}
	m.load(m.panes[0])
	m.load(m.panes[1])
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
