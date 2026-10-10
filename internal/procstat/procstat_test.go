package procstat

import (
	"runtime"
	"testing"
	"time"
)

func TestCPUAndRSS(t *testing.T) {
	before := CPU()
	end := time.Now().Add(50 * time.Millisecond)
	for x := 0; time.Now().Before(end); x++ {
		_ = x * x
	}
	if after := CPU(); after <= before {
		t.Fatalf("CPU time did not grow: %v -> %v", before, after)
	}
	if n, ok := RSS(); runtime.GOOS == "linux" && (!ok || n < 1<<20) {
		t.Fatalf("RSS %d %v", n, ok)
	}
}
