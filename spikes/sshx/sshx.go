// Package sshx dials the spike test server (spikes/testenv).
package sshx

import (
	"os"

	"golang.org/x/crypto/ssh"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Dial() (*ssh.Client, error) {
	key, err := os.ReadFile(env("PANGEA_KEY", "spikes/testenv/id_ed25519"))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", env("PANGEA_ADDR", "127.0.0.1:2222"), &ssh.ClientConfig{
		User: env("PANGEA_USER", "dev"),
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// ponytail: spike only; the real client verifies via knownhosts.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
}
