// Package fsx gives local and SFTP file systems one interface, plus the
// operations Pangea's file browser needs: copying files and folders with
// progress and cancellation, deleting folders, renaming and chmod.
//
// Safety rules, enforced here so every caller gets them:
//   - transfers write to a temporary name and rename it into place, so a
//     failed or canceled transfer never leaves a truncated file behind and
//     overwriting is atomic;
//   - symbolic links are never followed when copying or deleting folders;
//   - names coming from a listing are checked (no "..", no separators), so a
//     hostile server can't make a download escape the target folder.
package fsx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pkg/sftp"
)

// FS is a local or remote file system.
type FS interface {
	Remote() bool
	Join(elem ...string) string
	Base(p string) string
	Dir(p string) string
	ReadDir(dir string) ([]fs.FileInfo, error)
	Stat(p string) (fs.FileInfo, error)
	Lstat(p string) (fs.FileInfo, error)
	Mkdir(p string) error
	Remove(p string) error // a file or an empty folder
	Rename(oldp, newp string) error
	Chmod(p string, mode fs.FileMode) error
	Open(p string) (io.ReadCloser, error)
	Create(p string) (io.WriteCloser, error) // must not exist yet
	replace(tmp, final string) error         // rename tmp over final
}

// ---- local ----

type Local struct{}

func (Local) Remote() bool                           { return false }
func (Local) Join(elem ...string) string             { return filepath.Join(elem...) }
func (Local) Base(p string) string                   { return filepath.Base(p) }
func (Local) Dir(p string) string                    { return filepath.Dir(p) }
func (Local) Stat(p string) (fs.FileInfo, error)     { return os.Stat(p) }
func (Local) Lstat(p string) (fs.FileInfo, error)    { return os.Lstat(p) }
func (Local) Mkdir(p string) error                   { return os.Mkdir(p, 0o755) }
func (Local) Remove(p string) error                  { return os.Remove(p) }
func (Local) Chmod(p string, mode fs.FileMode) error { return os.Chmod(p, mode) }
func (Local) Open(p string) (io.ReadCloser, error)   { return os.Open(p) }
func (Local) replace(tmp, final string) error        { return os.Rename(tmp, final) }
func (Local) Create(p string) (io.WriteCloser, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

// Rename refuses to replace an existing entry (os.Rename would).
func (l Local) Rename(oldp, newp string) error {
	if _, err := os.Lstat(newp); err == nil {
		return fmt.Errorf("%s already exists", filepath.Base(newp))
	}
	return os.Rename(oldp, newp)
}

func (Local) ReadDir(dir string) ([]fs.FileInfo, error) {
	des, err := os.ReadDir(dir)
	out := make([]fs.FileInfo, 0, len(des))
	for _, d := range des {
		if fi, e := d.Info(); e == nil {
			out = append(out, fi)
		}
	}
	return out, err
}

// ---- remote ----

type Remote struct{ C *sftp.Client }

func (Remote) Remote() bool                                { return true }
func (Remote) Join(elem ...string) string                  { return path.Join(elem...) }
func (Remote) Base(p string) string                        { return path.Base(p) }
func (Remote) Dir(p string) string                         { return path.Dir(p) }
func (r Remote) ReadDir(dir string) ([]fs.FileInfo, error) { return r.C.ReadDir(dir) }
func (r Remote) Stat(p string) (fs.FileInfo, error)        { return r.C.Stat(p) }
func (r Remote) Lstat(p string) (fs.FileInfo, error)       { return r.C.Lstat(p) }
func (r Remote) Mkdir(p string) error                      { return r.C.Mkdir(p) }
func (r Remote) Chmod(p string, mode fs.FileMode) error    { return r.C.Chmod(p, mode) }
func (r Remote) Open(p string) (io.ReadCloser, error)      { return r.C.Open(p) }
func (r Remote) Create(p string) (io.WriteCloser, error) {
	return r.C.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
}

// Remove deletes a file, or an empty folder.
func (r Remote) Remove(p string) error {
	fi, err := r.C.Lstat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return r.C.RemoveDirectory(p)
	}
	return r.C.Remove(p)
}

