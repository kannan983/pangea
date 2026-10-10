package tui

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"golang.org/x/crypto/ssh"

	"github.com/kannan983/pangea/internal/session"
	"github.com/kannan983/pangea/internal/sshc"
	"github.com/kannan983/pangea/internal/store"
)

// ---- panes ----

// pane is one shell in a terminal tab; a tab holds one, or two when split.
type pane struct {
	sess   *session.Session
	scroll int        // lines scrolled back into history
	sel    *selection // mouse text selection, in pane cells
}

type selection struct {
	ax, ay, bx, by int
	moved          bool
}

// ordered returns the selection start and end in reading order.
func (s *selection) ordered() (x0, y0, x1, y1 int) {
	if s.ay < s.by || (s.ay == s.by && s.ax <= s.bx) {
		return s.ax, s.ay, s.bx, s.by
	}
	return s.bx, s.by, s.ax, s.ay
}

// selectRegion returns the selected text of plain screen lines, one line
// per row with trailing spaces trimmed.
func selectRegion(lines []string, s *selection, w int) string {
	x0, y0, x1, y1 := s.ordered()
	var out []string
	for y := max(0, y0); y <= y1 && y < len(lines); y++ {
		from, to := 0, w
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		out = append(out, strings.TrimRight(ansi.Cut(lines[y], from, to), " "))
	}
	return strings.Join(out, "\n")
}

// highlight redraws selected rows from plain text with the selection in
// reverse video (colors on those rows are dropped while selecting).
func highlight(rendered, plain []string, s *selection, w int) []string {
	x0, y0, x1, y1 := s.ordered()
	out := append([]string(nil), rendered...)
	for y := max(0, y0); y <= y1 && y < len(out) && y < len(plain); y++ {
		from, to := 0, w
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		line := fit(plain[y], w)
		out[y] = ansi.Cut(line, 0, from) + "\x1b[7m" + ansi.Cut(line, from, to) + "\x1b[27m" + ansi.Cut(line, to, w)
	}
	return out
}

type rect struct{ x, y, w, h int }

// inner is the area inside a pane's border and padding.
func (r rect) inner() rect {
	return rect{r.x + termPadX, r.y + termPadY, max(10, r.w-2*termPadX), max(3, r.h-2*termPadY)}
}

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// paneRects lays out n panes in the body (below the tab bar): side by side
// when the window is wide enough, else stacked.
func (m *Model) paneRects(n int) []rect {
	W, H := m.w, m.h-1
	switch {
	case n < 2:
		return []rect{{0, 0, W, H}}
	case W >= 120:
		return []rect{{0, 0, W / 2, H}, {W / 2, 0, W - W/2, H}}
	}
	return []rect{{0, 0, W, H / 2}, {0, H / 2, W, H - H/2}}
}

func (t *tab) pane() *pane {
	if t.focus < len(t.panes) {
		return t.panes[t.focus]
	}
	return nil
}

// resizePanes fits every pane's PTY to its place in the layout.
func (m *Model) resizePanes(t *tab) {
	rs := m.paneRects(len(t.panes))
	for i, p := range t.panes {
		if in := rs[i].inner(); !p.sess.Closed() {
			p.sess.Resize(in.w, in.h)
		}
	}
}

func (m *Model) openSession(t *tab, c *ssh.Client, w, h int) (*session.Session, error) {
	return session.Open(c, w, h,
		func(s *session.Session) { go m.prog.Send(outputMsg{s}) },
		func(s *session.Session) { go m.prog.Send(closedMsg{s}) })
}

type splitMsg struct {
	t   *tab
	s   *session.Session
	err error
}

// split opens a second shell on the same connection next to the first;
// with two panes it moves focus to the other one.
func (m *Model) split(t *tab) tea.Cmd {
	if t.state != stReady || len(t.panes) == 0 {
		return nil
	}
	if len(t.panes) == 2 {
		t.focus = 1 - t.focus
		return nil
	}
	c := t.panes[0].sess.Client
	in := m.paneRects(2)[1].inner()
	return func() tea.Msg {
		s, err := m.openSession(t, c, in.w, in.h)
		return splitMsg{t, s, err}
	}
}

