package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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

	"github.com/kannan983/pangea/internal/fsx"
	"github.com/kannan983/pangea/internal/store"
)

// fpane is one side of an SFTP tab: this computer or any server.
type fpane struct {
	fs      fsx.FS
	remote  bool
	label   string       // "Local" or the host's label
	home    string       // shown as ~
	conn    *ssh.Client  // remote panes
	sc      *sftp.Client // remote panes
	hostID  string
	dir     string
	entries []fs.FileInfo
	sel     int
	top     int
}

type transfer struct {
	name, from, to     string
	fromLabel, toLabel string // "Local" or host labels, for the history
	fromDir, toDir     string // folders as shown (with ~)
	toPane             int
	toFS               fsx.FS // to jump there from the history, if the pane still shows it
	fromFS             fsx.FS
	folder             bool
	total              atomic.Int64 // bytes; filled in by the worker for folders
	pr                 fsx.Progress
	start              time.Time
	finished           time.Duration
	err                error
	cancel             context.CancelFunc
}

func (t *transfer) running() bool { return t.finished == 0 }

type sftpView struct {
	panes     [2]*fpane
	active    int
	transfers []*transfer // this tab's history, newest last
	ticking   bool
	drag      *fs.FileInfo
	dragFrom  int
	edit      *pathEdit // path bar being typed into
	hist      bool      // history panel open
	histSel   int
}

type pathEdit struct {
	pane  int
	input textinput.Model
}

type sftpTickMsg struct{ v *sftpView }
type transferDoneMsg struct {
	v    *sftpView
	t    *transfer
	to   string // destination actually used (keep both renames)
	err  error
	took time.Duration
}
type opDoneMsg struct {
	v    *sftpView
	what string
	err  error
}

func (p *fpane) cur() fs.FileInfo {
	if p.sel < len(p.entries) {
		return p.entries[p.sel]
	}
	return nil
}

// ---- opening ----

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
		c := term.pane().sess.Client
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
			c := o.panes[0].sess.Client
			return func() tea.Msg { return dialedMsg{t: t, c: c} }
		}
	}
	return m.dial(t)
}

func (m *Model) startSFTP(t *tab, c *ssh.Client) tea.Cmd {
	local := t.host.LocalDir
	if local == "" {
		local = m.st.Settings.LocalDir
	}
	lp, warn1 := localPane(local)
	rp, warn2, err := remotePane(c, t.host)
	if err != nil {
		t.state, t.err = stFailed, err
		m.release(c)
		return nil
	}
	t.sftp, t.state = &sftpView{panes: [2]*fpane{lp, rp}}, stReady
	slog.Info("sftp open", "host", t.host.Label, "local", lp.dir, "remote", rp.dir)
	if w := strings.Trim(warn1+"; "+warn2, "; "); w != "" {
		m.fail(errors.New(w))
	}
	return nil
}

// localPane opens this computer at dir (or home; warn says why).
func localPane(dir string) (*fpane, string) {
	home, _ := os.UserHomeDir()
	p := &fpane{fs: fsx.Local{}, label: "Local", home: home, dir: home}
	warn := ""
	if dir != "" {
		p.dir = store.ExpandHome(dir)
		if err := p.load(); err != nil {
			warn, p.dir = "local folder "+dir+": "+err.Error(), home
		}
	}
	if err := p.load(); err != nil {
		warn = err.Error()
	}
	return p, warn
}

// remotePane opens an SFTP session on c at h's start folder (or home).
func remotePane(c *ssh.Client, h store.Host) (*fpane, string, error) {
	sc, err := sftp.NewClient(c, sftp.UseConcurrentWrites(true), sftp.UseConcurrentReads(true))
	if err != nil {
		return nil, "", err
	}
	home, err := sc.Getwd()
	if err != nil {
		sc.Close()
		return nil, "", err
	}
	p := &fpane{fs: fsx.Remote{C: sc}, remote: true, label: h.Label, home: home, dir: home,
		conn: c, sc: sc, hostID: h.ID}
	warn := ""
	if h.RemoteDir != "" {
		p.dir = expandTilde(h.RemoteDir, home)
		if err := p.load(); err != nil {
			warn, p.dir = "remote folder "+h.RemoteDir+": "+err.Error(), home
		}
	}
	if err := p.load(); err != nil {
		sc.Close()
		return nil, "", err
	}
	return p, warn, nil
}

