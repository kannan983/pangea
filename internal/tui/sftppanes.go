package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/crypto/ssh"

	"github.com/kannan983/pangea/internal/sshc"
	"github.com/kannan983/pangea/internal/store"
)

// ---- switching what a pane shows ----

type paneReadyMsg struct {
	t      *tab
	v      *sftpView
	i      int
	p      *fpane
	warn   string
	host   store.Host
	typed  string      // password typed while dialing
	dialed *ssh.Client // connection opened just for this pane
	err    error
}

// pickSource lets the user choose what pane i shows: this computer, an
// open session (its login is reused), or any saved host.
func (m *Model) pickSource(t *tab, v *sftpView, i int) tea.Cmd {
	if busy(v, v.panes[i]) {
		m.fail(errors.New("transfers are running on this pane — wait, or cancel them with x"))
		return nil
	}
	items := []pickItem{{label: "Local", detail: "this computer",
		run: func() tea.Cmd { m.setPane(t, v, i, mustLocal(m.st.Settings.LocalDir)); return nil }}}
	open := map[string]bool{}
	for _, o := range m.tabs {
		if o.kind != tabTerm || o.state != stReady || len(o.panes) == 0 {
			continue
		}
		h, c := o.host, o.panes[0].sess.Client
		key := h.ID + "|" + h.Label
		if open[key] {
			continue
		}
		open[key] = true
		items = append(items, pickItem{label: "● " + h.Label, detail: "open session · " + userAddr(h),
			run: func() tea.Cmd { return m.connectPane(t, v, i, h, c) }})
	}
	hosts := append([]store.Host(nil), m.st.Hosts...)
	sort.SliceStable(hosts, func(a, b int) bool {
		if (hosts[a].Mode == m.mode) != (hosts[b].Mode == m.mode) {
			return hosts[a].Mode == m.mode // current mode first
		}
		return strings.ToLower(hosts[a].Label) < strings.ToLower(hosts[b].Label)
	})
	for _, h := range hosts {
		if open[h.ID+"|"+h.Label] {
			continue
		}
		items = append(items, pickItem{label: h.Label, detail: h.Mode + " · " + userAddr(h),
			run: func() tea.Cmd { return m.connectPane(t, v, i, h, nil) }})
	}
	side := "left"
	if i == 1 {
		side = "right"
	}
	return m.askPick("Open in the "+side+" pane", "Local, an open session, or any saved host", items)
}

func userAddr(h store.Host) string {
	if h.User != "" {
		return h.User + "@" + h.Addr()
	}
	return h.Addr()
}

func mustLocal(dir string) *fpane {
	p, _ := localPane(dir)
	return p
}

// connectPane opens h in pane i, on c if given, else on a connection
// already open to h, else by dialing (prompts appear as usual).
func (m *Model) connectPane(t *tab, v *sftpView, i int, h store.Host, c *ssh.Client) tea.Cmd {
	if c == nil {
		c = m.findConn(h)
	}
	m.info("Connecting " + h.Label + "…")
	pr := &tuiPrompter{m: m, t: t, h: &h}
	known, saved := m.knownHosts(), m.vault.Get(h.ID)
	return func() tea.Msg {
		msg := paneReadyMsg{t: t, v: v, i: i, host: h}
		if c == nil {
			c, msg.typed, msg.err = sshc.Dial(h, known, pr, saved)
			if msg.err != nil {
				return msg
			}
			msg.dialed = c
		}
		msg.p, msg.warn, msg.err = remotePane(c, h)
		if msg.err != nil && msg.dialed != nil {
			msg.dialed.Close()
		}
		return msg
	}
}

// findConn returns an open connection to h (from a session or an SFTP
// pane), so a second login isn't needed.
func (m *Model) findConn(h store.Host) *ssh.Client {
	if h.ID == "" {
		return nil
	}
	for _, o := range m.tabs {
		if o.host.ID == h.ID {
			for _, p := range o.panes {
				if !p.sess.Closed() {
					return p.sess.Client
				}
			}
		}
		if o.sftp != nil {
			for _, p := range o.sftp.panes {
				if p.hostID == h.ID && p.conn != nil {
					return p.conn
				}
			}
		}
	}
	return nil
}

func (m *Model) onPaneReady(msg paneReadyMsg) {
	switch {
	case errors.Is(msg.err, sshc.ErrCanceled):
		m.info("canceled")
		return
	case msg.err != nil:
		slog.Warn("pane connect failed", "host", msg.host.Label, "err", msg.err)
		m.fail(fmt.Errorf("%s: %w", msg.host.Label, msg.err))
		return
	case m.tabIndex(msg.t) < 0 || msg.t.sftp != msg.v: // tab closed meanwhile
		msg.p.close()
		if msg.dialed != nil {
			msg.dialed.Close()
		}
		return
	}
	m.setPane(msg.t, msg.v, msg.i, msg.p)
	if msg.warn != "" {
		m.fail(errors.New(msg.warn))
	}
	m.offerSavePassword(msg.host, msg.typed)
}

// setPane replaces pane i, closing what it showed before.
func (m *Model) setPane(t *tab, v *sftpView, i int, p *fpane) {
	old := v.panes[i]
	v.panes[i], v.active, v.edit = p, i, nil
	old.close()
	m.release(old.conn)
	slog.Info("sftp pane", "tab", t.host.Label, "side", i, "shows", p.label, "dir", p.dir)
	m.info("Pane shows " + p.label)
}

