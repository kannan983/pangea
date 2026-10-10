// Package sshc dials SSH hosts: auth chain, known_hosts verification and
// prompts through a Prompter.
package sshc

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/kannan983/pangea/internal/store"
)

// Prompter asks the user something while a connection is being set up.
// Implementations return ErrCanceled when the user cancels.
type Prompter interface {
	Secret(prompt string) (string, error)
	Confirm(prompt string) bool
}

// ErrCanceled is returned when the user cancels a prompt.
var ErrCanceled = errors.New("canceled")

// Dial connects to h. knownHostsFile is Pangea's own known_hosts; the user's
// ~/.ssh/known_hosts is consulted read-only as well. saved is the host's
// stored password ("" if none). typed is a password the user entered that
// worked, so the caller can offer to save it.
func Dial(h store.Host, knownHostsFile string, p Prompter, saved string) (c *ssh.Client, typed string, err error) {
	user := h.User
	if user == "" {
		user = os.Getenv("USER")
		if user == "" {
			user = os.Getenv("USERNAME") // Windows
		}
	}
	addr := h.Addr()
	hostKeyCB, algos, err := hostKeyCallback(addr, knownHostsFile, p)
	if err != nil {
		return nil, "", err
	}

	var auths []ssh.AuthMethod
	var closeAgent func()
	if h.KeyPath != "" {
		// An explicit key is used alone, so servers with low MaxAuthTries
		// don't reject us after trying every agent key.
		s, err := loadKey(store.ExpandHome(h.KeyPath), p, true)
		if err != nil {
			return nil, "", err
		}
		auths = append(auths, ssh.PublicKeys(s))
	} else {
		var signers []ssh.Signer
		if ag, done := agentSigners(); ag != nil {
			signers, closeAgent = ag, done
		}
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			// Default keys with a passphrase are skipped: the agent has them
			// if they're in use.
			if s, err := loadKey(filepath.Join(home, ".ssh", name), p, false); err == nil {
				signers = append(signers, s)
			}
		}
		if len(signers) > 0 {
			auths = append(auths, ssh.PublicKeys(signers...))
		}
	}
	if closeAgent != nil {
		defer closeAgent()
	}
	// Passwords: the saved one is tried once, then the user is asked (up to
	// 3 tries). After one canceled prompt nothing asks again.
	canceled, savedUsed, typed := false, false, ""
	ask := func(prompt string) (string, error) {
		if canceled {
			return "", ErrCanceled
		}
		s, err := p.Secret(prompt)
		canceled = errors.Is(err, ErrCanceled)
		return s, err
	}
	password := func(prompt string) (string, error) {
		if saved != "" && !savedUsed {
			savedUsed = true
			return saved, nil
		}
		s, err := ask(prompt)
		if err == nil {
			typed = s
		}
		return s, err
	}
	label := fmt.Sprintf("%s@%s", user, h.Hostname)
	auths = append(auths,
		ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) {
			return password(label + "'s password:")
		}), 3),
		ssh.RetryableAuthMethod(ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i, q := range questions {
				prompt := q
				if instruction != "" {
					prompt = instruction + "\n" + q
				}
				var err error
				if len(questions) == 1 && strings.Contains(strings.ToLower(q), "password") {
					answers[i], err = password(prompt)
				} else {
					answers[i], err = ask(prompt) // OTP etc.: never saved
				}
				if err != nil {
					return nil, err
				}
			}
			return answers, nil
		}), 3),
	)

	// Log what is tried, never secrets.
	slog.Info("dial", "host", h.Label, "addr", addr, "user", user, "key_file", h.KeyPath != "",
		"saved_password", saved != "", "host_key_algos", algos)
	c, err = ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:              user,
		Auth:              auths,
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: algos,
		Timeout:           15 * time.Second,
	})
	if canceled {
		if c != nil {
			c.Close()
		}
		slog.Info("dial canceled", "host", h.Label)
		return nil, "", ErrCanceled
	}
	if err != nil {
		slog.Warn("dial failed", "host", h.Label, "addr", addr, "err", err)
		return nil, "", err
	}
	slog.Info("connected", "host", h.Label, "server", string(c.ServerVersion()), "password_typed", typed != "")
	go keepalive(c)
	if typed == saved {
		typed = ""
	}
	return c, typed, nil
}

func keepalive(c *ssh.Client) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			return
		}
	}
}

func agentSigners() ([]ssh.Signer, func()) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		conn.Close()
		return nil, nil
	}
	return signers, func() { conn.Close() }
}

func loadKey(path string, p Prompter, askPassphrase bool) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) && askPassphrase {
		pass, perr := p.Secret("Passphrase for " + path + ": ")
		if perr != nil {
			return nil, perr
		}
		return ssh.ParsePrivateKeyWithPassphrase(b, []byte(pass))
	}
	return s, err
}

// hostKeyCallback verifies against ~/.ssh/known_hosts and pangea's file.
// Unknown hosts are confirmed by the user and saved to pangea's file;
// changed keys are refused. It also returns the host key algorithms already
// known for addr, so the server offers a key type we can verify.
func hostKeyCallback(addr, ownFile string, p Prompter) (ssh.HostKeyCallback, []string, error) {
	if err := os.MkdirAll(filepath.Dir(ownFile), 0o700); err != nil {
		return nil, nil, err
	}
	if f, err := os.OpenFile(ownFile, os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	files := []string{ownFile}
	if home, err := os.UserHomeDir(); err == nil {
		if user := filepath.Join(home, ".ssh", "known_hosts"); fileExists(user) {
			files = append(files, user)
		}
	}
	known, err := knownhosts.New(files...)
	if err != nil {
		return nil, nil, err
	}

	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := known(hostname, remote, key)
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err // nil (known and matching) or a real error
		}
		if len(ke.Want) > 0 {
			slog.Warn("host key mismatch", "host", hostname, "got", ssh.FingerprintSHA256(key), "known_in", ke.Want[0].Filename)
			return fmt.Errorf("HOST KEY CHANGED for %s (got %s %s): refusing to connect. "+
				"If this is expected, remove the old entry from %s",
				hostname, key.Type(), ssh.FingerprintSHA256(key), ke.Want[0].Filename)
		}
		if !p.Confirm(fmt.Sprintf("Unknown host %s\n%s key fingerprint\n%s\n\nTrust it and save to Pangea's known_hosts?",
			hostname, key.Type(), ssh.FingerprintSHA256(key))) {
			slog.Info("host key rejected by user", "host", hostname, "fingerprint", ssh.FingerprintSHA256(key))
			return errors.New("host key not trusted")
		}
		slog.Info("host key trusted", "host", hostname, "type", key.Type(), "fingerprint", ssh.FingerprintSHA256(key))
		f, err := os.OpenFile(ownFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
		return err
	}
	return cb, knownAlgos(known, addr), nil
}

// knownAlgos asks the callback about a throwaway key; the resulting KeyError
// lists the keys on file for addr.
func knownAlgos(known ssh.HostKeyCallback, addr string) []string {
	pub, _, _ := ed25519.GenerateKey(nil)
	probe, _ := ssh.NewPublicKey(pub)
	var ke *knownhosts.KeyError
	if !errors.As(known(addr, &net.TCPAddr{}, probe), &ke) {
		return nil
	}
	var algos []string
	for _, k := range ke.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, t)
		default:
			algos = append(algos, t)
		}
	}
	return algos
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