// expandTilde resolves ~ against a remote home.
func expandTilde(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return path.Join(home, p[1:])
	}
	return p
}

// close ends a remote pane's SFTP session (not the SSH connection).
func (p *fpane) close() {
	if p.sc != nil {
		p.sc.Close()
	}
}

func (v *sftpView) fsOf(p *fpane) fsx.FS { return p.fs }

func (v *sftpView) load(p *fpane) error { return p.load() }

func (p *fpane) load() error {
	ents, err := p.fs.ReadDir(p.dir)
	if err != nil {
		return err
	}
	// Names a server sends are shown but never trusted as paths.
	ents = slicesFilter(ents, func(e fs.FileInfo) bool { return fsx.SafeName(e.Name()) })
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return strings.ToLower(ents[i].Name()) < strings.ToLower(ents[j].Name())
	})
	p.entries = ents
	p.sel = max(0, min(p.sel, len(ents)-1))
	return nil
}

func slicesFilter[T any](s []T, keep func(T) bool) []T {
	out := s[:0]
	for _, x := range s {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

func (m *Model) reload(v *sftpView) {
	for _, p := range v.panes {
		if err := v.load(p); err != nil {
			m.fail(err)
		}
	}
}

// ---- transfers ----

func (v *sftpView) tick() tea.Cmd {
	if v.ticking {
		return nil
	}
	v.ticking = true
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return sftpTickMsg{v} })
}

func (m *Model) sftpTick(v *sftpView) tea.Cmd {
	v.ticking = false
	for _, t := range v.transfers {
		if t.running() {
			return v.tick()
		}
	}
	return nil
}

// transferDone records a finished transfer. Only the UI loop writes these
// fields, so the view never races the worker.
func (m *Model) transferDone(msg transferDoneMsg) {
	t := msg.t
	t.to, t.err, t.finished = msg.to, msg.err, max(msg.took, time.Nanosecond)
	switch {
	case errors.Is(t.err, context.Canceled):
		m.info(t.name + ": canceled")
	case t.err != nil:
		m.fail(fmt.Errorf("%s: %w", t.name, t.err))
	case t.pr.Skipped.Load() > 0:
		m.info(fmt.Sprintf("%s: done, %d link(s)/special file(s) skipped", t.name, t.pr.Skipped.Load()))
	}
	slog.Info("transfer done", "name", t.name, "from", t.fromLabel+":"+t.from, "to", t.toLabel+":"+t.to,
		"bytes", t.pr.Bytes.Load(), "files", t.pr.Files.Load(), "took", t.finished, "err", t.err)
	m.reload(msg.v)
}

// copyTo copies srcPath from side `from` (0 local, 1 remote) into the
// other pane's folder, asking first if the name is taken there.
func (m *Model) copyTo(v *sftpView, from int, srcPath string) tea.Cmd {
	sp := v.panes[from]
	return m.copyInto(v, sp.fs, sp.label, sp.home, srcPath, 1-from)
}

// copyInto copies srcPath (on sf) into pane `to`'s folder, asking first if
// the name is taken there. Works for any pair: local, server, server.
func (m *Model) copyInto(v *sftpView, sf fsx.FS, srcLabel, srcHome, srcPath string, to int) tea.Cmd {
	dp := v.panes[to]
	df := dp.fs
	name := sf.Base(srcPath)
	if !fsx.SafeName(name) {
		m.fail(fmt.Errorf("%q can't be copied", name))
		return nil
	}
	dst := df.Join(dp.dir, name)
	if _, err := df.Lstat(dst); err == nil {
		return m.askChoice("File exists", fmt.Sprintf("%s already exists on %s.\nFolders are merged when overwriting.", name, dp.label),
			[]choice{{"o", "overwrite"}, {"k", "keep both"}, {"s", "skip"}}, func(key string) tea.Cmd {
				switch key {
				case "o":
					return m.runCopy(v, sf, srcLabel, srcHome, srcPath, to, dst, fsx.Overwrite)
				case "k":
					return m.runCopy(v, sf, srcLabel, srcHome, srcPath, to, dst, fsx.KeepBoth)
				}
				return nil
			})
	}
	return m.runCopy(v, sf, srcLabel, srcHome, srcPath, to, dst, fsx.Overwrite)
}

