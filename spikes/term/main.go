// Spike 1+2: SSH sessions rendered inside Bubble Tea through x/vt (embedded),
// plus takeover mode that hands the raw terminal to a session and repaints
// from the emulator on entry.
//
// Keys: alt+n new tab, alt+1..9 switch tab, ctrl+] toggles takeover,
// ctrl+q quit. Everything else goes to the remote.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/vt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/kannan983/pangea/spikes/sshx"
)

type session struct {
	id    int
	sess  *ssh.Session
	stdin io.Writer
	emu   *vt.SafeEmulator
	dirty atomic.Bool

	mu     sync.Mutex
	mirror io.Writer // os.Stdout while in takeover

	// Lazy mode (PANGEA_LAZY=1): in takeover the emulator is skipped and only
	// the raw tail is kept, replayed into the emulator on return.
	tail      []byte
	overflow  bool
	replaying atomic.Bool
}

const tailMax = 1 << 20

var lazy = os.Getenv("PANGEA_LAZY") == "1"

type outputMsg struct{}
type closedMsg struct{ id int }
type takeoverDoneMsg struct{ err error }

var prog *tea.Program

func open(client *ssh.Client, id, w, h int) (*session, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	if err := sess.RequestPty("xterm-256color", h, w, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		return nil, err
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	s := &session{id: id, sess: sess, stdin: stdin, emu: vt.NewSafeEmulator(w, h)}
	if n, err := strconv.Atoi(os.Getenv("PANGEA_SCROLLBACK")); err == nil {
		s.emu.SetScrollbackSize(n)
	}
	if err := sess.Shell(); err != nil {
		return nil, err
	}
	// Emulator replies (DA, cursor reports, encoded keys) go to the remote.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := s.emu.Read(buf)
			if err != nil {
				return
			}
			// Replies to queries replayed from the tail must not reach the remote.
			if !s.replaying.Load() {
				stdin.Write(buf[:n])
			}
		}
	}()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				s.mu.Lock()
				if s.mirror != nil {
					s.mirror.Write(buf[:n])
				}
				if s.mirror != nil && lazy {
					s.keepTail(buf[:n])
				} else {
					s.emu.Write(buf[:n])
				}
				s.mu.Unlock()
				// Coalesce: at most one pending redraw message. Send from a
				// goroutine: the event loop is blocked while in takeover.
				if !s.dirty.Swap(true) {
					go prog.Send(outputMsg{})
				}
			}
			if err != nil {
				prog.Send(closedMsg{id})
				return
			}
		}
	}()
	return s, nil
}

func (s *session) keepTail(b []byte) {
	s.tail = append(s.tail, b...)
	if len(s.tail) > 2*tailMax {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-tailMax:]...)
		s.overflow = true
	}
}

// replayTail feeds the bytes seen during takeover into the emulator.
// ponytail: a cut tail can start mid escape sequence; the resize on return
// makes full-screen apps redraw, which hides that. Shell output beyond the
// tail is lost from the emulator's scrollback.
func (s *session) replayTail() {
	if len(s.tail) > tailMax {
		s.tail = s.tail[len(s.tail)-tailMax:]
	}
	s.replaying.Store(true)
	if s.overflow {
		s.emu.Write([]byte("\x1bc"))
	}
	s.emu.Write(s.tail)
	s.replaying.Store(false)
	s.tail, s.overflow = nil, false
}

// resize scrolls content up when shrinking past the cursor, like xterm;
// x/vt alone crops the bottom rows and the shell then overwrites output.
func (s *session) resize(w, h int) {
	if k := s.emu.CursorPosition().Y - h + 1; k > 0 {
		s.emu.Write(fmt.Appendf(nil, "\x1b7\x1b[%d;1H%s\x1b8\x1b[%dA", s.emu.Height(), strings.Repeat("\n", k), k))
	}
	s.emu.Resize(w, h)
	s.sess.WindowChange(h, w)
}

// takeover implements tea.ExecCommand: raw passthrough until ctrl+].
type takeover struct {
	s    *session
	w, h int
}

func (t *takeover) SetStdin(io.Reader)  {}
func (t *takeover) SetStdout(io.Writer) {}
func (t *takeover) SetStderr(io.Writer) {}

func (t *takeover) Run() error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)

	t.s.resize(t.w, t.h)
	// Repaint what the session currently shows, then mirror live output.
	pos := t.s.emu.CursorPosition()
	fmt.Fprintf(os.Stdout, "\x1b[H\x1b[2J%s\x1b[%d;%dH", strings.ReplaceAll(t.s.emu.Render(), "\n", "\r\n"), pos.Y+1, pos.X+1)
	t.s.mu.Lock()
	t.s.mirror = os.Stdout
	t.s.mu.Unlock()
	defer func() {
		t.s.mu.Lock()
		t.s.mirror = nil
		t.s.replayTail()
		t.s.mu.Unlock()
	}()
	// ponytail: no SIGWINCH handling while in takeover; add in the real app.
	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}
		if i := strings.IndexByte(string(buf[:n]), 0x1d); i >= 0 {
			t.s.stdin.Write(buf[:i])
			return nil
		}
		t.s.stdin.Write(buf[:n])
	}
}