// Rename refuses to replace an existing entry: some SFTP servers overwrite
// on rename, OpenSSH's doesn't, so check instead of trusting the server.
func (r Remote) Rename(oldp, newp string) error {
	if _, err := r.C.Lstat(newp); err == nil {
		return fmt.Errorf("%s already exists", path.Base(newp))
	}
	return r.C.Rename(oldp, newp)
}

// replace prefers the OpenSSH posix-rename extension (atomic overwrite);
// without it, the old file is removed first.
func (r Remote) replace(tmp, final string) error {
	if err := r.C.PosixRename(tmp, final); err == nil {
		return nil
	}
	if err := r.C.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return r.C.Rename(tmp, final)
}

// ---- names and modes ----

// SafeName reports whether name is a plain entry name: not empty, not "."
// or "..", no path separators or NUL.
func SafeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// ValidNewName checks a name typed for rename / new folder.
func ValidNewName(name string) error {
	if !SafeName(name) {
		return fmt.Errorf("%q is not a valid name (no /, \\, . or ..)", name)
	}
	return nil
}

// ParseMode reads an octal permission string like "644" or "0755".
func ParseMode(s string) (fs.FileMode, error) {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 4 {
		return 0, fmt.Errorf("permissions must be 3 or 4 octal digits, like 644 or 0755")
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o7777 {
		return 0, fmt.Errorf("%q is not an octal mode", s)
	}
	m := fs.FileMode(n & 0o777)
	if n&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if n&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if n&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m, nil
}

// Octal formats a mode the way ParseMode reads it.
func Octal(m fs.FileMode) string {
	n := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		n |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		n |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		n |= 0o1000
	}
	return fmt.Sprintf("%03o", n)
}

// FreeName returns name, or "name (1).ext", "name (2).ext"... whichever
// doesn't exist in dir.
func FreeName(f FS, dir, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if inner := path.Ext(stem); inner == ".tar" { // archive.tar.gz -> archive (1).tar.gz
		stem, ext = strings.TrimSuffix(stem, inner), inner+ext
	}
	if strings.HasPrefix(name, ".") && strings.Count(name, ".") == 1 {
		stem, ext = name, "" // dotfiles like .bashrc
	}
	cand := name
	for i := 1; i < 10000; i++ {
		if _, err := f.Lstat(f.Join(dir, cand)); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
		cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return "", fmt.Errorf("no free name for %s", name)
}

// ---- delete ----

// RemoveAll deletes p; folders are emptied first. Symlinks are removed as
// links, never followed.
func RemoveAll(f FS, p string) error {
	fi, err := f.Lstat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		ents, err := f.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !SafeName(e.Name()) {
				return fmt.Errorf("refusing unsafe name %q in %s", e.Name(), p)
			}
			if err := RemoveAll(f, f.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	return f.Remove(p)
}

// ---- copy ----

// Conflict decides what happens when the destination already exists.
type Conflict int

const (
	Overwrite Conflict = iota // replace files; merge into existing folders
	KeepBoth                  // copy under a free "name (1)" name
)

// Size totals the bytes and files under p (symlinks skipped).
func Size(f FS, p string) (bytes int64, files int, err error) {
	fi, err := f.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return 0, 0, nil
	case !fi.IsDir():
		return fi.Size(), 1, nil
	}
	ents, err := f.ReadDir(p)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if !SafeName(e.Name()) {
			return 0, 0, fmt.Errorf("refusing unsafe name %q in %s", e.Name(), p)
		}
		b, n, err := Size(f, f.Join(p, e.Name()))
		if err != nil {
			return 0, 0, err
		}
		bytes, files = bytes+b, files+n
	}
	return bytes, files, nil
}

// Progress is updated while copying; read it from any goroutine.
type Progress struct {
	Bytes   atomic.Int64
	Files   atomic.Int64
	Skipped atomic.Int64 // symlinks and other special files
}