func (m *Model) runCopy(v *sftpView, sf fsx.FS, srcLabel, srcHome, src string, to int, dst string, c fsx.Conflict) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	dp := v.panes[to]
	df := dp.fs
	t := &transfer{name: sf.Base(src), from: src, to: dst, fromLabel: srcLabel, toLabel: dp.label,
		fromDir: tildify(sf.Dir(src), srcHome), toDir: tildify(dp.dir, dp.home),
		toPane: to, toFS: df, fromFS: sf, start: time.Now(), cancel: cancel}
	if fi, err := sf.Lstat(src); err == nil {
		t.folder = fi.IsDir()
		t.total.Store(fi.Size())
	}
	v.transfers = append(v.transfers, t)
	slog.Info("transfer start", "name", t.name, "from", srcLabel+":"+src, "to", dp.label+":"+dst, "folder", t.folder)
	run := func() tea.Msg {
		if t.folder {
			if n, _, err := fsx.Size(sf, src); err == nil {
				t.total.Store(n)
			}
		}
		final, err := fsx.Copy(ctx, sf, src, df, dst, c, &t.pr)
		cancel()
		return transferDoneMsg{v, t, final, err, time.Since(t.start)}
	}
	return tea.Batch(run, v.tick())
}

// dropTarget is where files dropped from the desktop go: the remote pane,
// or the active one when both (or neither) are remote.
func dropTarget(v *sftpView) int {
	if v.panes[0].remote != v.panes[1].remote {
		if v.panes[0].remote {
			return 0
		}
		return 1
	}
	return v.active
}

// cancelNewest stops the most recently started running transfer.
func (m *Model) cancelNewest(v *sftpView) {
	for i := len(v.transfers) - 1; i >= 0; i-- {
		if t := v.transfers[i]; t.running() {
			t.cancel()
			return
		}
	}
	m.info("no transfer running")
}

// ---- file operations ----

func (m *Model) fileOp(v *sftpView, what string, fn func() error) tea.Cmd {
	return func() tea.Msg {
		err := fn()
		slog.Info("file op", "op", what, "err", err)
		return opDoneMsg{v, what, err}
	}
}

func (m *Model) opDone(msg opDoneMsg) {
	if msg.err != nil {
		m.fail(fmt.Errorf("%s: %w", msg.what, msg.err))
	} else {
		m.info(msg.what)
	}
	m.reload(msg.v)
}

func (m *Model) renamePrompt(v *sftpView, p *fpane) tea.Cmd {
	e := p.cur()
	if e == nil {
		return nil
	}
	f, old := v.fsOf(p), e.Name()
	return m.askText("Rename", "New name for "+old, old, func(name string) tea.Cmd {
		name = strings.TrimSpace(name)
		if name == old {
			return nil
		}
		if err := fsx.ValidNewName(name); err != nil {
			m.fail(err)
			return nil
		}
		return m.fileOp(v, "Renamed "+old+" to "+name, func() error {
			return f.Rename(f.Join(p.dir, old), f.Join(p.dir, name))
		})
	})
}

func (m *Model) mkdirPrompt(v *sftpView, p *fpane) tea.Cmd {
	f := v.fsOf(p)
	return m.askText("New folder", "Folder name in "+tildify(p.dir, p.home), "", func(name string) tea.Cmd {
		name = strings.TrimSpace(name)
		if err := fsx.ValidNewName(name); err != nil {
			m.fail(err)
			return nil
		}
		return m.fileOp(v, "Created folder "+name, func() error { return f.Mkdir(f.Join(p.dir, name)) })
	})
}

