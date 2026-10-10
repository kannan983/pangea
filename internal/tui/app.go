// Package tui is Pangea's terminal UI: a tab bar with the Hosts tab first,
// then one tab per SSH session or SFTP browser.
package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/crypto/ssh"

	"github.com/kannan983/pangea/internal/session"
	"github.com/kannan983/pangea/internal/sshc"
	"github.com/kannan983/pangea/internal/store"
	"github.com/kannan983/pangea/internal/vault"
)

const (
	sidebarW = 20
	frame    = time.Second / 60 // max redraws per second per busy session
)

var (
	accent = lipgloss.Color("#7aa2f7")
	dim    = lipgloss.Color("#737aa2")
	amber  = lipgloss.Color("#e0af68")
	bad    = lipgloss.Color("#f7768e")
	good   = lipgloss.Color("#9ece6a")

	stAccent = lipgloss.NewStyle().Foreground(accent)
	stDim    = lipgloss.NewStyle().Foreground(dim)
	stBold   = lipgloss.NewStyle().Bold(true)
	stSel    = lipgloss.NewStyle().Background(lipgloss.Color("#283457")).Foreground(lipgloss.Color("#c0caf5"))
)

type tabKind int

const (
	tabHosts tabKind = iota
	tabTerm
	tabSFTP
)

type tabState int

const (
	stConnecting tabState = iota
	stReady
	stEnded
	stFailed
)

type tab struct {
	kind     tabKind
	host     store.Host
	state    tabState
	err      error
	sess     *session.Session
	sftp     *sftpView
	activity bool // output while in the background
	scroll   int  // lines scrolled back into history
}

func (t *tab) title() string {
	switch t.kind {
	case tabHosts:
		return "⌂ Hosts"
	case tabSFTP:
		return "⇅ " + t.host.Label
	}
	return t.host.Label
}

func (t *tab) client() *ssh.Client {
	switch {
	case t.sess != nil:
		return t.sess.Client
	case t.sftp != nil:
		return t.sftp.conn
	}
	return nil
}

// zone is a clickable screen rectangle registered during View.
type zone struct {
	x0, y0, x1, y1 int
	click          func(double bool) tea.Cmd
}

type confirmation struct {
	question string
	yes      func() tea.Cmd
}

type Model struct {
	st    *store.Store
	vault *vault.Vault
	prog  *tea.Program
	w, h  int
	mode  string // hosts shown in the Hosts tab

	tabs   []*tab
	active int

	status   string
	statusSt lipgloss.Style
	confirm  *confirmation
	prompts  []*prompt
	pinput   textinput.Model

	hosts    hostsView
	form     *formView     // shown in the Hosts tab while editing a host
	settings *settingsView // shown in the Hosts tab instead of hosts
	sideFoc  bool          // keyboard is in the Hosts tab sidebar
	sideSel  int

	zones     []zone
	zoneOY    int // y offset added by addZone (body starts below the tab bar)
	lastClick struct {
		at   time.Time
		x, y int
	}
}

// New builds the UI; call SetProgram before Run. warn is shown on start.
func New(st *store.Store, v *vault.Vault, warn string) *Model {
	m := &Model{st: st, vault: v, mode: st.Settings.DefaultMode, tabs: []*tab{{kind: tabHosts, state: stReady}}}
	m.hosts = newHostsView()
	m.pinput = textinput.New()
	m.pinput.Prompt = "› "
	if warn != "" {
		m.status, m.statusSt = warn, lipgloss.NewStyle().Foreground(amber)
	}
	return m
}

func (m *Model) SetProgram(p *tea.Program) { m.prog = p }

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) cur() *tab { return m.tabs[m.active] }

func (m *Model) info(s string) { m.status, m.statusSt = s, lipgloss.NewStyle().Foreground(good) }
func (m *Model) fail(err error) {
	m.status, m.statusSt = err.Error(), lipgloss.NewStyle().Foreground(bad)
}

// Geometry. Row 0 is the tab bar; terminal tabs use the rest inside a
// border with 1 column of padding; other tabs keep two footer rows.
func (m *Model) bodyH() int { return max(3, m.h-3) }
func (m *Model) mainW() int { return max(20, m.w-sidebarW-1) }
func (m *Model) mainH() int { return m.bodyH() }

const termPadX, termPadY = 2, 1 // border + padding left, border top

