// Package store keeps Pangea's hosts and settings in JSON files under the
// user config dir.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Host struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Mode      string    `json:"mode,omitempty"`
	Group     string    `json:"group,omitempty"` // pre-modes files; migrated to Mode on Open
	Tags      []string  `json:"tags,omitempty"`
	Hostname  string    `json:"hostname"`
	Port      int       `json:"port,omitempty"`
	User      string    `json:"user,omitempty"`
	KeyPath   string    `json:"key_path,omitempty"`
	LocalDir  string    `json:"local_dir,omitempty"`  // SFTP start folder on this machine
	RemoteDir string    `json:"remote_dir,omitempty"` // SFTP start folder on the host
	LastUsed  time.Time `json:"last_used,omitzero"`
	Uses      int       `json:"uses,omitempty"`
}

// Addr is host:port with the default port filled in.
func (h Host) Addr() string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", h.Hostname, port)
}

type Settings struct {
	Modes       []string `json:"modes"`
	DefaultMode string   `json:"default_mode"`
	LocalDir    string   `json:"local_dir,omitempty"` // SFTP default local folder
	GridView    bool     `json:"grid_view,omitempty"` // hosts as cards instead of a list
}

type Store struct {
	Dir      string
	Hosts    []Host
	Settings Settings
}

// Dir returns the config dir: $XDG_CONFIG_HOME/pangea, %AppData%\pangea, ...
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "pangea"), nil
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir}
	if err := readJSON(s.hostsPath(), &s.Hosts); err != nil {
		return nil, err
	}
	if err := readJSON(s.settingsPath(), &s.Settings); err != nil {
		return nil, err
	}
	s.normalize()
	return s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// normalize migrates groups to modes and makes sure every host has a mode
// that exists.
func (s *Store) normalize() {
	for i := range s.Hosts {
		if h := &s.Hosts[i]; h.Mode == "" && h.Group != "" {
			h.Mode, h.Group = h.Group, ""
		}
	}
	for _, h := range s.Hosts {
		if h.Mode != "" && !slices.Contains(s.Settings.Modes, h.Mode) {
			s.Settings.Modes = append(s.Settings.Modes, h.Mode)
		}
	}
	if len(s.Settings.Modes) == 0 {
		s.Settings.Modes = []string{"Office", "Personal"}
	}
	if !slices.Contains(s.Settings.Modes, s.Settings.DefaultMode) {
		s.Settings.DefaultMode = s.Settings.Modes[0]
	}
	for i := range s.Hosts {
		if s.Hosts[i].Mode == "" {
			s.Hosts[i].Mode = s.Settings.DefaultMode
		}
	}
}

func (s *Store) hostsPath() string    { return filepath.Join(s.Dir, "hosts.json") }
func (s *Store) settingsPath() string { return filepath.Join(s.Dir, "settings.json") }

// Save writes both files atomically so a crash never leaves a truncated one.
func (s *Store) Save() error {
	if err := WriteJSON(s.hostsPath(), s.Hosts); err != nil {
		return err
	}
	return WriteJSON(s.settingsPath(), s.Settings)
}

// WriteJSON writes v to path atomically with owner-only permissions.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Put inserts or replaces (by ID) a host and returns its ID.
func (s *Store) Put(h Host) string {
	if h.ID == "" {
		h.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if h.Mode != "" && !slices.Contains(s.Settings.Modes, h.Mode) {
		s.Settings.Modes = append(s.Settings.Modes, h.Mode)
	}
	for i := range s.Hosts {
		if s.Hosts[i].ID == h.ID {
			s.Hosts[i] = h
			return h.ID
		}
	}
	s.Hosts = append(s.Hosts, h)
	return h.ID
}

func (s *Store) Delete(id string) {
	for i := range s.Hosts {
		if s.Hosts[i].ID == id {
			s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
			return
		}
	}
}

// Touch records a connection, for most-used-first ordering.
func (s *Store) Touch(id string) {
	for i := range s.Hosts {
		if s.Hosts[i].ID == id {
			s.Hosts[i].Uses++
			s.Hosts[i].LastUsed = time.Now()
		}
	}
}

// ModeCount returns how many hosts are in mode.
func (s *Store) ModeCount(mode string) int {
	n := 0
	for _, h := range s.Hosts {
		if h.Mode == mode {
			n++
		}
	}
	return n
}

// AddMode adds a mode if it doesn't exist yet.
func (s *Store) AddMode(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("mode name is empty")
	}
	if slices.Contains(s.Settings.Modes, name) {
		return fmt.Errorf("mode %q already exists", name)
	}
	s.Settings.Modes = append(s.Settings.Modes, name)
	return nil
}

