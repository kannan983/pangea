// Package backup writes and restores a single passphrase-encrypted file with
// everything Pangea needs on another machine: hosts, settings, known_hosts,
// saved passwords and the private keys hosts refer to.
//
// Format: magic line, 16-byte scrypt salt, then vault.Seal of a tar.gz.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/kannan983/pangea/internal/store"
	"github.com/kannan983/pangea/internal/vault"
)

const (
	magic   = "PANGEA-BACKUP-1\n"
	keysDir = "{pangea}/keys/" // KeyPath prefix inside a backup; replaced on restore
)

func deriveKey(pass string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
}

// Create writes the backup to dst (never overwriting) and returns how many
// key files it contains.
func Create(dst, pass string, st *store.Store, v *vault.Vault) (int, error) {
	if pass == "" {
		return 0, errors.New("passphrase is empty")
	}
	files := map[string][]byte{}
	hosts := append([]store.Host(nil), st.Hosts...)
	keyNames := map[string]string{} // source path -> name in backup
	for i, h := range hosts {
		if h.KeyPath == "" {
			continue
		}
		src := store.ExpandHome(h.KeyPath)
		name, ok := keyNames[src]
		if !ok {
			b, err := os.ReadFile(src)
			if err != nil {
				return 0, fmt.Errorf("host %s: key file: %w", h.Label, err)
			}
			name = uniqueName(filepath.Base(src), files)
			files["keys/"+name] = b
			keyNames[src] = name
		}
		hosts[i].KeyPath = keysDir + name
	}
	var err error
	if files["hosts.json"], err = json.MarshalIndent(hosts, "", "  "); err != nil {
		return 0, err
	}
	if files["settings.json"], err = json.MarshalIndent(st.Settings, "", "  "); err != nil {
		return 0, err
	}
	if files["passwords.json"], err = json.Marshal(v.All()); err != nil {
		return 0, err
	}
	if b, err := os.ReadFile(filepath.Join(st.Dir, "known_hosts")); err == nil {
		files["known_hosts"] = b
	}

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, err
		}
		if _, err := tw.Write(b); err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}

	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := deriveKey(pass, salt)
	if err != nil {
		return 0, err
	}
	sealed, err := vault.Seal(key, archive.Bytes())
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	out := append(append([]byte(magic), salt...), sealed...)
	if _, err := f.Write(out); err != nil {
		f.Close()
		return 0, err
	}
	return len(keyNames), f.Close()
}

func uniqueName(name string, files map[string][]byte) string {
	n := name
	for i := 2; files["keys/"+n] != nil; i++ {
		n = fmt.Sprintf("%s-%d", name, i)
	}
	return n
}

// Restore unpacks src into dir. An existing dir is first moved aside to
// dir.bak-<time>, which is returned so nothing is ever overwritten.
func Restore(src, pass, dir string) (moved string, err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(data, []byte(magic)) || len(data) < len(magic)+16 {
		return "", errors.New("not a Pangea backup file")
	}
	salt := data[len(magic) : len(magic)+16]
	key, err := deriveKey(pass, salt)
	if err != nil {
		return "", err
	}
	archive, err := vault.Unseal(key, data[len(magic)+16:])
	if err != nil {
		return "", errors.New("wrong passphrase or damaged backup")
	}
	files, err := untar(archive)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(dir); err == nil {
		moved = dir + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dir, moved); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		return moved, err
	}
	for name, b := range files {
		switch {
		case name == "passwords.json":
			continue
		case name == "hosts.json":
			b = []byte(strings.ReplaceAll(string(b), keysDir, filepath.ToSlash(filepath.Join(dir, "keys"))+"/"))
		case strings.HasPrefix(name, "keys/"):
		case name == "settings.json" || name == "known_hosts":
		default:
			continue // ignore anything unexpected
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(filepath.Separator)) {
			return moved, fmt.Errorf("bad path in backup: %s", name)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return moved, err
		}
	}

	var pw map[string]string
	if err := json.Unmarshal(files["passwords.json"], &pw); err != nil {
		return moved, fmt.Errorf("passwords: %w", err)
	}
	v, _, err := vault.Open(dir)
	if err != nil {
		return moved, err
	}
	v.Replace(pw)
	return moved, v.Save()
}

func untar(archive []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		files[filepath.ToSlash(filepath.Clean(hdr.Name))] = b
	}
}