func (m *Model) termSize() (int, int) { return max(10, m.w-2*termPadX), max(3, m.h-1-2*termPadY) }

func (m *Model) knownHosts() string { return filepath.Join(m.st.Dir, "known_hosts") }

// ---- async messages ----

type dialedMsg struct {
	t     *tab
	c     *ssh.Client
	typed string // password typed at a prompt that worked
	err   error
}
type outputMsg struct{ s *session.Session }
type frameMsg struct{ s *session.Session }
type closedMsg struct{ s *session.Session }
type fullscreenDoneMsg struct {
	t   *tab
	err error
}

// ---- tabs ----

func (m *Model) addTab(t *tab) {
	m.tabs = append(m.tabs, t)
	m.active = len(m.tabs) - 1
}

func (m *Model) tabIndex(t *tab) int {
	for i, x := range m.tabs {
		if x == t {
			return i
		}
	}
	return -1
}

func (m *Model) tabOf(s *session.Session) *tab {
	for _, t := range m.tabs {
		if t.sess == s {
			return t
		}
	}
	return nil
}

// release closes c unless another open tab still uses it.
func (m *Model) release(c *ssh.Client) {
	if c == nil {
		return
	}
	for _, t := range m.tabs {
		if t.client() == c {
			return
		}
	}
	c.Close()
}

func (m *Model) closeTab(t *tab) {
	i := m.tabIndex(t)
	if i <= 0 {
		return
	}
	m.tabs = append(m.tabs[:i], m.tabs[i+1:]...)
	if m.active >= i {
		m.active = max(0, m.active-1)
	}
	m.cancelPrompts(t)
	c := t.client()
	if t.sess != nil {
		t.sess.Close()
	}
	if t.sftp != nil {
		t.sftp.sc.Close()
	}
	m.release(c)
}

func (m *Model) askCloseTab(t *tab) {
	if t.kind == tabTerm && t.state == stReady {
		m.ask("Close session to "+t.host.Label+"? (y/n)", func() tea.Cmd { m.closeTab(t); return nil })
		return
	}
	m.closeTab(t)
}

func (m *Model) switchTo(i int) {
	if i >= 0 && i < len(m.tabs) {
		m.active = i
		m.tabs[i].activity = false
	}
}

// connect opens a terminal tab for h (or reuses t to reconnect) and dials
// in the background; prompts appear as a modal.
func (m *Model) connect(h store.Host, t *tab) tea.Cmd {
	if t == nil {
		t = &tab{kind: tabTerm, host: h}
		m.addTab(t)
	}
	t.state, t.err, t.sess, t.scroll = stConnecting, nil, nil, 0
	return m.dial(t)
}

// dial connects t.host in the background, trying its saved password first.
func (m *Model) dial(t *tab) tea.Cmd {
	p := &tuiPrompter{m: m, t: t}
	h, known, saved := t.host, m.knownHosts(), m.vault.Get(t.host.ID)
	return func() tea.Msg {
		c, typed, err := sshc.Dial(h, known, p, saved)
		return dialedMsg{t, c, typed, err}
	}
}

// offerSavePassword asks to store a password that was typed and worked.
func (m *Model) offerSavePassword(h store.Host, pw string) {
	if pw == "" || h.ID == "" {
		return
	}
	m.ask(fmt.Sprintf("Save the password for %s (encrypted)? (y/n)", h.Label), func() tea.Cmd {
		m.vault.Set(h.ID, pw)
		if err := m.vault.Save(); err != nil {
			m.fail(err)
		} else {
			m.info("Password saved for " + h.Label)
		}
		return nil
	})
}

func (m *Model) onDialed(msg dialedMsg) tea.Cmd {
	t := msg.t
	if m.tabIndex(t) < 0 { // tab closed while connecting
		if msg.c != nil {
			msg.c.Close()
		}
		return nil
	}
	if errors.Is(msg.err, sshc.ErrCanceled) {
		m.closeTab(t)
		m.info("Connection canceled")
		return nil
	}
	if msg.err != nil {
		t.state, t.err = stFailed, msg.err
		return nil
	}
	m.offerSavePassword(t.host, msg.typed)
	if t.kind == tabSFTP {
		return m.startSFTP(t, msg.c)
	}
	w, h := m.termSize()
	s, err := session.Open(msg.c, w, h,
		func(s *session.Session) { go m.prog.Send(outputMsg{s}) },
		func(s *session.Session) { go m.prog.Send(closedMsg{s}) })
	if err != nil {
		msg.c.Close()
		t.state, t.err = stFailed, err
		return nil
	}
	t.sess, t.state = s, stReady
	if t.host.ID != "" {
		m.st.Touch(t.host.ID)
		m.save()
	}
	return nil
}

