package health

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// serve accepts connections and runs fn on each.
func serve(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { fn(c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestCheck(t *testing.T) {
	ssh := serve(t, func(c net.Conn) { c.Write([]byte("hello\r\nSSH-2.0-OpenSSH_9.9\r\n")) })
	silent := serve(t, func(c net.Conn) { time.Sleep(2 * time.Second) })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()

	r := Check(context.Background(), ssh, time.Second)
	if r.State != Up || r.Banner != "SSH-2.0-OpenSSH_9.9" {
		t.Fatalf("ssh: %+v", r)
	}
	r = Check(context.Background(), silent, 300*time.Millisecond)
	if r.State != Down || r.Err != "port open, but no SSH greeting" {
		t.Fatalf("silent: %+v", r)
	}
	if r = Check(context.Background(), closed, time.Second); r.State != Down || r.Err != "connection refused" {
		t.Fatalf("closed: %+v", r)
	}
}

func TestCheckAll(t *testing.T) {
	ssh := serve(t, func(c net.Conn) { c.Write([]byte("SSH-2.0-x\r\n")) })
	var mu sync.Mutex
	got := map[string]State{}
	CheckAll(context.Background(), map[string]string{"a": ssh, "b": ssh, "c": "127.0.0.1:1"}, 2, time.Second,
		func(id string, r Result) { mu.Lock(); got[id] = r.State; mu.Unlock() })
	if got["a"] != Up || got["b"] != Up || got["c"] != Down {
		t.Fatalf("%v", got)
	}
}
