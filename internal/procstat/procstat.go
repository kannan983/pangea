// Package procstat reads Pangea's own CPU time and memory use.
package procstat

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CPU is the process's total user+system CPU time so far.
func CPU() time.Duration { return cpu() }

// RSS is resident memory in bytes. ok is false where the OS doesn't expose
// it cheaply; callers then fall back to the Go runtime's view.
func RSS() (bytes uint64, ok bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
