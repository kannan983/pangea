// Pangea is an all-in-one terminal SSH/SFTP client.
//
//	pangea                 start the TUI
//	pangea backup FILE     write an encrypted backup (hosts, settings, passwords, keys)
//	pangea restore FILE    restore a backup (the current config is moved aside first)
package main

import (
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/kannan983/pangea/internal/backup"
	"github.com/kannan983/pangea/internal/store"
	"github.com/kannan983/pangea/internal/tui"
	"github.com/kannan983/pangea/internal/vault"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pangea:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dir, err := store.Dir()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if len(args) != 2 || (args[0] != "backup" && args[0] != "restore") {
			return errors.New("usage: pangea [backup FILE | restore FILE]")
		}
		if args[0] == "backup" {
			return runBackup(dir, args[1])
		}
		return runRestore(dir, args[1])
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	v, warn, err := vault.Open(dir)
	if err != nil {
		return err
	}
	m := tui.New(st, v, warn)
	p := tea.NewProgram(m)
	m.SetProgram(p)
	_, err = p.Run()
	return err
}

func readPass(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func runBackup(dir, file string) error {
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	v, _, err := vault.Open(dir)
	if err != nil {
		return err
	}
	p1, err := readPass("Backup passphrase (needed to restore): ")
	if err != nil {
		return err
	}
	p2, err := readPass("Repeat passphrase: ")
	if err != nil {
		return err
	}
	if p1 != p2 {
		return errors.New("passphrases do not match")
	}
	n, err := backup.Create(file, p1, st, v)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Backup written to %s (%d hosts, %d key files).\n", file, len(st.Hosts), n)
	return nil
}

func runRestore(dir, file string) error {
	pass, err := readPass("Backup passphrase: ")
	if err != nil {
		return err
	}
	moved, err := backup.Restore(file, pass, dir)
	if err != nil {
		return err
	}
	if moved != "" {
		fmt.Fprintf(os.Stderr, "Your previous config was moved to %s\n", moved)
	}
	fmt.Fprintf(os.Stderr, "Restored into %s\n", dir)
	return nil
}
