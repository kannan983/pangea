// Package health checks whether hosts are reachable: a TCP connect to the
// SSH port and reading the server's identification line ("SSH-2.0-...").
// Nothing is sent and no login is attempted.
package health

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

type State int

const (
	Unknown State = iota
	Up
	Slow // up, but the connect took longer than SlowAfter
	Down
)

const SlowAfter = 300 * time.Millisecond

type Result struct {
	State   State
	RTT     time.Duration // TCP connect time
	Banner  string        // e.g. SSH-2.0-OpenSSH_9.6
	Err     string
	Checked time.Time
}

// Check probes addr ("host:port") within timeout.
func Check(ctx context.Context, addr string, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	r := Result{Checked: time.Now()}
	if err != nil {
		r.State, r.Err = Down, short(err)
		return r
	}
	defer conn.Close()
	r.RTT = time.Since(start)
	if dl, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(dl)
	}
	// RFC 4253: the server may send other lines before "SSH-"; cap the read.
	br := bufio.NewReaderSize(&limited{conn, 4096}, 512)
	for {
		line, err := br.ReadString('\n')
		if strings.HasPrefix(line, "SSH-") {
			r.Banner = strings.TrimSpace(line)
			break
		}
		if err != nil {
			r.State, r.Err = Down, "port open, but no SSH greeting"
			return r
		}
	}
	r.State = Up
	if r.RTT > SlowAfter {
		r.State = Slow
	}
	return r
}

type limited struct {
	c net.Conn
	n int
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("greeting too long")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.c.Read(p)
	l.n -= n
	return n, err
}

func short(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "no such host"):
		return "unknown host"
	}
	return err.Error()
}

// CheckAll probes every address with at most workers at a time and calls
// done for each result as it arrives (from other goroutines).
func CheckAll(ctx context.Context, addrs map[string]string, workers int, timeout time.Duration, done func(id string, r Result)) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for id, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			done(id, Check(ctx, addr, timeout))
		}()
	}
	wg.Wait()
}