// sendKey works around x/vt dropping printable keys that carry ModShift
// (upper case, !@#...): printable text goes straight to the remote.
func sendKey(s *session, k tea.KeyPressMsg) {
	if k.Text != "" && k.Mod&^(tea.ModShift|tea.ModAlt) == 0 {
		if k.Mod&tea.ModAlt != 0 {
			s.stdin.Write([]byte("\x1b" + k.Text))
		} else {
			s.stdin.Write([]byte(k.Text))
		}
		return
	}
	s.emu.SendKey(vt.KeyPressEvent(k))
}

type model struct {
	client   *ssh.Client
	tabs     []*session
	active   int
	nextID   int
	w, h     int
	err      error
	renders  int
	quitting bool
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) cur() *session {
	if m.active < len(m.tabs) {
		return m.tabs[m.active]
	}
	return nil
}

func (m *model) newTab() {
	s, err := open(m.client, m.nextID, m.w, m.h-1)
	if err != nil {
		m.err = err
		return
	}
	m.nextID++
	m.tabs = append(m.tabs, s)
	m.active = len(m.tabs) - 1
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if len(m.tabs) == 0 {
			m.newTab()
		}
		for _, s := range m.tabs {
			s.resize(m.w, m.h-1)
		}
	case outputMsg:
		for _, s := range m.tabs {
			s.dirty.Store(false)
		}
	case closedMsg:
		for i, s := range m.tabs {
			if s.id == msg.id {
				m.tabs = append(m.tabs[:i], m.tabs[i+1:]...)
				break
			}
		}
		if len(m.tabs) == 0 {
			return m, tea.Quit
		}
		m.active = min(m.active, len(m.tabs)-1)
	case takeoverDoneMsg:
		if s := m.cur(); s != nil {
			s.resize(m.w, m.h-1)
		}
		m.err = msg.err
	case tea.PasteMsg:
		if s := m.cur(); s != nil {
			s.emu.Paste(msg.Content)
		}
	case tea.MouseMsg:
		if s := m.cur(); s != nil {
			mm := msg.Mouse()
			mm.Y-- // tab bar row
			if mm.Y >= 0 {
				switch msg.(type) {
				case tea.MouseClickMsg:
					s.emu.SendMouse(vt.MouseClick(mm))
				case tea.MouseReleaseMsg:
					s.emu.SendMouse(vt.MouseRelease(mm))
				case tea.MouseWheelMsg:
					s.emu.SendMouse(vt.MouseWheel(mm))
				case tea.MouseMotionMsg:
					s.emu.SendMouse(vt.MouseMotion(mm))
				}
			}
		}
	case tea.KeyPressMsg:
		switch k := msg.String(); {
		case k == "ctrl+q":
			m.quitting = true
			return m, tea.Quit
		case k == "alt+n":
			m.newTab()
			return m, nil
		case k == "ctrl+]":
			if s := m.cur(); s != nil {
				return m, tea.Exec(&takeover{s, m.w, m.h}, func(err error) tea.Msg { return takeoverDoneMsg{err} })
			}
		case len(k) == 5 && strings.HasPrefix(k, "alt+") && k[4] >= '1' && k[4] <= '9':
			if i := int(k[4] - '1'); i < len(m.tabs) {
				m.active = i
			}
			return m, nil
		}
		if s := m.cur(); s != nil {
			sendKey(s, msg)
		}
	}
	return m, nil
}

func (m *model) View() tea.View {
	var b strings.Builder
	for i, s := range m.tabs {
		label := fmt.Sprintf(" %d:dev@test ", s.id+1)
		if i == m.active {
			label = "\x1b[7m" + label + "\x1b[0m"
		}
		b.WriteString(label)
	}
	if m.err != nil {
		fmt.Fprintf(&b, " err: %v", m.err)
	}
	b.WriteString("\x1b[0m\n")
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeAllMotion}
	if m.quitting {
		return v
	}
	if s := m.cur(); s != nil {
		b.WriteString(s.emu.Render())
		pos := s.emu.CursorPosition()
		v.Cursor = tea.NewCursor(pos.X, pos.Y+1)
	}
	v.SetContent(b.String())
	return v
}

func main() {
	client, err := sshx.Dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()
	if p := os.Getenv("PANGEA_PPROF"); p != "" {
		f, _ := os.Create(p)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	prog = tea.NewProgram(&model{client: client})
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