// Copy copies src (file or folder) to dst. If dst exists, c decides; it
// returns the destination path actually used.
func Copy(ctx context.Context, sf FS, src string, df FS, dst string, c Conflict, pr *Progress) (string, error) {
	if c == KeepBoth {
		if _, err := df.Lstat(dst); err == nil {
			name, err := FreeName(df, df.Dir(dst), df.Base(dst))
			if err != nil {
				return "", err
			}
			dst = df.Join(df.Dir(dst), name)
		}
	}
	return dst, copyAny(ctx, sf, src, df, dst, pr)
}

func copyAny(ctx context.Context, sf FS, src string, df FS, dst string, pr *Progress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fi, err := sf.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0 || !(fi.IsDir() || fi.Mode().IsRegular()):
		pr.Skipped.Add(1)
		return nil
	case fi.Mode().IsRegular():
		return copyFile(ctx, sf, src, df, dst, fi, pr)
	}
	if dfi, err := df.Lstat(dst); err == nil {
		if !dfi.IsDir() {
			return fmt.Errorf("%s exists and is not a folder", dst)
		}
	} else if err := df.Mkdir(dst); err != nil {
		return err
	}
	ents, err := sf.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !SafeName(e.Name()) {
			return fmt.Errorf("refusing unsafe name %q in %s", e.Name(), src)
		}
		if err := copyAny(ctx, sf, sf.Join(src, e.Name()), df, df.Join(dst, e.Name()), pr); err != nil {
			return err
		}
	}
	return nil
}

func tmpName(df FS, dst string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return df.Join(df.Dir(dst), "."+df.Base(dst)+".pangea-"+hex.EncodeToString(b))
}

// copyFile writes to a temp file next to dst and renames it into place.
func copyFile(ctx context.Context, sf FS, src string, df FS, dst string, fi fs.FileInfo, pr *Progress) (err error) {
	if dfi, err := df.Lstat(dst); err == nil && dfi.IsDir() {
		return fmt.Errorf("%s exists and is a folder", dst)
	}
	r, err := sf.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()
	tmp := tmpName(df, dst)
	w, err := df.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			w.Close()
			df.Remove(tmp)
		}
	}()
	switch {
	case sf.Remote() && df.Remote():
		// Server to server, relayed: a pipe lets the source's pipelined
		// WriteTo and the destination's pipelined ReadFrom run together.
		err = relay(ctx, r, w, fi.Size(), pr)
	case sf.Remote():
		// SFTP source: its WriteTo pipelines reads. (Not "does r implement
		// io.WriterTo": *os.File does too, and would bypass the pipelined
		// ReadFrom of an SFTP destination.)
		_, err = io.Copy(&ctxWriter{ctx, w, pr}, r)
	default:
		// The size lets an SFTP destination's ReadFrom pipeline writes.
		_, err = io.Copy(w, &io.LimitedReader{R: &ctxReader{ctx, r, pr}, N: fi.Size()})
	}
	if err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	df.Chmod(tmp, fi.Mode().Perm()) // best effort: keep e.g. executable bits
	if err = df.replace(tmp, dst); err != nil {
		return err
	}
	pr.Files.Add(1)
	return nil
}

// relay copies r to w through a pipe so both ends stream concurrently.
func relay(ctx context.Context, r io.Reader, w io.Writer, size int64, pr *Progress) error {
	pipeR, pipeW := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(&ctxWriter{ctx, pipeW, pr}, r)
		pipeW.CloseWithError(err) // nil err = EOF for the reader
		errc <- err
	}()
	_, werr := io.Copy(w, &io.LimitedReader{R: pipeR, N: size})
	pipeR.CloseWithError(werr) // unblock the reading side if writing failed
	if rerr := <-errc; rerr != nil && !errors.Is(rerr, io.ErrClosedPipe) {
		return rerr
	}
	return werr
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
	pr  *Progress
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	c.pr.Bytes.Add(int64(n))
	return n, err
}

type ctxWriter struct {
	ctx context.Context
	w   io.Writer
	pr  *Progress
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	c.pr.Bytes.Add(int64(n))
	return n, err
}