// ---- update ----

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.hosts.search.SetWidth(m.mainW() - 6)
		w, h := m.termSize()
		for _, t := range m.tabs {
			if t.sess != nil && !t.sess.Closed() {
				t.sess.Resize(w, h)
			}
		}
		return m, nil
	case dialedMsg:
		return m, m.onDialed(msg)
	case outputMsg:
		if t := m.tabOf(msg.s); t != nil && t != m.cur() {
			t.activity = true
		}
		s := msg.s
		return m, tea.Tick(frame, func(time.Time) tea.Msg { return frameMsg{s} })
	case frameMsg:
		msg.s.Rendered()
		return m, nil
	case closedMsg:
		if t := m.tabOf(msg.s); t != nil {
			t.state = stEnded
			c := t.sess.Client
			t.sess = nil
			m.release(c)
			if t != m.cur() {
				m.info(t.host.Label + ": session ended")
			}
		}
		return m, nil
	case fullscreenDoneMsg:
		if msg.t.sess != nil && !msg.t.sess.Closed() {
			msg.t.sess.Resize(m.termSize())
		}
		return m, nil
	case promptMsg:
		return m, m.pushPrompt(msg.p)
	case backupDoneMsg:
		if msg.err != nil {
			m.fail(fmt.Errorf("backup failed: %w", msg.err))
		} else {
			m.info(fmt.Sprintf("Backup written to %s (%d key file%s). Keep the passphrase safe.", msg.path, msg.keys, plural(msg.keys)))
		}
		return m, nil
	case sftpTickMsg:
		return m, m.sftpTick(msg.v)
	case transferDoneMsg:
		m.transferDone(msg)
		return m, nil
	}

	if len(m.prompts) > 0 {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			return m, m.promptKey(k)
		}
		if p, ok := msg.(tea.PasteMsg); ok {
			var cmd tea.Cmd
			m.pinput, cmd = m.pinput.Update(p)
			return m, cmd
		}
		return m, nil
	}
	if m.confirm != nil {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			c := m.confirm
			m.confirm, m.status = nil, ""
			if k.String() == "y" {
				return m, c.yes()
			}
			return m, nil
		}
	}

	if k, ok := msg.(tea.KeyPressMsg); ok {
		m.status = "" // messages last until the next key
		if cmd, done := m.globalKey(k); done {
			return m, cmd
		}
	}
	if c, ok := msg.(tea.MouseClickMsg); ok && c.Button == tea.MouseLeft {
		if c.Y == 0 || m.cur().kind != tabTerm {
			return m, m.click(c.X, c.Y)
		}
	}

	switch t := m.cur(); t.kind {
	case tabHosts:
		switch {
		case m.sideFoc:
			return m, m.updateSidebar(msg)
		case m.form != nil:
			return m, m.updateForm(msg)
		case m.settings != nil:
			return m, m.updateSettings(msg)
		}
		return m, m.updateHosts(msg)
	case tabTerm:
		return m, m.updateTerm(t, msg)
	case tabSFTP:
		if t.sftp != nil {
			return m, m.updateSFTP(t, msg)
		}
		if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" && t.state == stFailed {
			return m, m.openSFTPIn(t)
		}
	}
	return m, nil
}

// typing reports whether a text field in the Hosts tab has the keyboard.
func (m *Model) typing() bool {
	return m.active == 0 && !m.sideFoc && (m.form != nil || (m.settings != nil && m.settings.typing()) ||
		(m.settings == nil && m.hosts.search.Focused()))
}

