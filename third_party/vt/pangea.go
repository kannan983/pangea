package vt

import "github.com/charmbracelet/x/ansi"

// pangea fork additions.

// MouseTracking reports whether the application enabled any mouse mode.
func (e *Emulator) MouseTracking() bool {
	for _, m := range []ansi.DECMode{ansi.ModeMouseX10, ansi.ModeMouseNormal,
		ansi.ModeMouseHighlight, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent} {
		if e.isModeSet(m) {
			return true
		}
	}
	return false
}

// CursorVisible reports whether the application shows the cursor.
func (e *Emulator) CursorVisible() bool { return !e.scr.cur.Hidden }

func (se *SafeEmulator) MouseTracking() bool {
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.Emulator.MouseTracking()
}

func (se *SafeEmulator) CursorVisible() bool {
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.Emulator.CursorVisible()
}

// ScrollbackRange copies rendered scrollback lines [from, to) under the lock.
func (se *SafeEmulator) ScrollbackRange(from, to int) []string {
	se.mu.RLock()
	defer se.mu.RUnlock()
	sb := se.Emulator.Scrollback()
	from, to = max(0, from), min(to, sb.Len())
	if from >= to {
		return nil
	}
	return append([]string(nil), sb.Lines()[from:to]...)
}
