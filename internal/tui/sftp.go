package tui

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/kannan983/pangea/internal/store"
)

type fpane struct {
	remote  bool
	dir     string
	entries []fs.FileInfo
	sel     int
	top     int
}

func (p *fpane) join(name string) string {
	if p.remote {
		return path.Join(p.dir, name)
	}
	return filepath.Join(p.dir, name)
}

type transfer struct {
	name     string
	total    int64
	done     atomic.Int64
	start    time.Time
	finished time.Duration
	err      error
}

type sftpView struct {
	sc        *sftp.Client
	conn      *ssh.Client
	label     string
	panes     [2]*fpane
	active    int
	transfers []*transfer
	ticking   bool
	drag      *fs.FileInfo
	dragFrom  int
	edit      *pathEdit // path bar being typed into
	homes     [2]string // local and remote home, shown as ~
}

type pathEdit struct {
	pane  int
	input textinput.Model
}

type sftpTickMsg struct{ v *sftpView }
type transferDoneMsg struct {
	v *sftpView
	t *transfer
}

// openSFTP opens an SFTP tab for h.
func (m *Model) openSFTP(h store.Host) tea.Cmd {
	t := &tab{kind: tabSFTP, host: h}
	m.addTab(t)
	return m.openSFTPIn(t)
}

// openSFTPFor opens SFTP next to terminal tab term, on its connection.
func (m *Model) openSFTPFor(term *tab) tea.Cmd {
	t := &tab{kind: tabSFTP, host: term.host}
	m.addTab(t)
	if term.state == stReady {
		t.state = stConnecting
		c := term.sess.Client
		return func() tea.Msg { return dialedMsg{t: t, c: c} }
	}
	return m.openSFTPIn(t)
}

// openSFTPIn connects t, reusing a live session's connection to the same
// host when there is one (no second login).
func (m *Model) openSFTPIn(t *tab) tea.Cmd {
	t.state, t.err = stConnecting, nil
	for _, o := range m.tabs {
		if o.kind == tabTerm && o.state == stReady && o.host.ID != "" && o.host.ID == t.host.ID {
			c := o.sess.Client
			return func() tea.Msg { return dialedMsg{t: t, c: c} }
		}
	}
	return m.dial(t)
}

func (m *Model) startSFTP(t *tab, c *ssh.Client) tea.Cmd {
	sc, err := sftp.NewClient(c, sftp.UseConcurrentWrites(true), sftp.UseConcurrentReads(true))
	if err != nil {
		t.state, t.err = stFailed, err
		m.release(c)
		return nil
	}
	local := t.host.LocalDir
	if local == "" {
		local = m.st.Settings.LocalDir
	}
	v, warn, err := newSFTPView(sc, c, t.host.Label, local, t.host.RemoteDir)
	if err != nil {
		sc.Close()
		t.state, t.err = stFailed, err
		m.release(c)
		return nil
	}
	t.sftp, t.state = v, stReady
	if warn != "" {
		m.fail(errors.New(warn))
	}
	return nil
}

// newSFTPView opens both panes at their start folders, falling back to the
// home folders (warn says why) when a configured folder can't be opened.
func newSFTPView(sc *sftp.Client, conn *ssh.Client, label, localDir, remoteDir string) (*sftpView, string, error) {
	home, _ := os.UserHomeDir()
	rhome, err := sc.Getwd()
	if err != nil {
		return nil, "", err
	}
	v := &sftpView{sc: sc, conn: conn, label: label, homes: [2]string{home, rhome},
		panes: [2]*fpane{{dir: home}, {remote: true, dir: rhome}}}
	var warns []string
	if localDir != "" {
		v.panes[0].dir = store.ExpandHome(localDir)
		if err := v.load(v.panes[0]); err != nil {
			warns = append(warns, "local folder "+localDir+": "+err.Error())
			v.panes[0].dir = home
		}
	}
	if remoteDir != "" {
		v.panes[1].dir = v.remoteExpand(remoteDir, rhome)
		if err := v.load(v.panes[1]); err != nil {
			warns = append(warns, "remote folder "+remoteDir+": "+err.Error())
			v.panes[1].dir = rhome
		}
	}
	if err := v.load(v.panes[0]); err != nil {
		return nil, "", err
	}
	return v, strings.Join(warns, "; "), v.load(v.panes[1])
}