func (m *Model) onSplit(msg splitMsg) {
	t := msg.t
	switch {
	case msg.err != nil:
		slog.Warn("split failed", "host", t.host.Label, "err", msg.err)
		m.fail(fmt.Errorf("split: %w", msg.err))
		return
	case m.tabIndex(t) < 0 || t.state != stReady || len(t.panes) != 1:
		msg.s.Close() // tab changed meanwhile
		return
	}
	t.panes = append(t.panes, &pane{sess: msg.s})
	t.focus = 1
	m.resizePanes(t)
	slog.Info("split", "host", t.host.Label)
}

// popOut moves the focused pane of a split tab into its own tab.
func (m *Model) popOut(t *tab) {
	if len(t.panes) != 2 {
		return
	}
	p := t.panes[t.focus]
	t.panes = append(t.panes[:t.focus], t.panes[t.focus+1:]...)
	t.focus = 0
	m.resizePanes(t)
	nt := &tab{kind: tabTerm, host: t.host, state: stReady, panes: []*pane{p}}
	i := m.tabIndex(t) + 1
	m.tabs = append(m.tabs[:i], append([]*tab{nt}, m.tabs[i:]...)...)
	m.active = i
	m.resizePanes(nt)
}

// ---- terminal tab input ----

func (m *Model) updateTerm(t *tab, msg tea.Msg) tea.Cmd {
	if t.state != stReady {
		if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" && t.state != stConnecting {
			return m.connect(t.host, t) // reconnect in place
		}
		return nil
	}
	p := t.pane()
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "shift+pgup":
			m.scrollPane(p, m.h/2)
			return nil
		case "shift+pgdown":
			m.scrollPane(p, -m.h/2)
			return nil
		}
		p.scroll, p.sel = 0, nil
		// x/vt drops printable keys that carry shift (upper case, !@#...),
		// so printable text goes to the remote directly.
		if msg.Text != "" && msg.Mod&^(tea.ModShift|tea.ModAlt) == 0 {
			if msg.Mod&tea.ModAlt != 0 {
				p.sess.Write([]byte("\x1b" + msg.Text))
			} else {
				p.sess.Write([]byte(msg.Text))
			}
			return nil
		}
		p.sess.SendKey(vt.KeyPressEvent(msg))
	case tea.PasteMsg:
		p.scroll, p.sel = 0, nil
		p.sess.Paste(msg.Content)
	case tea.MouseMsg:
		return m.termMouse(t, msg)
	}
	return nil
}

