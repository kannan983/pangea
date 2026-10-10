package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kannan983/pangea/internal/store"
)

const (
	fHost = iota
	fLabel
	fPort
	fUser
	fPass
	fKey
	fMode
	fTags
	fLocal
	fRemote
	fCount
)

var fieldNames = [fCount]string{"Hostname *", "Label", "Port", "Username", "Password", "Key file", "Mode",
	"Tags", "SFTP local", "SFTP remote"}
var fieldHints = [fCount]string{
	"IP or DNS name",
	"defaults to the hostname",
	"22",
	"defaults to your local user",
	"saved encrypted; empty = use keys / ask when needed",
	"empty: ssh-agent, then ~/.ssh/id_*",
	"Office, Personal, ...",
	"comma separated",
	"start folder here, e.g. ~/TTS (default from Settings)",
	"start folder on the host, e.g. /opt/app (default: home)",
}

type formView struct {
	host       store.Host // being edited (ID empty for new)
	cloneOf    string     // ID whose saved password a clone inherits
	hasPass    bool       // host already has a saved password
	removePass bool
	fields     [fCount]textinput.Model
	focus      int
	err        string
}

func (m *Model) openForm(h store.Host, clone bool) tea.Cmd {
	f := &formView{hasPass: m.vault.Get(h.ID) != ""}
	if clone {
		f.cloneOf = h.ID
		h.ID, h.Uses, h.LastUsed = "", 0, time.Time{}
		h.Label += " (copy)"
	}
	f.host = h
	vals := [fCount]string{h.Hostname, h.Label, "", h.User, "", h.KeyPath, h.Mode, strings.Join(h.Tags, ", "),
		h.LocalDir, h.RemoteDir}
	if h.Port != 0 {
		vals[fPort] = strconv.Itoa(h.Port)
	}
	for i := range f.fields {
		ti := textinput.New()
		ti.Prompt = ""
		ti.Placeholder = fieldHints[i]
		ti.SetValue(vals[i])
		ti.SetWidth(min(90, m.mainW()-22))
		f.fields[i] = ti
	}
	f.fields[fKey].ShowSuggestions = true
	f.fields[fKey].SetSuggestions(keyFiles())
	f.fields[fMode].ShowSuggestions = true
	f.fields[fMode].SetSuggestions(m.st.Settings.Modes)
	f.fields[fPass].EchoMode = textinput.EchoPassword
	if f.hasPass {
		f.fields[fPass].Placeholder = "•••••• saved — type to change, ctrl+x to remove"
	}
	f.focus = 0
	m.form = f
	return f.fields[f.focus].Focus()
}

// keyFiles lists likely private keys in ~/.ssh as ~/.ssh/name.
func keyFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	ents, _ := os.ReadDir(filepath.Join(home, ".ssh"))
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasSuffix(n, ".pub") || strings.HasPrefix(n, "known_hosts") ||
			n == "config" || n == "authorized_keys" || strings.HasPrefix(n, ".") {
			continue
		}
		out = append(out, "~/.ssh/"+n)
	}
	return out
}

func (m *Model) closeForm() { m.form = nil }

func (f *formView) setFocus(i int) tea.Cmd {
	f.fields[f.focus].Blur()
	f.focus = (i + fCount) % fCount
	return f.fields[f.focus].Focus()
}

func (m *Model) updateForm(msg tea.Msg) tea.Cmd {
	f := m.form
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			m.closeForm()
			return nil
		case "ctrl+s":
			return m.saveForm()
		case "ctrl+x":
			if f.focus == fPass && f.hasPass {
				f.removePass = true
				f.fields[fPass].SetValue("")
				f.fields[fPass].Placeholder = "saved password will be removed on save"
				return nil
			}
		case "enter":
			if f.focus == fCount-1 {
				return m.saveForm()
			}
			return f.setFocus(f.focus + 1)
		case "tab", "down":
			// tab accepts a visible suggestion first, like a shell.
			if k.String() == "tab" && f.fields[f.focus].CurrentSuggestion() != "" &&
				f.fields[f.focus].CurrentSuggestion() != f.fields[f.focus].Value() {
				break
			}
			return f.setFocus(f.focus + 1)
		case "shift+tab", "up":
			return f.setFocus(f.focus - 1)
		}
	}
	var cmd tea.Cmd
	f.fields[f.focus], cmd = f.fields[f.focus].Update(msg)
	return cmd
}