// remoteExpand resolves ~ against the remote home.
func (v *sftpView) remoteExpand(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return path.Join(home, p[1:])
	}
	return p
}

// ---- path bar ----

func (v *sftpView) startEdit(i, width int) tea.Cmd {
	p := v.panes[i]
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetWidth(max(10, width))
	val := p.dir
	if !strings.HasSuffix(val, v.sep(p)) {
		val += v.sep(p)
	}
	ti.SetValue(val)
	ti.CursorEnd()
	v.active, v.edit = i, &pathEdit{pane: i, input: ti}
	return v.edit.input.Focus()
}

func (v *sftpView) sep(p *fpane) string {
	if p.remote {
		return "/"
	}
	return string(filepath.Separator)
}

// resolve expands ~ and makes typed paths absolute against the pane dir.
func (v *sftpView) resolve(p *fpane, typed string) (string, error) {
	if p.remote {
		if strings.HasPrefix(typed, "~") {
			home, err := v.sc.Getwd()
			if err != nil {
				return "", err
			}
			typed = v.remoteExpand(typed, home)
		}
		if !path.IsAbs(typed) {
			typed = path.Join(p.dir, typed)
		}
		return path.Clean(typed), nil
	}
	typed = store.ExpandHome(typed)
	if !filepath.IsAbs(typed) {
		typed = filepath.Join(p.dir, typed)
	}
	return filepath.Clean(typed), nil
}

func (v *sftpView) readDir(p *fpane, dir string) ([]fs.FileInfo, error) {
	if p.remote {
		return v.sc.ReadDir(dir)
	}
	des, err := os.ReadDir(dir)
	var out []fs.FileInfo
	for _, d := range des {
		if fi, e := d.Info(); e == nil {
			out = append(out, fi)
		}
	}
	return out, err
}

// complete fills in folder names after the last separator, like a shell:
// one match completes fully, several complete their common prefix.
func (m *Model) complete(v *sftpView) {
	e, p := v.edit, v.panes[v.edit.pane]
	val := e.input.Value()
	cut := strings.LastIndex(val, v.sep(p)) + 1
	if p.remote {
		cut = strings.LastIndex(val, "/") + 1
	}
	dirPart, prefix := val[:cut], val[cut:]
	dir, err := v.resolve(p, dirPart)
	if dirPart == "" {
		dir = p.dir
	}
	if err != nil {
		m.fail(err)
		return
	}
	ents, err := v.readDir(p, dir)
	if err != nil {
		m.fail(err)
		return
	}
	var names []string
	for _, fold := range []bool{false, true} { // exact case first, then any case
		for _, en := range ents {
			n := en.Name()
			ok := strings.HasPrefix(n, prefix)
			if fold {
				ok = strings.HasPrefix(strings.ToLower(n), strings.ToLower(prefix))
			}
			if en.IsDir() && ok && !(strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".")) {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			break
		}
	}
	switch len(names) {
	case 0:
		m.fail(fmt.Errorf("no folder starting with %q", prefix))
	case 1:
		e.input.SetValue(dirPart + names[0] + v.sep(p))
	default:
		sort.Strings(names)
		cp := names[0]
		for _, n := range names[1:] {
			for !strings.HasPrefix(n, cp) {
				cp = cp[:len(cp)-1]
			}
		}
		if len(cp) > len(prefix) {
			e.input.SetValue(dirPart + cp)
		}
		shown := names
		if len(shown) > 8 {
			shown = append(shown[:8:8], fmt.Sprintf("… %d more", len(names)-8))
		}
		m.info(strings.Join(shown, "   "))
	}
	e.input.CursorEnd()
}

// goTo opens the typed folder in the edited pane.
func (m *Model) goTo(v *sftpView) {
	p := v.panes[v.edit.pane]
	dir, err := v.resolve(p, strings.TrimSpace(v.edit.input.Value()))
	if err != nil {
		m.fail(err)
		return
	}
	old := p.dir
	p.dir, p.sel, p.top = dir, 0, 0
	if err := v.load(p); err != nil {
		p.dir = old
		m.fail(err)
		return
	}
	v.edit = nil
}

