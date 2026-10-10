package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/kannan983/pangea/internal/sshc"
)

// ---- terminal tab ----

func (m *Model) updateTerm(t *tab, msg tea.Msg) tea.Cmd {
	if t.state != stReady {
		if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" && t.state != stConnecting {
			return m.connect(t.host, t) // reconnect in place
		}
		return nil
	}
	s := t.sess
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "shift+pgup":
			m.scrollTerm(t, m.h/2)
			return nil
		case "shift+pgdown":
			m.scrollTerm(t, -m.h/2)
			return nil
		}
		t.scroll = 0
		// x/vt drops printable keys that carry shift (upper case, !@#...),
		// so printable text goes to the remote directly.
		if msg.Text != "" && msg.Mod&^(tea.ModShift|tea.ModAlt) == 0 {
			if msg.Mod&tea.ModAlt != 0 {
				s.Write([]byte("\x1b" + msg.Text))
			} else {
				s.Write([]byte(msg.Text))
			}
			return nil
		}
		s.SendKey(vt.KeyPressEvent(msg))
	case tea.PasteMsg:
		t.scroll = 0
		s.Paste(msg.Content)
	case tea.MouseMsg:
		mm := msg.Mouse()
		mm.X -= termPadX
		mm.Y -= 1 + termPadY // tab bar + border
		if w, h := m.termSize(); mm.X < 0 || mm.Y < 0 || mm.X >= w || mm.Y >= h {
			return nil
		}
		if s.MouseTracking() {
			switch msg.(type) {
			case tea.MouseClickMsg:
				s.SendMouse(vt.MouseClick(mm))
			case tea.MouseReleaseMsg:
				s.SendMouse(vt.MouseRelease(mm))
			case tea.MouseWheelMsg:
				s.SendMouse(vt.MouseWheel(mm))
			case tea.MouseMotionMsg:
				s.SendMouse(vt.MouseMotion(mm))
			}
			return nil
		}
		if w, ok := msg.(tea.MouseWheelMsg); ok {
			up := w.Button == tea.MouseWheelUp
			if s.AltScreen() {
				// less, man, etc.: wheel scrolls the app, like xterm's
				// alternate scroll mode.
				key := "\x1b[B"
				if up {
					key = "\x1b[A"
				}
				s.Write([]byte(strings.Repeat(key, 3)))
				return nil
			}
			if up {
				m.scrollTerm(t, 3)
			} else {
				m.scrollTerm(t, -3)
			}
		}
	}
	return nil
}

func (m *Model) scrollTerm(t *tab, delta int) {
	t.scroll = max(0, min(t.scroll+delta, t.sess.ScrollbackLen()))
}

func (m *Model) viewTerm(t *tab, v *tea.View) []string {
	return m.frame(m.termLines(t, v), t.state == stReady)
}

// frame draws the terminal area's border with one column of padding.
func (m *Model) frame(lines []string, live bool) []string {
	w, h := m.termSize()
	st := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
	if live {
		st = lipgloss.NewStyle().Foreground(accent)
	}
	bar := st.Render("│")
	out := []string{st.Render("╭" + strings.Repeat("─", w+2) + "╮")}
	for y := range h {
		l := ""
		if y < len(lines) {
			l = lines[y]
		}
		// Reset attributes so a remote style never leaks into the border.
		out = append(out, bar+" "+fit(l, w)+"\x1b[m "+bar)
	}
	return append(out, st.Render("╰"+strings.Repeat("─", w+2)+"╯"))
}

func (m *Model) termLines(t *tab, v *tea.View) []string {
	w, h := m.termSize()
	switch t.state {
	case stConnecting:
		return center([]string{stBold.Render("Connecting to " + t.host.Label), stDim.Render(t.host.Addr())}, w, h)
	case stFailed:
		return center([]string{
			lipgloss.NewStyle().Foreground(bad).Bold(true).Render("Could not connect to " + t.host.Label),
			"", trunc(t.err.Error(), w-4), "",
			stDim.Render("enter retry · alt+w close · ctrl+] hosts"),
		}, w, h)
	case stEnded:
		return center([]string{stBold.Render(t.host.Label + ": session ended"), "",
			stDim.Render("enter reconnect · alt+w close · ctrl+] hosts")}, w, h)
	}
	s := t.sess
	screen := strings.Split(s.Render(), "\n")
	if t.scroll == 0 {
		if x, y, ok := s.Cursor(); ok {
			v.Cursor = tea.NewCursor(x+termPadX, y+1+termPadY)
		}
		return screen
	}
	// History view: the last t.scroll scrollback lines, then the screen.
	sbLen := s.ScrollbackLen()
	top := sbLen - t.scroll
	lines := s.Scrollback(top, min(sbLen, top+h))
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
)

type promptReply struct {
	text string
	ok   bool
}

type prompt struct {
	t          *tab // tab it belongs to, or nil
	title, sub string
	text       string
	kind       promptKind
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
}

func (p *tuiPrompter) ask(text string, kind promptKind) promptReply {
	ch := make(chan promptReply, 1)
	p.m.prog.Send(promptMsg{&prompt{t: p.t, title: p.t.host.Label, sub: p.t.host.Addr(),
		text: text, kind: kind, reply: ch}})
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
	switch k.String() {
	case "esc", "ctrl+c":
		return m.answer(promptReply{})
	case "enter":
		if p.kind != pConfirm {
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
	if p.kind != pConfirm {
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
	if p.kind == pConfirm {
		box = append(box, row(stAccent.Render("y")+" yes   "+stAccent.Render("n")+" no"))
	} else {
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