func (m *Model) termMouse(t *tab, msg tea.MouseMsg) tea.Cmd {
	mm := msg.Mouse()
	x, y := mm.X, mm.Y-1 // body coordinates
	rs := m.paneRects(len(t.panes))
	i := -1
	for j, r := range rs {
		if r.has(x, y) {
			i = j
		}
	}
	if i < 0 || i >= len(t.panes) {
		return nil
	}
	if _, ok := msg.(tea.MouseClickMsg); ok {
		t.focus = i
	}
	p, in := t.panes[i], rs[i].inner()
	lx := max(0, min(in.w-1, x-in.x))
	ly := max(0, min(in.h-1, y-in.y))
	inside := x >= in.x && x < in.x+in.w && y >= in.y && y < in.y+in.h

	// Apps that use the mouse (htop, vim) get it; shift bypasses that.
	if p.sess.MouseTracking() && mm.Mod&tea.ModShift == 0 && p.scroll == 0 {
		if !inside {
			return nil
		}
		mm.X, mm.Y = lx, ly
		switch msg.(type) {
		case tea.MouseClickMsg:
			p.sess.SendMouse(vt.MouseClick(mm))
		case tea.MouseReleaseMsg:
			p.sess.SendMouse(vt.MouseRelease(mm))
		case tea.MouseWheelMsg:
			p.sess.SendMouse(vt.MouseWheel(mm))
		case tea.MouseMotionMsg:
			p.sess.SendMouse(vt.MouseMotion(mm))
		}
		return nil
	}

	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		p.sel = nil
		up := msg.Button == tea.MouseWheelUp
		if p.sess.AltScreen() {
			// less, man, etc.: wheel scrolls the app, like xterm's
			// alternate scroll mode.
			key := "\x1b[B"
			if up {
				key = "\x1b[A"
			}
			p.sess.Write([]byte(strings.Repeat(key, 3)))
			return nil
		}
		if up {
			m.scrollPane(p, 3)
		} else {
			m.scrollPane(p, -3)
		}
	case tea.MouseClickMsg:
		p.sel = nil
		if msg.Button == tea.MouseLeft && inside {
			p.sel = &selection{ax: lx, ay: ly, bx: lx, by: ly}
		}
	case tea.MouseMotionMsg:
		if p.sel != nil {
			p.sel.bx, p.sel.by, p.sel.moved = lx, ly, true
		}
	case tea.MouseReleaseMsg:
		if p.sel == nil {
			return nil
		}
		if !p.sel.moved {
			p.sel = nil
			return nil
		}
		text := selectRegion(m.plainLines(p, in.h), p.sel, in.w)
		if text == "" {
			return nil
		}
		const maxClip = 1 << 20 // keep the OSC 52 payload sane
		if len(text) > maxClip {
			text = text[:maxClip]
		}
		m.info(fmt.Sprintf("Copied %d characters", len([]rune(text))))
		return tea.SetClipboard(text)
	}
	return nil
}

func (m *Model) scrollPane(p *pane, delta int) {
	p.scroll = max(0, min(p.scroll+delta, p.sess.ScrollbackLen()))
}

// ---- terminal tab view ----

// viewTerm draws the tab's panes, each in its own border.
func (m *Model) viewTerm(t *tab, v *tea.View) []string {
	if t.state != stReady || len(t.panes) == 0 {
		r := m.paneRects(1)[0]
		in := r.inner()
		return m.frameBox(m.stateLines(t, in.w, in.h), r, t.state == stReady)
	}
	rs := m.paneRects(len(t.panes))
	boxes := make([][]string, len(t.panes))
	for i, p := range t.panes {
		in := rs[i].inner()
		lines := m.paneLines(p, in)
		if i == t.focus && p.scroll == 0 && p.sel == nil {
			if x, y, ok := p.sess.Cursor(); ok {
				v.Cursor = tea.NewCursor(in.x+x, 1+in.y+y)
			}
		}
		boxes[i] = m.frameBox(lines, rs[i], len(t.panes) == 1 || i == t.focus)
	}
	if len(boxes) == 1 {
		return boxes[0]
	}
	if rs[1].x > 0 { // side by side
		out := make([]string, len(boxes[0]))
		for r := range out {
			out[r] = boxes[0][r] + boxes[1][r]
		}
		return out
	}
	return append(boxes[0], boxes[1]...)
}

// frameBox draws a rounded border around lines for rect r.
func (m *Model) frameBox(lines []string, r rect, hot bool) []string {
	in := r.inner()
	st := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
	if hot {
		st = lipgloss.NewStyle().Foreground(accent)
	}
	bar := st.Render("│")
	out := []string{st.Render("╭" + strings.Repeat("─", r.w-2) + "╮")}
	for y := range in.h {
		l := ""
		if y < len(lines) {
			l = lines[y]
		}
		// Reset attributes so a remote style never leaks into the border.
		out = append(out, bar+" "+fit(l, in.w)+"\x1b[m "+bar)
	}
	return append(out, st.Render("╰"+strings.Repeat("─", r.w-2)+"╯"))
}