func (m *Model) editKey(v *sftpView, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		v.edit = nil
		return nil
	case "enter":
		m.goTo(v)
		return nil
	case "tab":
		m.complete(v)
		return nil
	}
	var cmd tea.Cmd
	v.edit.input, cmd = v.edit.input.Update(k)
	return cmd
}

func (v *sftpView) load(p *fpane) error {
	var ents []fs.FileInfo
	var err error
	if p.remote {
		ents, err = v.sc.ReadDir(p.dir)
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
		return err
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return strings.ToLower(ents[i].Name()) < strings.ToLower(ents[j].Name())
	})
	p.entries = ents
	p.sel = min(p.sel, max(0, len(ents)-1))
	return nil
}

func (v *sftpView) tick() tea.Cmd {
	if v.ticking {
		return nil
	}
	v.ticking = true
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return sftpTickMsg{v} })
}

func (v *sftpView) exists(remote bool, p string) bool {
	var err error
	if remote {
		_, err = v.sc.Stat(p)
	} else {
		_, err = os.Stat(p)
	}
	return err == nil
}

// copyFile copies srcPath into the directory of the other side. upload is
// local→remote. Existing destination files are never overwritten.
func (m *Model) copyFile(v *sftpView, upload bool, srcPath string) tea.Cmd {
	dst := v.panes[0]
	if upload {
		dst = v.panes[1]
	}
	name := filepath.Base(srcPath)
	if !upload {
		name = path.Base(srcPath)
	}
	dstPath := dst.join(name)
	if v.exists(dst.remote, dstPath) {
		m.fail(fmt.Errorf("%s already exists in %s — not overwriting", name, dst.dir))
		return nil
	}
	var fi fs.FileInfo
	var err error
	if upload {
		fi, err = os.Stat(srcPath)
	} else {
		fi, err = v.sc.Stat(srcPath)
	}
	if err != nil {
		m.fail(err)
		return nil
	}
	if !fi.Mode().IsRegular() {
		m.fail(fmt.Errorf("%s: only regular files can be copied for now", name))
		return nil
	}
	t := &transfer{name: name, total: fi.Size(), start: time.Now()}
	v.transfers = append(v.transfers, t)
	run := func() tea.Msg {
		t.err = v.transfer(upload, srcPath, dstPath, t)
		t.finished = time.Since(t.start)
		return transferDoneMsg{v, t}
	}
	return tea.Batch(run, v.tick())
}

func (v *sftpView) transfer(upload bool, src, dst string, t *transfer) error {
	if upload {
		lf, err := os.Open(src)
		if err != nil {
			return err
		}
		defer lf.Close()
		rf, err := v.sc.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return err
		}
		// The size lets ReadFrom pipeline writes (about 3x faster).
		if _, err := rf.ReadFrom(&io.LimitedReader{R: &counter{lf, &t.done}, N: t.total}); err != nil {
			rf.Close()
			return err
		}
		return rf.Close()
	}
	rf, err := v.sc.Open(src)
	if err != nil {
		return err
	}
	defer rf.Close()
	lf, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := rf.WriteTo(&counterW{lf, &t.done}); err != nil {
		lf.Close()
		return err
	}
	return lf.Close()
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