func (m *Model) deletePrompt(v *sftpView, p *fpane) tea.Cmd {
	e := p.cur()
	if e == nil {
		return nil
	}
	f, name := v.fsOf(p), e.Name()
	full := f.Join(p.dir, name)
	what := "Delete file " + name + "?"
	if e.IsDir() {
		what = "Delete folder " + name + " and everything in it?"
	}
	where := "on this computer"
	if p.remote {
		where = "on " + p.label
	}
	return m.askConfirm("Delete", what+"\n"+full+" "+where+"\nThis can't be undone.", func() tea.Cmd {
		return m.fileOp(v, "Deleted "+name, func() error { return fsx.RemoveAll(f, full) })
	})
}

func (m *Model) chmodPrompt(v *sftpView, p *fpane) tea.Cmd {
	e := p.cur()
	if e == nil {
		return nil
	}
	f, name := v.fsOf(p), e.Name()
	return m.askText("Permissions", fmt.Sprintf("Octal permissions for %s (now %s, %s)", name, fsx.Octal(e.Mode()), e.Mode().Perm()),
		fsx.Octal(e.Mode()), func(s string) tea.Cmd {
			mode, err := fsx.ParseMode(s)
			if err != nil {
				m.fail(err)
				return nil
			}
			return m.fileOp(v, fmt.Sprintf("Set %s to %s", name, fsx.Octal(mode)), func() error {
				return f.Chmod(f.Join(p.dir, name), mode)
			})
		})
}