// busy reports whether a running transfer reads from or writes to p.
func busy(v *sftpView, p *fpane) bool {
	for _, t := range runningTransfers(v) {
		if t.fromFS == p.fs || t.toFS == p.fs {
			return true
		}
	}
	return false
}

// ---- transfer history ----

func runningTransfers(v *sftpView) []*transfer {
	var out []*transfer
	for _, t := range v.transfers {
		if t.running() {
			out = append(out, t)
		}
	}
	return out
}

// transferSummary is the one line that replaces finished transfers.
func transferSummary(v *sftpView) string {
	var done, failed, canceled int
	for _, t := range v.transfers {
		switch {
		case t.running():
		case errors.Is(t.err, context.Canceled):
			canceled++
		case t.err != nil:
			failed++
		default:
			done++
		}
	}
	var parts []string
	if done > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(good).Render(fmt.Sprintf("✓ %d done", done)))
	}
	if failed > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(bad).Render(fmt.Sprintf("✗ %d failed", failed)))
	}
	if canceled > 0 {
		parts = append(parts, stDim.Render(fmt.Sprintf("⊘ %d canceled", canceled)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, "   ")
}

// clearFinished drops finished transfers, keeping running ones.
func clearFinished(v *sftpView) {
	v.transfers = runningTransfers(v)
	v.histSel = 0
}

func (m *Model) histKey(v *sftpView, k tea.KeyPressMsg) tea.Cmd {
	n := len(v.transfers)
	switch k.String() {
	case "esc", "t", "q":
		v.hist = false
	case "up", "k":
		v.histSel = max(0, v.histSel-1)
	case "down", "j":
		v.histSel = max(0, min(n-1, v.histSel+1))
	case "c", "C":
		clearFinished(v)
	case "enter":
		if v.histSel < n {
			m.jumpTo(v, v.transfers[n-1-v.histSel]) // newest first
		}
	}
	return nil
}

// jumpTo opens the folder a transfer went to, if its pane still shows
// the same place.
func (m *Model) jumpTo(v *sftpView, t *transfer) {
	p := v.panes[t.toPane]
	if p.fs != t.toFS {
		m.fail(errors.New("that pane now shows something else"))
		return
	}
	old := p.dir
	p.dir = p.fs.Dir(t.to)
	if err := p.load(); err != nil {
		p.dir = old
		m.fail(err)
		return
	}
	name := p.fs.Base(t.to)
	for i, e := range p.entries {
		if e.Name() == name {
			p.sel = i
		}
	}
	v.active, v.hist = t.toPane, false
}

// historyLine renders one transfer for the history panel.
func historyLine(t *transfer, w int) string {
	icon, st := "…", lipgloss.NewStyle().Foreground(accent)
	switch {
	case t.running():
	case errors.Is(t.err, context.Canceled):
		icon, st = "⊘", stDim
	case t.err != nil:
		icon, st = "✗", lipgloss.NewStyle().Foreground(bad)
	default:
		icon, st = "✓", lipgloss.NewStyle().Foreground(good)
	}
	took := t.finished
	if t.running() {
		took = time.Since(t.start)
	}
	speed := float64(t.pr.Bytes.Load()) / max(took.Seconds(), 0.001) / 1e6
	route := fmt.Sprintf("%s %s → %s %s", t.fromLabel, t.fromDir, t.toLabel, t.toDir)
	dur := took.Round(100 * time.Millisecond).String()
	if took < time.Second {
		dur = fmt.Sprintf("%dms", took.Milliseconds())
	}
	result := fmt.Sprintf("%8s %7.1f MB/s %7s", human(t.pr.Bytes.Load()), speed, dur)
	if t.err != nil && !errors.Is(t.err, context.Canceled) {
		result = "failed: " + t.err.Error()
	}
	nameW, routeW := 26, max(10, w-26-12-34)
	return fmt.Sprintf(" %s %s %-*s %-*s %s", stDim.Render(t.start.Format("15:04:05")), st.Render(icon),
		nameW, trunc(t.name, nameW), routeW, trunc(route, routeW), st.Render(result))
}

func (m *Model) viewHistory(v *sftpView) []string {
	h := m.bodyH()
	border := lipgloss.NewStyle().Foreground(accent)
	iw := m.w - 2
	title := fmt.Sprintf(" Transfer history · %d ", len(v.transfers))
	out := []string{border.Render("╭─" + title + strings.Repeat("─", max(0, iw-1-lipgloss.Width(title))) + "╮")}
	rows := h - 2
	n := len(v.transfers)
	if n == 0 {
		out = append(out, border.Render("│")+fit(stDim.Render(" No transfers in this tab yet."), iw)+border.Render("│"))
	}
	first := max(0, min(v.histSel-rows+1, n-rows))
	for i := first; i < n && len(out) < rows+1; i++ {
		l := historyLine(v.transfers[n-1-i], iw)
		if i == v.histSel {
			l = stSel.Render(fit(ansi.Strip(l), iw))
		}
		out = append(out, border.Render("│")+fit(l, iw)+border.Render("│"))
	}
	for len(out) < rows+1 {
		out = append(out, border.Render("│")+strings.Repeat(" ", iw)+border.Render("│"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", iw)+"╯"))
}