func (m *Model) stateLines(t *tab, w, h int) []string {
	switch t.state {
	case stConnecting:
		return center([]string{stBold.Render("Connecting to " + t.host.Label), stDim.Render(t.host.Addr())}, w, h)
	case stFailed:
		return center([]string{
			lipgloss.NewStyle().Foreground(bad).Bold(true).Render("Could not connect to " + t.host.Label),
			"", trunc(t.err.Error(), w-4), "",
			stDim.Render("enter retry · alt+w close · ctrl+] hosts"),
		}, w, h)
	}
	return center([]string{stBold.Render(t.host.Label + ": session ended"), "",
		stDim.Render("enter reconnect · alt+w close · ctrl+] hosts")}, w, h)
}

// paneLines is what a pane shows: the live screen, or history when
// scrolled back; selected text is highlighted.
func (m *Model) paneLines(p *pane, in rect) []string {
	var lines []string
	if p.scroll == 0 {
		lines = strings.Split(p.sess.Render(), "\n")
	} else {
		// History view: the last p.scroll scrollback lines, then the screen.
		sbLen := p.sess.ScrollbackLen()
		top := sbLen - p.scroll
		lines = p.sess.Scrollback(top, min(sbLen, top+in.h))
		screen := strings.Split(p.sess.Render(), "\n")
		for i := 0; len(lines) < in.h && i < len(screen); i++ {
			lines = append(lines, screen[i])
		}
	}
	if p.sel != nil {
		lines = highlight(lines, m.plainLines(p, in.h), p.sel, in.w)
	}
	return lines
}

// plainLines is paneLines without colors, for selecting text.
func (m *Model) plainLines(p *pane, h int) []string {
	screen := p.sess.PlainScreen()
	if p.scroll == 0 {
		return screen
	}
	sbLen := p.sess.ScrollbackLen()
	top := sbLen - p.scroll
	var lines []string
	for _, l := range p.sess.Scrollback(top, min(sbLen, top+h)) {
		lines = append(lines, ansi.Strip(l))
	}
	for i := 0; len(lines) < h && i < len(screen); i++ {
		lines = append(lines, screen[i])
	}
	return lines
}

// ---- prompts (passwords, host keys, names) as a modal ----

type promptKind int

const (
	pSecret promptKind = iota
	pText
	pConfirm
	pChoice
	pPick
)

type choice struct{ key, label string }

// pickItem is one row of a pPick list.
type pickItem struct {
	label, detail string
	run           func() tea.Cmd
}

type promptReply struct {
	text string
	ok   bool
}

type prompt struct {
	t          *tab // tab it belongs to, or nil
	title, sub string
	text       string
	kind       promptKind
	choices    []choice   // pChoice
	items      []pickItem // pPick
	pickSel    int
	value      string                             // prefilled answer
	reply      chan promptReply                   // answered to a dial goroutine, or
	done       func(text string, ok bool) tea.Cmd // handled in the UI
	replied    bool
}

type promptMsg struct{ p *prompt }

// tuiPrompter implements sshc.Prompter from the dial goroutine.
type tuiPrompter struct {
	m *Model
	t *tab
	h *store.Host // host being dialed when it isn't t.host (SFTP panes)
}

func (p *tuiPrompter) ask(text string, kind promptKind) promptReply {
	h := p.t.host
	if p.h != nil {
		h = *p.h
	}
	ch := make(chan promptReply, 1)
	p.m.prog.Send(promptMsg{&prompt{t: p.t, title: h.Label, sub: h.Addr(), text: text, kind: kind, reply: ch}})
	return <-ch
}

func (p *tuiPrompter) Secret(text string) (string, error) {
	r := p.ask(text, pSecret)
	if !r.ok {
		return "", sshc.ErrCanceled
	}
	return r.text, nil
}

func (p *tuiPrompter) Confirm(text string) bool { return p.ask(text, pConfirm).ok }

// askText / askSecret prompt from the UI; done runs with the answer.
func (m *Model) askText(title, text, value string, done func(string) tea.Cmd) tea.Cmd {
	return m.pushPrompt(&prompt{title: title, text: text, kind: pText, value: value,
		done: func(s string, ok bool) tea.Cmd {
			if !ok {
				return nil
			}
			return done(s)
		}})
}