func (m *Model) saveForm() tea.Cmd {
	f := m.form
	v := func(i int) string { return strings.TrimSpace(f.fields[i].Value()) }
	h := f.host
	h.Hostname, h.User, h.KeyPath, h.Mode = v(fHost), v(fUser), v(fKey), v(fMode)
	h.LocalDir, h.RemoteDir = v(fLocal), v(fRemote)
	if h.Mode == "" {
		h.Mode = m.mode
	}
	if !slices.Contains(m.st.Settings.Modes, h.Mode) {
		f.err = fmt.Sprintf("no mode %q — pick one of %s, or add it with + New mode", h.Mode,
			strings.Join(m.st.Settings.Modes, ", "))
		return nil
	}
	h.Label = v(fLabel)
	if h.Label == "" {
		h.Label = h.Hostname
	}
	h.Port = 0
	h.Tags = nil
	for _, t := range strings.Split(v(fTags), ",") {
		if t = strings.TrimSpace(t); t != "" {
			h.Tags = append(h.Tags, t)
		}
	}
	if err := validate(&h, v(fPort)); err != nil {
		f.err = err.Error()
		return nil
	}
	id := m.st.Put(h)
	m.save()
	pw := f.fields[fPass].Value() // not trimmed: spaces can be part of a password
	switch {
	case pw != "":
		m.vault.Set(id, pw)
	case f.removePass:
		m.vault.Set(id, "")
	case f.cloneOf != "":
		m.vault.Set(id, m.vault.Get(f.cloneOf))
	}
	if err := m.vault.Save(); err != nil {
		f.err = "host saved, but the password was not: " + err.Error()
		return nil
	}
	m.info("Saved " + h.Label)
	m.closeForm()
	if h.Mode != m.mode {
		m.setMode(h.Mode)
	}
	return nil
}

func validate(h *store.Host, port string) error {
	if h.Hostname == "" {
		return errors.New("hostname is required")
	}
	if strings.ContainsAny(h.Hostname, " \t@") {
		return errors.New("hostname must not contain spaces or @ (put the user in Username)")
	}
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("port must be 1-65535")
		}
		if p != 22 {
			h.Port = p
		}
	}
	if h.KeyPath != "" {
		if _, err := os.Stat(store.ExpandHome(h.KeyPath)); err != nil {
			return fmt.Errorf("key file: %v", err)
		}
	}
	if h.LocalDir != "" {
		if fi, err := os.Stat(store.ExpandHome(h.LocalDir)); err != nil || !fi.IsDir() {
			return fmt.Errorf("SFTP local folder %q does not exist", h.LocalDir)
		}
	}
	return nil
}

func (m *Model) viewForm() []string {
	f := m.form
	title := "New host"
	if f.host.ID != "" {
		title = "Edit host"
	}
	lines := []string{"", stBold.Render("  " + title), ""}
	ox := sidebarW + 1
	for i := range f.fields {
		name := fmt.Sprintf("  %-12s", fieldNames[i])
		if i == f.focus {
			name = stAccent.Bold(true).Render(fmt.Sprintf("› %-12s", fieldNames[i]))
		}
		y := len(lines)
		lines = append(lines, name+"  "+f.fields[i].View())
		i := i
		m.addZone(ox, y, m.mainW(), 1, func(bool) tea.Cmd { return f.setFocus(i) })
	}
	lines = append(lines, "")
	if f.err != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(bad).Render("  "+f.err), "")
	}
	y := len(lines)
	save := lipgloss.NewStyle().Background(accent).Foreground(lipgloss.Color("#1a1b26")).Bold(true).Render(" Save  ctrl+s ")
	cancel := lipgloss.NewStyle().Background(lipgloss.Color("#292e42")).Render(" Cancel  esc ")
	lines = append(lines, "  "+save+"  "+cancel)
	m.addZone(ox+2, y, lipgloss.Width(save), 1, func(bool) tea.Cmd { return m.saveForm() })
	m.addZone(ox+4+lipgloss.Width(save), y, lipgloss.Width(cancel), 1, func(bool) tea.Cmd { m.closeForm(); return nil })
	return lines
}