func (m *Model) globalKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	s := k.String()
	if strings.HasPrefix(s, "alt+") && len(s) == 5 && s[4] >= '1' && s[4] <= '9' {
		m.switchTo(int(s[4] - '1'))
		return nil, true
	}
	t := m.cur()
	switch s {
	case "ctrl+]":
		m.switchTo(0)
		return nil, true
	case "alt+w":
		m.askCloseTab(t)
		return nil, true
	case "alt+s":
		if t.kind == tabTerm {
			return m.openSFTPFor(t), true
		}
	case "f11":
		if t.kind == tabTerm && t.state == stReady {
			return tea.Exec(&termCmd{func() error { return t.sess.Attach(os.Stdin, os.Stdout) }},
				func(err error) tea.Msg { return fullscreenDoneMsg{t, err} }), true
		}
	case "ctrl+c":
		if t.kind != tabTerm {
			return m.quit(), true
		}
	case "q":
		if t.kind == tabHosts && !m.typing() {
			return m.quit(), true
		}
	}
	return nil, false
}

func (m *Model) quit() tea.Cmd {
	live := 0
	for _, t := range m.tabs {
		if t.kind == tabTerm && t.state == stReady {
			live++
		}
	}
	if live == 0 {
		return tea.Quit
	}
	m.ask(fmt.Sprintf("%d session(s) open — quit and close them? (y/n)", live), func() tea.Cmd {
		for _, t := range m.tabs {
			if t.sess != nil {
				t.sess.Close()
			}
		}
		return tea.Quit
	})
	return nil
}

func (m *Model) ask(q string, yes func() tea.Cmd) {
	m.confirm = &confirmation{q, yes}
	m.status, m.statusSt = q, lipgloss.NewStyle().Foreground(amber)
}

func (m *Model) save() {
	if err := m.st.Save(); err != nil {
		m.fail(err)
	}
}

// termCmd runs fn with the real terminal (TUI suspended).
type termCmd struct{ fn func() error }

func (c *termCmd) Run() error          { return c.fn() }
func (c *termCmd) SetStdin(io.Reader)  {}
func (c *termCmd) SetStdout(io.Writer) {}
func (c *termCmd) SetStderr(io.Writer) {}

// ---- mouse ----

// click dispatches to the zone under (x, y); two clicks within 400ms on the
// same cell count as a double click.
func (m *Model) click(x, y int) tea.Cmd {
	double := time.Since(m.lastClick.at) < 400*time.Millisecond && m.lastClick.x == x && m.lastClick.y == y
	m.lastClick.at, m.lastClick.x, m.lastClick.y = time.Now(), x, y
	for i := len(m.zones) - 1; i >= 0; i-- {
		z := m.zones[i]
		if x >= z.x0 && x < z.x1 && y >= z.y0 && y < z.y1 {
			return z.click(double)
		}
	}
	return nil
}

func (m *Model) addZone(x, y, w, h int, fn func(double bool) tea.Cmd) {
	y += m.zoneOY
	m.zones = append(m.zones, zone{x, y, x + w, y + h, fn})
}

// ---- view ----

func (m *Model) View() tea.View {
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion, WindowTitle: "Pangea"}
	if m.w == 0 {
		return v
	}
	m.zones = m.zones[:0]
	t := m.cur()

	m.zoneOY = 0
	bar := m.viewTabBar()
	m.zoneOY = 1
	var body []string
	bh := m.bodyH()
	switch t.kind {
	case tabHosts:
		body = m.viewHostsTab()
	case tabSFTP:
		body = m.viewSFTPTab(t)
	case tabTerm:
		bh = m.h - 1
		body = m.viewTerm(t, &v)
	}
	if len(m.prompts) > 0 {
		body = m.overlayPrompt(body, bh)
		v.Cursor = nil
	}

	var b strings.Builder
	b.WriteString(bar)
	for y := range bh {
		b.WriteString("\n")
		if y < len(body) {
			b.WriteString(body[y])
		}
	}
	if t.kind != tabTerm {
		b.WriteString("\n" + fit(" "+m.statusSt.Render(m.status), m.w))
		b.WriteString("\n" + fit(" "+stDim.Render(m.keyHelp()), m.w))
	}
	v.SetContent(b.String())
	return v
}

func (m *Model) viewHostsTab() []string {
	var main []string
	switch {
	case m.form != nil:
		main = m.viewForm()
	case m.settings != nil:
		main = m.viewSettings()
	default:
		main = m.viewHosts()
	}
	lines := make([]string, m.mainH())
	side := m.viewSidebar()
	divider := stDim.Render("│")
	if m.sideFoc {
		divider = stAccent.Render("┃")
	}
	for y := range lines {
		l, r := "", ""
		if y < len(side) {
			l = side[y]
		}
		if y < len(main) {
			r = main[y]
		}
		lines[y] = fit(l, sidebarW) + divider + fit(r, m.mainW())
	}
	return lines
}