func (m *Model) askSecret(title, text string, done func(string) tea.Cmd) tea.Cmd {
	return m.pushPrompt(&prompt{title: title, text: text, kind: pSecret,
		done: func(s string, ok bool) tea.Cmd {
			if !ok {
				return nil
			}
			return done(s)
		}})
}

// askConfirm asks a yes/no question; done runs on yes.
func (m *Model) askConfirm(title, text string, done func() tea.Cmd) tea.Cmd {
	return m.pushPrompt(&prompt{title: title, text: text, kind: pConfirm,
		done: func(_ string, ok bool) tea.Cmd {
			if !ok {
				return nil
			}
			return done()
		}})
}

// askPick shows a fuzzy-filtered list; enter runs the selected item.
func (m *Model) askPick(title, text string, items []pickItem) tea.Cmd {
	return m.pushPrompt(&prompt{title: title, text: text, kind: pPick, items: items})
}

// filterPick returns items matching q, best first (all when q is empty).
func filterPick(items []pickItem, q string) []pickItem {
	q = strings.TrimSpace(q)
	if q == "" {
		return items
	}
	type scored struct {
		it pickItem
		s  int
	}
	var out []scored
	for _, it := range items {
		if sc := fuzzyScore(q, it.label+" "+it.detail); sc >= 0 {
			out = append(out, scored{it, sc})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].s > out[b].s })
	res := make([]pickItem, len(out))
	for i, o := range out {
		res[i] = o.it
	}
	return res
}

// askChoice offers single-key choices; done gets the chosen key (esc cancels).
func (m *Model) askChoice(title, text string, choices []choice, done func(key string) tea.Cmd) tea.Cmd {
	return m.pushPrompt(&prompt{title: title, text: text, kind: pChoice, choices: choices,
		done: func(s string, ok bool) tea.Cmd {
			if !ok {
				return nil
			}
			return done(s)
		}})
}

func (m *Model) pushPrompt(p *prompt) tea.Cmd {
	m.prompts = append(m.prompts, p)
	if len(m.prompts) == 1 {
		return m.showPrompt()
	}
	return nil
}

func (m *Model) showPrompt() tea.Cmd {
	p := m.prompts[0]
	if p.t != nil {
		m.switchTo(m.tabIndex(p.t))
	}
	m.pinput.Reset()
	m.pinput.SetWidth(min(56, m.w-12))
	m.pinput.EchoMode = textinput.EchoNormal
	if p.kind == pSecret {
		m.pinput.EchoMode = textinput.EchoPassword
	}
	m.pinput.SetValue(p.value)
	m.pinput.CursorEnd()
	return m.pinput.Focus()
}

func (m *Model) answer(r promptReply) tea.Cmd {
	p := m.prompts[0]
	m.prompts = m.prompts[1:]
	var cmds []tea.Cmd
	if !p.replied {
		p.replied = true
		if p.reply != nil {
			p.reply <- r
		}
		if p.done != nil {
			cmds = append(cmds, p.done(r.text, r.ok))
		}
	}
	if len(m.prompts) > 0 {
		cmds = append(cmds, m.showPrompt())
	}
	return tea.Batch(cmds...)
}

