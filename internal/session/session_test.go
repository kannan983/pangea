package session

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func TestPlainRows(t *testing.T) {
	e := vt.NewSafeEmulator(10, 3)
	go func() { // drain replies, as Session does
		buf := make([]byte, 64)
		for {
			if _, err := e.Read(buf); err != nil {
				return
			}
		}
	}()
	e.Write([]byte("\x1b[31mred\x1b[0m ok\r\n世界x"))
	rows := plainRows(e)
	if got := strings.TrimRight(rows[0], " "); got != "red ok" {
		t.Fatalf("row 0 %q", got)
	}
	if got := strings.TrimRight(rows[1], " "); got != "世界x" {
		t.Fatalf("row 1 %q", got)
	}
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
}