// sideItem is one selectable sidebar row: a mode, "+ New mode" or Settings.
type sideItem struct {
	mode string // "" for the two actions
	act  int    // 0 mode, 1 new mode, 2 settings
}

func (m *Model) sideItems() []sideItem {
	var items []sideItem
	for _, mode := range m.st.Settings.Modes {
		items = append(items, sideItem{mode: mode})
	}
	return append(items, sideItem{act: 1}, sideItem{act: 2})
}

// activateSide runs a sidebar item and hands the keyboard back to content.
func (m *Model) activateSide(it sideItem) tea.Cmd {
	m.sideFoc = false
	switch it.act {
	case 1:
		return m.newModePrompt()
	case 2:
		m.openSettings()
		return nil
	}
	m.setMode(it.mode)
	return nil
}

// updateSidebar handles keys while the sidebar has focus.
func (m *Model) updateSidebar(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	items := m.sideItems()
	m.sideSel = max(0, min(m.sideSel, len(items)-1))
	switch k.String() {
	case "up", "k":
		m.sideSel = max(0, m.sideSel-1)
	case "down", "j":
		m.sideSel = min(len(items)-1, m.sideSel+1)
	case "enter", "space", "right", "l":
		return m.activateSide(items[m.sideSel])
	case "tab", "esc":
		m.sideFoc = false
	}
	return nil
}

// focusSidebar moves the keyboard to the sidebar, on the current item.
func (m *Model) focusSidebar() {
	m.sideFoc, m.sideSel = true, 0
	for i, it := range m.sideItems() {
		if (m.settings != nil && it.act == 2) || (m.settings == nil && it.act == 0 && it.mode == m.mode) {
			m.sideSel = i
		}
	}
	m.hosts.search.Blur()
}

// viewSidebar lists modes (click, m, or Tab then arrows), + New mode, Settings.
func (m *Model) viewSidebar() []string {
	side := []string{"", stBold.Foreground(accent).Render("  Pangea"), "", stDim.Render("  MODES")}
	for i, it := range m.sideItems() {
		if it.act == 2 {
			side = append(side, "")
		}
		var label, count string
		active := false
		switch it.act {
		case 0:
			label, count = trunc(it.mode, sidebarW-7), fmt.Sprintf("%3d", m.st.ModeCount(it.mode))
			active = it.mode == m.mode && m.settings == nil
		case 1:
			label = "+ New mode"
		case 2:
			label, active = "⚙ Settings", m.settings != nil
		}
		row := "  " + fit(label, sidebarW-7) + stDim.Render(count)
		switch {
		case m.sideFoc && i == m.sideSel:
			row = stSel.Bold(true).Render(fit("› "+fit(label, sidebarW-7)+count, sidebarW))
		case active:
			row = stAccent.Bold(true).Render("▌ "+fit(label, sidebarW-7)) + stDim.Render(count)
		case it.act == 1:
			row = stDim.Render(row)
		}
		y := len(side)
		side = append(side, row)
		it := it
		m.addZone(0, y, sidebarW, 1, func(bool) tea.Cmd { return m.activateSide(it) })
	}
	if open := m.openSessions(); open > 0 {
		side = append(side, "", stDim.Render(fmt.Sprintf("  %d open session%s", open, plural(open))))
	}
	return side
}

func (m *Model) openSessions() int {
	n := 0
	for _, t := range m.tabs[1:] {
		if t.kind == tabTerm && t.state == stReady {
			n++
		}
	}
	return n
}

// setMode shows mode's hosts (and leaves form/settings).
func (m *Model) setMode(mode string) {
	m.mode, m.form, m.settings = mode, nil, nil
	m.hosts.sel, m.hosts.scroll = 0, 0
}

// nextMode cycles through modes.
func (m *Model) nextMode() {
	ms := m.st.Settings.Modes
	for i, x := range ms {
		if x == m.mode {
			m.setMode(ms[(i+1)%len(ms)])
			m.info("Mode: " + m.mode)
			return
		}
	}
	m.setMode(ms[0])
}