func (v *sftpView) side(p *fpane) int {
	if p.remote {
		return 1
	}
	return 0
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
			typed = expandTilde(typed, p.home)
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
	ents, err := v.fsOf(p).ReadDir(dir)
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
			if en.IsDir() && fsx.SafeName(n) && ok && !(strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".")) {
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

// ---- update ----

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
	if v.hist {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			return m.histKey(v, k)
		}
		return nil
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
		to := dropTarget(v)
		for _, f := range splitDropped(msg.Content) {
			if _, err := os.Lstat(f); err == nil {
				home, _ := os.UserHomeDir()
				cmds = append(cmds, m.copyInto(v, fsx.Local{}, "Local", home, f, to))
			} else {
				skipped++
			}
		}
		if len(cmds) > 0 {
			m.info(fmt.Sprintf("Copying %d dropped item(s) to %s:%s", len(cmds), v.panes[to].label, v.panes[to].dir))
		} else if skipped > 0 {
			m.fail(errors.New("dropped text is not a local file or folder path"))
		}
		return tea.Batch(cmds...)
	case tea.MouseReleaseMsg:
		if v.drag != nil {
			fi, from := *v.drag, v.dragFrom
			v.drag = nil
			if paneAt(msg.X) != from {
				return m.copyTo(v, from, v.panes[from].fs.Join(v.panes[from].dir, fi.Name()))
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
		case "home", "g":
			p.sel = 0
		case "end", "G":
			p.sel = max(0, len(p.entries)-1)
		case "enter", "right", "l":
			m.sftpOpen(v, p)
		case "backspace", "left", "h":
			old := p.dir
			p.dir = v.fsOf(p).Dir(p.dir)
			p.sel, p.top = 0, 0
			if err := v.load(p); err != nil {
				p.dir = old
				m.fail(err)
			}
		case "r":
			m.reload(v)
		case "c":
			if e := p.cur(); e != nil {
				return m.copyTo(v, v.active, v.fsOf(p).Join(p.dir, e.Name()))
			}
		case "e", "f2":
			return m.renamePrompt(v, p)
		case "n":
			return m.mkdirPrompt(v, p)
		case "d", "delete":
			return m.deletePrompt(v, p)
		case "p":
			return m.chmodPrompt(v, p)
		case "x":
			m.cancelNewest(v)
		case "o":
			return m.pickSource(tb, v, v.active)
		case "t":
			v.hist, v.histSel = true, 0
		case "C":
			clearFinished(v)
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
	e := p.cur()
	if e == nil || !e.IsDir() {
		return
	}
	old := p.dir
	p.dir = v.fsOf(p).Join(p.dir, e.Name())
	p.sel, p.top = 0, 0
	if err := v.load(p); err != nil {
		p.dir = old
		m.fail(err)
	}
}

// ---- view ----

const maxShownTransfers = 5

// listH is the rows left for file entries: body minus pane borders (2),
// path and header rows (2) and the transfers block.
func (m *Model) listH(v *sftpView) int {
	tr := 0
	if len(v.transfers) > 0 || v.drag != nil {
		tr = 1 + min(len(runningTransfers(v)), maxShownTransfers)
		if len(v.transfers) > len(runningTransfers(v)) {
			tr++ // summary line
		}
		if v.drag != nil {
			tr++
		}
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
	if v.hist {
		return m.viewHistory(v)
	}
	half := m.w / 2
	iw := half - 2 // inside a pane's border
	cols := [2][]string{}
	for i, p := range v.panes {
		bst := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
		if i == v.active {
			bst = lipgloss.NewStyle().Foreground(accent).Bold(true)
		}
		name := " " + trunc(p.label, iw-12) + " "
		lines := []string{bst.Render("╭─" + name + strings.Repeat("─", max(0, iw-1-lipgloss.Width(name))) + "╮")}
		var pathLine string
		if v.edit != nil && v.edit.pane == i {
			pathLine = stAccent.Render(" / ") + v.edit.input.View()
		} else {
			pathLine = " " + stBold.Render(truncLeft(tildify(p.dir, p.home), iw-2))
		}
		nameW := max(8, iw-19)
		lines = append(lines, pathLine, stDim.Render(fmt.Sprintf(" %-*s %5s %10s", nameW, "Name", "Perm", "Size")))
		for j := p.top; j < len(p.entries) && j < p.top+m.listH(v); j++ {
			e := p.entries[j]
			name := e.Name()
			size := human(e.Size())
			if e.IsDir() {
				name, size = name+"/", ""
			}
			l := fit(fmt.Sprintf(" %-*s %5s %10s", nameW, trunc(name, nameW), fsx.Octal(e.Mode()), size), iw)
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
				} else if !double {
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
	out = append(out, stDim.Render(" Transfers   t history · C clear finished · x cancel newest"))
	running := runningTransfers(v)
	if len(running) > maxShownTransfers {
		running = running[len(running)-maxShownTransfers:]
	}
	for _, t := range running {
		out = append(out, transferLine(t, m.w))
	}
	if s := transferSummary(v); s != "" {
		out = append(out, s)
	}
	if v.drag != nil {
		out = append(out, stAccent.Render(" dragging "+(*v.drag).Name()+" — release over the other pane to copy"))
	}
	return out
}

// transferLine renders " name  ███████░░░  62%  3.1M / 5.0M  48.2 MB/s  ETA 0:04".
func transferLine(t *transfer, w int) string {
	done, total := t.pr.Bytes.Load(), t.total.Load()
	el := time.Since(t.start)
	if !t.running() {
		el = t.finished
	}
	pct := 100
	if total > 0 {
		pct = int(min(100, done*100/total))
	}
	speed := float64(done) / max(el.Seconds(), 0.001)
	color, state := accent, fmt.Sprintf("%3d%%", pct)
	switch {
	case errors.Is(t.err, context.Canceled):
		color, state = dim, "canceled"
	case t.err != nil:
		color, state = bad, "failed"
	case !t.running():
		color, state = good, "done ✓"
	}
	extra := ""
	if t.running() && speed > 0 && total > done {
		extra = "  ETA " + fmtDur(time.Duration(float64(total-done)/speed*float64(time.Second)))
	}
	if t.folder {
		extra = fmt.Sprintf("  %d files", t.pr.Files.Load()) + extra
	}
	barW := max(10, min(30, w/5))
	filled := barW * pct / 100
	st := lipgloss.NewStyle().Foreground(color)
	bar := st.Render(strings.Repeat("█", filled)) + stDim.Render(strings.Repeat("░", barW-filled))
	return fmt.Sprintf(" %-24s %s %s  %8s / %-8s %7.1f MB/s%s", trunc(t.name, 24), bar, st.Render(fmt.Sprintf("%-8s", state)),
		human(done), human(total), speed/1e6, extra)
}

func fmtDur(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
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