// splitDropped parses what terminals paste when files are dropped on them:
// space separated paths with backslash-escaped spaces, quoted paths, one
// per line, or file:// URIs.
func splitDropped(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	esc := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		p := cur.String()
		if strings.HasPrefix(p, "file://") {
			if u, err := url.Parse(p); err == nil {
				p = u.Path
			}
		}
		out = append(out, p)
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote == 0:
			esc = true
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote == 0 && (r == ' ' || r == '\n' || r == '\r' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func (m *Model) sftpTick(v *sftpView) tea.Cmd {
	v.ticking = false
	for _, t := range v.transfers {
		if t.finished == 0 {
			return v.tick()
		}
	}
	return nil
}

func (m *Model) transferDone(msg transferDoneMsg) {
	if msg.t.err != nil {
		m.fail(fmt.Errorf("%s: %w", msg.t.name, msg.t.err))
	}
	for _, p := range msg.v.panes {
		if err := msg.v.load(p); err != nil {
			m.fail(err)
		}
	}
}

func (m *Model) updateSFTP(tb *tab, msg tea.Msg) tea.Cmd {
	v := tb.sftp
	p := v.panes[v.active]
	half := m.w / 2
	if v.edit != nil {
		switch msg := msg.(type) {
		case tea.KeyPressMsg:
			return m.editKey(v, msg)
		case tea.PasteMsg:
			var cmd tea.Cmd
			v.edit.input, cmd = v.edit.input.Update(msg)
			return cmd
		}
	}
	paneAt := func(x int) int {
		if x < half {
			return 0
		}
		return 1
	}
	switch msg := msg.(type) {
	case tea.PasteMsg:
		var cmds []tea.Cmd
		skipped := 0
		for _, f := range splitDropped(msg.Content) {
			if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
				cmds = append(cmds, m.copyFile(v, true, f))
			} else {
				skipped++
			}
		}
		if len(cmds) > 0 {
			m.info(fmt.Sprintf("Uploading %d dropped file(s) to %s", len(cmds), v.panes[1].dir))
		} else if skipped > 0 {
			m.fail(errors.New("dropped text is not a local file path (folders are not supported yet)"))
		}
		return tea.Batch(cmds...)
	case tea.MouseReleaseMsg:
		if v.drag != nil {
			fi, from := *v.drag, v.dragFrom
			v.drag = nil
			if paneAt(msg.X) != from {
				return m.copyFile(v, from == 0, v.panes[from].join(fi.Name()))
			}
		}
	case tea.MouseWheelMsg:
		wp := v.panes[paneAt(msg.X)]
		if msg.Button == tea.MouseWheelDown {
			wp.top = min(wp.top+3, max(0, len(wp.entries)-m.listH(v)))
		} else {
			wp.top = max(0, wp.top-3)
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "tab":
			v.active = 1 - v.active
		case "/", "ctrl+l":
			return v.startEdit(v.active, half-8)
		case "up", "k":
			p.sel = max(0, p.sel-1)
		case "down", "j":
			p.sel = max(0, min(len(p.entries)-1, p.sel+1))
		case "enter", "right", "l":
			m.sftpOpen(v, p)
		case "backspace", "left", "h":
			if p.remote {
				p.dir = path.Dir(p.dir)
			} else {
				p.dir = filepath.Dir(p.dir)
			}
			p.sel, p.top = 0, 0
			if err := v.load(p); err != nil {
				m.fail(err)
			}
		case "r":
			for _, p := range v.panes {
				if err := v.load(p); err != nil {
					m.fail(err)
				}
			}
		case "c":
			if len(p.entries) > 0 {
				return m.copyFile(v, !p.remote, p.join(p.entries[p.sel].Name()))
			}
		}
		lh := m.listH(v)
		if p.sel < p.top {
			p.top = p.sel
		} else if p.sel >= p.top+lh {
			p.top = p.sel - lh + 1
		}
	}
	return nil
}

func (m *Model) sftpOpen(v *sftpView, p *fpane) {
	if len(p.entries) == 0 || !p.entries[p.sel].IsDir() {
		return
	}
	p.dir = p.join(p.entries[p.sel].Name())
	p.sel, p.top = 0, 0
	if err := v.load(p); err != nil {
		m.fail(err)
	}
}

// listH is the rows left for file entries: body minus pane borders (2),
// path and header rows (2) and the transfers block.
func (m *Model) listH(v *sftpView) int {
	tr := 0
	if n := min(len(v.transfers), 5); n > 0 || v.drag != nil {
		tr = 1 + n + 1
	}
	return max(1, m.bodyH()-4-tr)
}

func (m *Model) viewSFTPTab(t *tab) []string {
	w, h := m.w, m.bodyH()
	switch t.state {
	case stConnecting:
		return center([]string{stBold.Render("Opening SFTP to " + t.host.Label), stDim.Render(t.host.Addr())}, w, h)
	case stFailed:
		return center([]string{lipgloss.NewStyle().Foreground(bad).Bold(true).Render("SFTP to " + t.host.Label + " failed"),
			"", trunc(t.err.Error(), w-4), "", stDim.Render("enter retry · alt+w close")}, w, h)
	}
	v := t.sftp
	half := m.w / 2
	iw := half - 2 // inside a pane's border
	cols := [2][]string{}
	for i, p := range v.panes {
		bst := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
		if i == v.active {
			bst = lipgloss.NewStyle().Foreground(accent).Bold(true)
		}
		name := " Local "
		if p.remote {
			name = " " + trunc(v.label, iw-6) + " "
		}
		lines := []string{bst.Render("╭─" + name + strings.Repeat("─", max(0, iw-1-lipgloss.Width(name))) + "╮")}
		var path string
		if v.edit != nil && v.edit.pane == i {
			path = stAccent.Render(" / ") + v.edit.input.View()
		} else {
			path = " " + stBold.Render(truncLeft(tildify(p.dir, v.homes[i]), iw-2))
		}
		nameW := max(8, iw-13)
		lines = append(lines, path, stDim.Render(fmt.Sprintf(" %-*s %10s", nameW, "Name", "Size")))
		for j := p.top; j < len(p.entries) && j < p.top+m.listH(v); j++ {
			e := p.entries[j]
			name := e.Name()
			size := human(e.Size())
			if e.IsDir() {
				name, size = name+"/", ""
			}
			l := fit(fmt.Sprintf(" %-*s %10s", nameW, trunc(name, nameW), size), iw)
			if e.IsDir() {
				l = stAccent.Render(l)
			}
			if j == p.sel {
				l = stSel.Render(l)
			}
			y := len(lines)
			lines = append(lines, l)
			i, j, e := i, j, e
			m.addZone(i*half+1, y, iw, 1, func(double bool) tea.Cmd {
				v.active = i
				v.panes[i].sel = j
				if double && e.IsDir() {
					m.sftpOpen(v, v.panes[i])
				} else if !e.IsDir() {
					v.drag, v.dragFrom = &e, i
				}
				return nil
			})
		}
		for len(lines) < m.listH(v)+3 {
			lines = append(lines, "")
		}
		bar := bst.Render("│")
		for r := 1; r < len(lines); r++ {
			lines[r] = bar + fit(lines[r], iw) + bar
		}
		lines = append(lines, bst.Render("╰"+strings.Repeat("─", iw)+"╯"))
		cols[i] = lines
		i := i
		m.addZone(i*half, 0, half, 2, func(double bool) tea.Cmd {
			v.active = i
			if double {
				return v.startEdit(i, iw-4)
			}
			return nil
		})
	}
	var out []string
	for r := range cols[0] {
		out = append(out, cols[0][r]+cols[1][r])
	}
	if len(v.transfers) == 0 && v.drag == nil {
		return out
	}
	out = append(out, stDim.Render(" Transfers"))
	ts := v.transfers
	if len(ts) > 5 {
		ts = ts[len(ts)-5:]
	}
	for _, t := range ts {
		d := t.done.Load()
		el := time.Since(t.start)
		state := fmt.Sprintf("%3d%%", 100*d/max(1, t.total))
		st := lipgloss.NewStyle()
		if t.finished > 0 {
			el, state, st = t.finished, "done", lipgloss.NewStyle().Foreground(good)
		}
		if t.err != nil {
			state, st = "failed: "+t.err.Error(), lipgloss.NewStyle().Foreground(bad)
		}
		out = append(out, fmt.Sprintf(" %-30s %8s / %-8s %7.1f MB/s  %s", trunc(t.name, 30), human(d), human(t.total),
			float64(d)/1e6/max(el.Seconds(), 0.001), st.Render(state)))
	}
	if v.drag != nil {
		out = append(out, stAccent.Render(" dragging "+(*v.drag).Name()+" — release over the other pane to copy"))
	}
	return out
}

// tildify shows dir under home as ~/...
func tildify(dir, home string) string {
	if home == "" || home == "/" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home); ok && (strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, `\`)) {
		return "~" + rest
	}
	return dir
}

// truncLeft keeps the end of s (the current folder of a long path).
func truncLeft(s string, n int) string {
	if r := []rune(s); len(r) > n && n > 1 {
		return "…" + string(r[len(r)-n+1:])
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
