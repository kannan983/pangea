package fsx

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// remoteFS serves dir over an in-process SFTP server.
func remoteFS(t *testing.T, dir string) Remote {
	t.Helper()
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, sftp.WithServerWorkingDirectory(dir))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := sftp.NewClientPipe(cr, cw, sftp.UseConcurrentWrites(true), sftp.UseConcurrentReads(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); c.Close() }) // server first: unblocks the client reader
	return Remote{c}
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if strings.Contains(d.Name(), ".pangea-") {
			t.Errorf("leftover temp file %s", p)
		}
		return nil
	})
}

func TestUploadDownloadOverwrite(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	r := remoteFS(t, remote)
	data := make([]byte, 3<<20+123)
	rand.Read(data)
	write(t, filepath.Join(local, "a.bin"), data)
	os.Chmod(filepath.Join(local, "a.bin"), 0o750)

	var pr Progress
	got, err := Copy(context.Background(), Local{}, filepath.Join(local, "a.bin"), r, filepath.Join(remote, "a.bin"), Overwrite, &pr)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, data) {
		t.Fatal("upload content differs")
	}
	if pr.Bytes.Load() != int64(len(data)) || pr.Files.Load() != 1 {
		t.Fatalf("progress %d bytes %d files", pr.Bytes.Load(), pr.Files.Load())
	}
	if fi, _ := os.Stat(got); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o750 {
		t.Fatalf("mode not kept: %v", fi.Mode().Perm())
	}

	// Overwrite an existing remote file atomically.
	write(t, filepath.Join(local, "a.bin"), []byte("new"))
	if _, err := Copy(context.Background(), Local{}, filepath.Join(local, "a.bin"), r, filepath.Join(remote, "a.bin"), Overwrite, &Progress{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "a.bin")); string(b) != "new" {
		t.Fatalf("overwrite: %q", b)
	}

	// Download, keeping both.
	write(t, filepath.Join(local, "a.bin"), []byte("local"))
	got, err = Copy(context.Background(), r, filepath.Join(remote, "a.bin"), Local{}, filepath.Join(local, "a.bin"), KeepBoth, &Progress{})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "a (1).bin" {
		t.Fatalf("keep both named %s", got)
	}
	if b, _ := os.ReadFile(got); string(b) != "new" {
		t.Fatal("download content differs")
	}
	if b, _ := os.ReadFile(filepath.Join(local, "a.bin")); string(b) != "local" {
		t.Fatal("keep both must not touch the original")
	}
	noTemps(t, local)
	noTemps(t, remote)
}

func TestCancelLeavesNothing(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	r := remoteFS(t, remote)
	write(t, filepath.Join(local, "big"), make([]byte, 8<<20))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Copy(ctx, Local{}, filepath.Join(local, "big"), r, filepath.Join(remote, "big"), Overwrite, &Progress{}); err == nil {
		t.Fatal("canceled copy must fail")
	}
	if _, err := os.Stat(filepath.Join(remote, "big")); err == nil {
		t.Fatal("canceled copy created the destination")
	}
	noTemps(t, remote)
}

func TestCopyTreeSkipsSymlinks(t *testing.T) {
	local, remote, outside := t.TempDir(), t.TempDir(), t.TempDir()
	r := remoteFS(t, remote)
	write(t, filepath.Join(local, "app", "x.txt"), []byte("x"))
	write(t, filepath.Join(local, "app", "sub", "y.txt"), []byte("yy"))
	write(t, filepath.Join(outside, "secret"), []byte("s"))
	if err := os.Symlink(outside, filepath.Join(local, "app", "link")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	b, n, err := Size(Local{}, filepath.Join(local, "app"))
	if err != nil || b != 3 || n != 2 {
		t.Fatalf("size %d %d %v", b, n, err)
	}
	var pr Progress
	if _, err := Copy(context.Background(), Local{}, filepath.Join(local, "app"), r, filepath.Join(remote, "app"), Overwrite, &pr); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(filepath.Join(remote, "app", "sub", "y.txt")); string(c) != "yy" {
		t.Fatal("nested file not copied")
	}
	if _, err := os.Lstat(filepath.Join(remote, "app", "link")); err == nil {
		t.Fatal("symlink must be skipped")
	}
	if pr.Files.Load() != 2 || pr.Skipped.Load() != 1 {
		t.Fatalf("files %d skipped %d", pr.Files.Load(), pr.Skipped.Load())
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	for _, f := range []struct {
		name string
		fs   func(t *testing.T, dir string) FS
	}{
		{"local", func(*testing.T, string) FS { return Local{} }},
		{"remote", func(t *testing.T, dir string) FS { return remoteFS(t, dir) }},
	} {
		t.Run(f.name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			write(t, filepath.Join(outside, "keep"), []byte("k"))
			write(t, filepath.Join(dir, "d", "f"), []byte("f"))
			if err := os.Symlink(outside, filepath.Join(dir, "d", "link")); err != nil {
				t.Skip(err)
			}
			if err := RemoveAll(f.fs(t, dir), filepath.Join(dir, "d")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "d")); err == nil {
				t.Fatal("folder not removed")
			}
			if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
				t.Fatal("followed a symlink and deleted outside data")
			}
		})
	}
}

func TestRenameRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	r := remoteFS(t, dir)
	write(t, filepath.Join(dir, "a"), []byte("a"))
	write(t, filepath.Join(dir, "b"), []byte("b"))
	for _, f := range []FS{Local{}, r} {
		if err := f.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err == nil {
			t.Fatalf("%T: rename over an existing file must fail", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b")); string(b) != "b" {
		t.Fatal("existing file was replaced")
	}
}

func TestNamesAndModes(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "x\x00"} {
		if SafeName(bad) || ValidNewName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !SafeName("ok name.txt") || !SafeName("..hidden") {
		t.Error("valid names rejected")
	}
	for in, want := range map[string]string{"644": "644", "0755": "755", "4755": "4755", "1777": "1777"} {
		m, err := ParseMode(in)
		if err != nil || Octal(m) != want {
			t.Errorf("ParseMode(%q) = %v %v, Octal %s", in, m, err, Octal(m))
		}
	}
	for _, bad := range []string{"8", "abc", "17777", "999", "75"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) accepted", bad)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".bashrc"), nil)
	if n, _ := FreeName(Local{}, dir, ".bashrc"); n != ".bashrc (1)" {
		t.Errorf("dotfile free name %q", n)
	}
	write(t, filepath.Join(dir, "app.tar.gz"), nil)
	if n, _ := FreeName(Local{}, dir, "app.tar.gz"); n != "app (1).tar.gz" {
		t.Errorf("tar.gz free name %q", n)
	}
}

func TestServerToServer(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ra, rb := remoteFS(t, a), remoteFS(t, b)
	data := make([]byte, 5<<20+7)
	rand.Read(data)
	write(t, filepath.Join(a, "dir", "big.bin"), data)
	write(t, filepath.Join(a, "dir", "small.txt"), []byte("hi"))
	var pr Progress
	if _, err := Copy(context.Background(), ra, filepath.Join(a, "dir"), rb, filepath.Join(b, "dir"), Overwrite, &pr); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "dir", "big.bin")); !bytes.Equal(got, data) {
		t.Fatal("server-to-server content differs")
	}
	if pr.Files.Load() != 2 || pr.Bytes.Load() != int64(len(data)+2) {
		t.Fatalf("progress %d files %d bytes", pr.Files.Load(), pr.Bytes.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Copy(ctx, ra, filepath.Join(a, "dir", "big.bin"), rb, filepath.Join(b, "new.bin"), Overwrite, &Progress{}); err == nil {
		t.Fatal("canceled server-to-server copy must fail")
	}
	if _, err := os.Stat(filepath.Join(b, "new.bin")); err == nil {
		t.Fatal("canceled copy created the file")
	}
	noTemps(t, b)
}

func TestRelayCancelMidway(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ra, rb := remoteFS(t, a), remoteFS(t, b)
	write(t, filepath.Join(a, "big"), make([]byte, 64<<20))
	ctx, cancel := context.WithCancel(context.Background())
	var pr Progress
	go func() {
		for pr.Bytes.Load() < 1<<20 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := Copy(ctx, ra, filepath.Join(a, "big"), rb, filepath.Join(b, "big"), Overwrite, &pr); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, "big")); err == nil {
		t.Fatal("partial file left as the destination")
	}
	noTemps(t, b)
}
