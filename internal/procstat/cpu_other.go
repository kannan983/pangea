//go:build !unix && !windows

package procstat

import "time"

func cpu() time.Duration { return 0 }
