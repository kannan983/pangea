// Package session runs remote shells rendered by a headless terminal
// emulator, so the TUI can draw them inside a tab. Attach hands a session
// the real terminal (fullscreen); its output then bypasses the emulator.
package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/muesli/cancelreader"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

const tailMax = 1 << 20

type Session struct {
	Client *ssh.Client
	Start  time.Time

	sess  *ssh.Session
	stdin io.Writer
	done  chan struct{}
	emu   *vt.SafeEmulator
	dirty atomic.Bool

	mu        sync.Mutex
	mirror    io.Writer // real terminal while attached
	tail      []byte    // raw output seen while attached
	overflow  bool
	replaying atomic.Bool
	closed    atomic.Bool
}

// Open starts a login shell on client with a w×h PTY.
// onOutput runs (from another goroutine) when new output arrives and the
// previous notification was acknowledged with Rendered. onClose runs when
// the remote side ends the session.
func Open(client *ssh.Client, w, h int, onOutput, onClose func(*Session)) (*Session, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	// Not the local $TERM: servers rarely have terminfo for xterm-ghostty
	// etc., and the emulator speaks xterm.
	if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
		sess.Close()
		return nil, err
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, err
	}
	s := &Session{Client: client, Start: time.Now(), sess: sess, stdin: stdin,
		done: make(chan struct{}), emu: vt.NewSafeEmulator(w, h)}
	go s.replies()
	go s.pump(stdout, onOutput, onClose)
	return s, nil
}

// replies forwards what the emulator sends (encoded keys, mouse, answers to
// terminal queries) to the remote, except while replaying old output.
func (s *Session) replies() {
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if err != nil {
			return
		}
		if !s.replaying.Load() {
			s.stdin.Write(buf[:n])
		}
	}
}

func (s *Session) pump(r io.Reader, onOutput, onClose func(*Session)) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.mu.Lock()
			if s.mirror != nil {
				s.mirror.Write(buf[:n])
				s.keepTail(buf[:n])
			} else {
				s.emu.Write(buf[:n])
			}
			s.mu.Unlock()
			if !s.dirty.Swap(true) && onOutput != nil {
				onOutput(s)
			}
		}
		if err != nil {
			s.closed.Store(true)
			close(s.done)
			s.sess.Close()
			if onClose != nil {
				onClose(s)
			}
			return
		}
	}
}

// Rendered acknowledges an onOutput notification; the next output will
// notify again. This caps notifications at the UI's frame rate.
func (s *Session) Rendered() { s.dirty.Store(false) }

func (s *Session) Closed() bool { return s.closed.Load() }

// Close ends the remote shell (the SSH connection stays open).
func (s *Session) Close() { s.sess.Close() }

// ---- input ----

func (s *Session) Write(b []byte) { s.stdin.Write(b) }

func (s *Session) SendKey(k vt.KeyPressEvent) { s.emu.SendKey(k) }

func (s *Session) SendMouse(m vt.Mouse) { s.emu.SendMouse(m) }

// Paste sends text, bracketed if the remote application asked for it.
func (s *Session) Paste(text string) { s.emu.Paste(text) }

// ---- screen ----

func (s *Session) Render() string { return s.emu.Render() }

func (s *Session) Cursor() (x, y int, visible bool) {
	p := s.emu.CursorPosition()
	return p.X, p.Y, s.emu.CursorVisible()
}

func (s *Session) MouseTracking() bool { return s.emu.MouseTracking() }

func (s *Session) AltScreen() bool { return s.emu.IsAltScreen() }

func (s *Session) ScrollbackLen() int { return s.emu.ScrollbackLen() }

func (s *Session) Scrollback(from, to int) []string { return s.emu.ScrollbackRange(from, to) }

// Resize keeps PTY and emulator in step. When shrinking past the cursor,
// content scrolls up like xterm instead of being cropped (x/vt crops, and
// the shell then overwrites the last output line).
func (s *Session) Resize(w, h int) {
	s.mu.Lock()
	if s.emu.Width() != w || s.emu.Height() != h {
		if k := s.emu.CursorPosition().Y - h + 1; k > 0 {
			s.emu.Write(fmt.Appendf(nil, "\x1b7\x1b[%d;1H%s\x1b8\x1b[%dA", s.emu.Height(), strings.Repeat("\n", k), k))
		}
		s.emu.Resize(w, h)
	}
	s.mu.Unlock()
	s.sess.WindowChange(h, w)
}

// ---- fullscreen ----

func (s *Session) keepTail(b []byte) {
	s.tail = append(s.tail, b...)
	if len(s.tail) > 2*tailMax {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-tailMax:]...)
		s.overflow = true
	}
}

// replayTail feeds output seen while attached into the emulator, with its
// replies to old terminal queries suppressed. Called with mu held.
// ponytail: a cut tail can start mid escape sequence; the redraw that
// full-screen apps do on resize hides it. Shell output older than the tail
// is missing from the emulator's scrollback.
func (s *Session) replayTail() {
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

// ErrClosed is returned by Attach when the remote ended the session.
var ErrClosed = errors.New("session ended")

// detach keys: F11 (xterm and kitty encodings) and ctrl+].
var detachSeqs = [][]byte{[]byte("\x1b[23~"), []byte("\x1b[23u"), {0x1d}, []byte("\x1b[93;5u")}

// Attach gives the session the whole terminal, with output passed straight
// through (OpenSSH speed), until F11 or ctrl+] (returns nil) or the remote
// side ends (returns ErrClosed).
func (s *Session) Attach(in, out *os.File) error {
	if s.Closed() {
		return ErrClosed
	}
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)

	w, h, err := term.GetSize(int(out.Fd()))
	if err == nil {
		s.Resize(w, h)
	}
	s.mu.Lock()
	p := s.emu.CursorPosition()
	fmt.Fprintf(out, "\x1b[H\x1b[2J%s\x1b[%d;%dH", strings.ReplaceAll(s.emu.Render(), "\n", "\r\n"), p.Y+1, p.X+1)
	s.mirror = out
	s.mu.Unlock()

	stop := make(chan struct{})
	defer func() {
		close(stop)
		s.mu.Lock()
		s.mirror = nil
		s.replayTail()
		s.mu.Unlock()
	}()
	// ponytail: polls the size instead of SIGWINCH so Windows works too.
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if nw, nh, err := term.GetSize(int(out.Fd())); err == nil && (nw != w || nh != h) {
					w, h = nw, nh
					s.Resize(w, h)
				}
			}
		}
	}()

	cr, err := cancelreader.NewReader(in)
	if err != nil {
		return err
	}
	defer cr.Close()
	go func() {
		select {
		case <-s.done:
			cr.Cancel()
		case <-stop:
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := cr.Read(buf)
		if err != nil {
			if s.Closed() {
				return ErrClosed
			}
			return err
		}
		for _, seq := range detachSeqs {
			if i := bytes.Index(buf[:n], seq); i >= 0 {
				s.stdin.Write(buf[:i])
				return nil
			}
		}
		if _, err := s.stdin.Write(buf[:n]); err != nil {
			return ErrClosed
		}
	}
}
