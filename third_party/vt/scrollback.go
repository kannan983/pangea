package vt

import (
	uv "github.com/charmbracelet/ultraviolet"
)

// DefaultScrollbackSize is the default maximum number of lines in the scrollback buffer.
const DefaultScrollbackSize = 10000

// Scrollback represents a scrollback buffer that stores lines scrolled off the screen.
type Scrollback struct {
	// pangea fork: lines are kept rendered (ANSI string) instead of as
	// []uv.Cell. That is ~100x smaller and leaves the GC one pointer per
	// line to scan instead of several per cell.
	lines    []string
	maxLines int
}

// NewScrollback creates a new scrollback buffer with the given maximum number of lines.
func NewScrollback(maxLines int) *Scrollback {
	if maxLines <= 0 {
		maxLines = DefaultScrollbackSize
	}
	return &Scrollback{
		lines:    make([]string, 0, min(maxLines, 1000)), // Pre-allocate reasonable capacity
		maxLines: maxLines,
	}
}

// Push adds a line to the scrollback buffer.
// If the buffer is full, the oldest line is removed.
func (s *Scrollback) Push(line uv.Line) {
	if s == nil || s.maxLines <= 0 {
		return
	}

	// Find last non-empty cell to trim trailing empty cells.
	// This helps with wrapping and window resizing.
	lastNonEmpty := -1
	for i := len(line) - 1; i >= 0; i-- {
		c := &line[i]
		if !c.IsZero() && !c.Equal(&uv.EmptyCell) {
			lastNonEmpty = i
			break
		}
	}

	rendered := line[:lastNonEmpty+1].Render()

	if len(s.lines) >= s.maxLines {
		// Remove oldest line and append new one
		// Reslice instead of shifting: append reallocates (copying only
		// live lines) when capacity runs out, so pushes are amortised O(1).
		s.lines[0] = ""
		s.lines = s.lines[1:]
	}
	s.lines = append(s.lines, rendered)
}

// PushN adds n lines from the buffer starting at line y to the scrollback.
func (s *Scrollback) PushN(buf *uv.RenderBuffer, y, n int) {
	if s == nil || buf == nil || n <= 0 {
		return
	}

	for i := range min(n, buf.Height()-y) {
		if line := buf.Line(y + i); line != nil {
			s.Push(line)
		}
	}
}

// Len returns the number of lines in the scrollback buffer.
func (s *Scrollback) Len() int {
	if s == nil {
		return 0
	}
	return len(s.lines)
}

// MaxLines returns the maximum number of lines the scrollback buffer can hold.
func (s *Scrollback) MaxLines() int {
	if s == nil {
		return 0
	}
	return s.maxLines
}

// SetMaxLines sets the maximum number of lines in the scrollback buffer.
// If the current number of lines exceeds the new maximum, oldest lines are removed.
func (s *Scrollback) SetMaxLines(maxLines int) {
	if s == nil || maxLines <= 0 {
		return
	}

	s.maxLines = maxLines
	if len(s.lines) > maxLines {
		// Remove oldest lines
		s.lines = s.lines[len(s.lines)-maxLines:]
	}
}

// Line returns the rendered line at the given index.
// Index 0 is the oldest line, Len()-1 is the most recent.
// Returns "" if index is out of bounds.
func (s *Scrollback) Line(index int) string {
	if s == nil || index < 0 || index >= len(s.lines) {
		return ""
	}
	return s.lines[index]
}

// Lines returns all rendered lines in the scrollback buffer.
// Index 0 is the oldest line.
func (s *Scrollback) Lines() []string {
	if s == nil {
		return nil
	}
	return s.lines
}

// Clear removes all lines from the scrollback buffer.
func (s *Scrollback) Clear() {
	if s == nil {
		return
	}
	s.lines = s.lines[:0]
}