func (m *Model) promptKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.prompts[0]
	if p.kind == pChoice {
		for _, c := range p.choices {
			if k.String() == c.key {
				return m.answer(promptReply{c.key, true})
			}
		}
	}
	if p.kind == pPick {
		f := filterPick(p.items, m.pinput.Value())
		switch k.String() {
		case "up", "ctrl+p":
			p.pickSel = max(0, p.pickSel-1)
			return nil
		case "down", "ctrl+n", "tab":
			p.pickSel = max(0, min(len(f)-1, p.pickSel+1))
			return nil
		case "enter":
			if p.pickSel < len(f) {
				run := f[p.pickSel].run
				return tea.Batch(m.answer(promptReply{ok: true}), run())
			}
			return nil
		case "esc", "ctrl+c":
			return m.answer(promptReply{})
		}
		var cmd tea.Cmd
		m.pinput, cmd = m.pinput.Update(k)
		p.pickSel = 0
		return cmd
	}
	switch k.String() {
	case "esc", "ctrl+c":
		return m.answer(promptReply{})
	case "enter":
		if p.kind == pSecret || p.kind == pText {
			return m.answer(promptReply{m.pinput.Value(), true})
		}
	case "y":
		if p.kind == pConfirm {
			return m.answer(promptReply{ok: true})
		}
	case "n":
		if p.kind == pConfirm {
			return m.answer(promptReply{})
		}
	}
	if p.kind == pSecret || p.kind == pText {
		var cmd tea.Cmd
		m.pinput, cmd = m.pinput.Update(k)
		return cmd
	}
	return nil
}

// cancelPrompts answers (as canceled) every prompt that belongs to t.
func (m *Model) cancelPrompts(t *tab) {
	keep := m.prompts[:0]
	for _, p := range m.prompts {
		if p.t == t {
			if !p.replied && p.reply != nil {
				p.replied = true
				p.reply <- promptReply{}
			}
			continue
		}
		keep = append(keep, p)
	}
	m.prompts = keep
}

// overlayPrompt draws the current prompt as a box over the body lines.
func (m *Model) overlayPrompt(body []string, h int) []string {
	p := m.prompts[0]
	bw := min(70, m.w-4)
	border := lipgloss.NewStyle().Foreground(accent)
	row := func(s string) string { return border.Render("│") + " " + fit(s, bw-4) + " " + border.Render("│") }
	box := []string{border.Render("╭" + strings.Repeat("─", bw-2) + "╮"),
		row(stBold.Render(p.title) + stDim.Render("  "+p.sub)), row("")}
	for _, l := range strings.Split(p.text, "\n") {
		box = append(box, row(trunc(l, bw-4)))
	}
	box = append(box, row(""))
	switch p.kind {
	case pConfirm:
		box = append(box, row(stAccent.Render("y")+" yes   "+stAccent.Render("n")+" no"))
	case pChoice:
		var opts []string
		for _, c := range p.choices {
			opts = append(opts, stAccent.Render(c.key)+" "+c.label)
		}
		box = append(box, row(strings.Join(append(opts, stDim.Render("esc cancel")), "   ")))
	case pPick:
		box = append(box, row(m.pinput.View()), row(""))
		f := filterPick(p.items, m.pinput.Value())
		const shown = 10
		first := max(0, min(p.pickSel-shown+1, len(f)-shown))
		for i := first; i < len(f) && i < first+shown; i++ {
			l := fit(" "+f[i].label, 30) + stDim.Render(f[i].detail)
			if i == p.pickSel {
				l = stSel.Render(fit(" "+f[i].label, 30)) + stSel.Render(fit(f[i].detail, bw-34))
			}
			box = append(box, row(l))
		}
		if len(f) == 0 {
			box = append(box, row(stDim.Render(" no match")))
		}
		box = append(box, row(""), row(stDim.Render("type to filter · ↑↓ select · enter open · esc cancel")))
	default:
		box = append(box, row(m.pinput.View()), row(""), row(stDim.Render("enter ok · esc cancel")))
	}
	box = append(box, border.Render("╰"+strings.Repeat("─", bw-2)+"╯"))

	out := make([]string, h)
	copy(out, body)
	top := max(0, (h-len(box))/2)
	left := max(0, (m.w-bw)/2)
	for i, l := range box {
		if r := top + i; r < h {
			// Splice the box in so what's around it (e.g. borders) stays.
			line := fit(out[r], m.w)
			out[r] = ansi.Truncate(line, left, "") + "\x1b[m" + l + ansi.TruncateLeft(line, left+lipgloss.Width(l), "")
		}
	}
	return out
}