// newModePrompt asks for a name and adds the mode.
func (m *Model) newModePrompt() tea.Cmd {
	return m.askText("New mode", "Name for the new mode (e.g. Office, Personal)", "", func(name string) tea.Cmd {
		if err := m.st.AddMode(name); err != nil {
			m.fail(err)
			return nil
		}
		m.save()
		m.setMode(strings.TrimSpace(name))
		m.info("Added mode " + m.mode)
		return nil
	})
}

var (
	stTab       = lipgloss.NewStyle().Background(lipgloss.Color("#1f2335")).Foreground(lipgloss.Color("#a9b1d6"))
	stTabActive = lipgloss.NewStyle().Background(lipgloss.Color("#3b4261")).Foreground(lipgloss.Color("#c0caf5")).Bold(true)
)

func (m *Model) viewTabBar() string {
	var b strings.Builder
	x := 0
	for i, t := range m.tabs {
		mark := ""
		switch {
		case t.kind == tabHosts:
		case t.state == stConnecting:
			mark = "… "
		case t.state == stReady && t.kind == tabTerm:
			mark = lipgloss.NewStyle().Foreground(good).Render("●") + " "
		case t.state == stEnded || t.state == stFailed:
			mark = lipgloss.NewStyle().Foreground(bad).Render("○") + " "
		}
		title := trunc(t.title(), 22)
		if t.activity {
			title = lipgloss.NewStyle().Foreground(amber).Render(title)
		}
		st := stTab
		if i == m.active {
			st = stTabActive
		}
		num := stDim.Render(fmt.Sprintf("%d ", i+1))
		label := " " + num + mark + title + " "
		closeX := ""
		if i > 0 {
			closeX = "× "
		}
		seg := st.Render(label + closeX)
		w := lipgloss.Width(seg)
		i, t := i, t
		m.addZone(x, 0, w, 1, func(bool) tea.Cmd { m.switchTo(i); return nil })
		if closeX != "" {
			m.addZone(x+w-2, 0, 2, 1, func(bool) tea.Cmd { m.askCloseTab(t); return nil })
		}
		b.WriteString(seg + " ")
		x += w + 1
	}
	right := ""
	if m.cur().kind == tabTerm {
		// Terminal tabs have no footer: messages and hints go here.
		if m.status != "" {
			right = m.statusSt.Render(m.status)
		} else if t := m.cur(); t.scroll > 0 {
			right = lipgloss.NewStyle().Foreground(amber).Render(fmt.Sprintf("history −%d · any key returns", t.scroll))
		} else {
			right = stDim.Render("ctrl+] hosts · alt+s sftp · alt+1-9 tabs · alt+w close · F11 full")
		}
	}
	left := b.String()
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		return fit(left, m.w)
	}
	return left + strings.Repeat(" ", gap) + right + " "
}

func (m *Model) keyHelp() string {
	if m.confirm != nil {
		return "y confirm · any other key cancels"
	}
	t := m.cur()
	switch t.kind {
	case tabHosts:
		switch {
		case m.sideFoc:
			return "↑↓ move · enter open · tab/esc back to hosts · q quit"
		case m.form != nil:
			return "tab/↓ next · shift+tab/↑ prev · enter save (on last field) · ctrl+s save · esc cancel"
		case m.settings != nil:
			return m.settingsHelp()
		case m.hosts.search.Focused():
			return "type to search · enter connect · ↓ hosts · esc clear · alt+1-9 tabs · ctrl+c quit"
		}
		return "↑↓ move · enter connect · s sftp · n new · e edit · c clone · d delete · m mode · v view · / search · tab sidebar · q quit"
	case tabSFTP:
		if t.sftp != nil && t.sftp.edit != nil {
			return "type a path · tab complete · enter go · esc cancel"
		}
		return "tab pane · ↑↓ move · enter open · backspace up · / go to path · c copy → · r refresh · drag across · drop to upload · alt+w close"
	}
	return ""
}

// ---- helpers ----

// fit pads or truncates a styled string to exactly w cells.
func fit(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw > w {
		return ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", w-sw)
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		if n <= 1 {
			return string(r[:max(0, n)])
		}
		return string(r[:n-1]) + "…"
	}
	return s
}

// center places lines in the middle of a w×h area.
func center(lines []string, w, h int) []string {
	out := make([]string, max(0, (h-len(lines))/2))
	for _, l := range lines {
		pad := max(0, (w-lipgloss.Width(l))/2)
		out = append(out, strings.Repeat(" ", pad)+l)
	}
	return out
}