// RenameMode renames a mode and moves its hosts along.
func (s *Store) RenameMode(old, name string) error {
	name = strings.TrimSpace(name)
	i := slices.Index(s.Settings.Modes, old)
	switch {
	case i < 0:
		return fmt.Errorf("no mode %q", old)
	case name == "":
		return errors.New("mode name is empty")
	case name != old && slices.Contains(s.Settings.Modes, name):
		return fmt.Errorf("mode %q already exists", name)
	}
	s.Settings.Modes[i] = name
	if s.Settings.DefaultMode == old {
		s.Settings.DefaultMode = name
	}
	for j := range s.Hosts {
		if s.Hosts[j].Mode == old {
			s.Hosts[j].Mode = name
		}
	}
	return nil
}

// DeleteMode removes an empty mode; the last mode can't be deleted.
func (s *Store) DeleteMode(name string) error {
	if n := s.ModeCount(name); n > 0 {
		return fmt.Errorf("mode %q still has %d host(s)", name, n)
	}
	if len(s.Settings.Modes) == 1 {
		return errors.New("can't delete the last mode")
	}
	s.Settings.Modes = slices.DeleteFunc(s.Settings.Modes, func(m string) bool { return m == name })
	if s.Settings.DefaultMode == name {
		s.Settings.DefaultMode = s.Settings.Modes[0]
	}
	return nil
}

// Import adds ssh_config hosts whose label isn't already in the store to
// mode and returns how many were added. The ssh_config file is only read.
func (s *Store) Import(r io.Reader, mode string) (int, error) {
	parsed, err := ParseSSHConfig(r)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, h := range s.Hosts {
		have[h.Label] = true
	}
	added := 0
	for _, h := range parsed {
		if have[h.Label] {
			continue
		}
		h.ID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), added)
		h.Mode = mode
		s.Hosts = append(s.Hosts, h)
		added++
	}
	return added, nil
}

// ParseSSHConfig reads the Host/HostName/User/Port/IdentityFile subset of
// an OpenSSH client config.
// ponytail: Include, Match and ProxyJump are ignored; wildcard Host patterns
// are skipped. Add when imports need them.
func ParseSSHConfig(r io.Reader) ([]Host, error) {
	var hosts []Host
	var cur []int // indexes of hosts in the current Host block
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := cut(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			cur = cur[:0]
			for _, alias := range strings.Fields(val) {
				if strings.ContainsAny(alias, "*?!") {
					continue
				}
				cur = append(cur, len(hosts))
				hosts = append(hosts, Host{Label: alias, Hostname: alias})
			}
		case "match":
			cur = cur[:0]
		case "hostname":
			for _, i := range cur {
				hosts[i].Hostname = val
			}
		case "user":
			for _, i := range cur {
				hosts[i].User = val
			}
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("ssh_config: bad port %q", val)
			}
			for _, i := range cur {
				hosts[i].Port = p
			}
		case "identityfile":
			for _, i := range cur {
				hosts[i].KeyPath = val
			}
		}
	}
	return hosts, sc.Err()
}

// cut splits "Key value", "Key=value" and "Key = value", unquoting the value.
func cut(line string) (string, string, bool) {
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return "", "", false
	}
	key := line[:i]
	val := strings.TrimLeft(line[i:], " \t=")
	return key, strings.Trim(val, `"`), val != ""
}

// ExpandHome turns a leading ~ into the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
