package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kannan983/pangea/internal/backup"
	"github.com/kannan983/pangea/internal/store"
)

const (
	sLocal = iota
	sDefMode
	sModes
	sCount
)

type settingsView struct {
	local, defMode textinput.Model
	focus          int
	modeSel        int
	err            string
}

func (v *settingsView) typing() bool { return v.focus < sModes }

func (m *Model) openSettings() {
	v := &settingsView{local: textinput.New(), defMode: textinput.New()}
	for _, ti := range []*textinput.Model{&v.local, &v.defMode} {
		ti.Prompt = ""
		ti.SetWidth(min(50, m.mainW()-34))
	}
	v.local.Placeholder = "empty = your home folder"
	v.local.SetValue(m.st.Settings.LocalDir)
	v.defMode.SetValue(m.st.Settings.DefaultMode)
	v.defMode.ShowSuggestions = true
	v.defMode.SetSuggestions(m.st.Settings.Modes)
	v.local.Focus()
	m.settings, m.form = v, nil
}

func (m *Model) settingsHelp() string {
	if m.settings.focus == sModes {
		return "↑↓ select · enter/* make default · r rename · d delete · n new · b back up · tab fields · esc back"
	}
	return "type · tab next · enter/ctrl+s save · esc back"
}

func (v *settingsView) setFocus(i int) tea.Cmd {
	v.local.Blur()
	v.defMode.Blur()
	v.focus = (i + sCount) % sCount
	switch v.focus {
	case sLocal:
		return v.local.Focus()
	case sDefMode:
		return v.defMode.Focus()
	}
	return nil
}

func (m *Model) saveSettings() error {
	v := m.settings
	local, mode := strings.TrimSpace(v.local.Value()), strings.TrimSpace(v.defMode.Value())
	if local != "" {
		if fi, err := os.Stat(store.ExpandHome(local)); err != nil || !fi.IsDir() {
			return fmt.Errorf("folder %q does not exist", local)
		}
	}
	if !slices.Contains(m.st.Settings.Modes, mode) {
		return fmt.Errorf("no mode %q (add it under Modes first)", mode)
	}
	m.st.Settings.LocalDir, m.st.Settings.DefaultMode = local, mode
	m.save()
	m.info("Settings saved")
	return nil
}

func (m *Model) updateSettings(msg tea.Msg) tea.Cmd {
	v := m.settings
	k, isKey := msg.(tea.KeyPressMsg)
	if isKey {
		v.err = ""
		switch k.String() {
		case "esc":
			m.settings = nil
			return nil
		case "tab", "down":
			if v.focus < sModes {
				return v.setFocus(v.focus + 1)
			}
		case "shift+tab":
			return v.setFocus(v.focus - 1)
		case "up":
			if v.focus < sModes {
				return v.setFocus(v.focus - 1)
			}
		case "enter", "ctrl+s":
			if v.focus < sModes {
				if err := m.saveSettings(); err != nil {
					v.err = err.Error()
					return nil
				}
				return v.setFocus(v.focus + 1)
			}
		}
	}
	switch v.focus {
	case sLocal:
		var cmd tea.Cmd
		v.local, cmd = v.local.Update(msg)
		return cmd
	case sDefMode:
		var cmd tea.Cmd
		v.defMode, cmd = v.defMode.Update(msg)
		return cmd
	}
	if !isKey {
		return nil
	}
	modes := m.st.Settings.Modes
	v.modeSel = min(v.modeSel, len(modes)-1)
	sel := modes[v.modeSel]
	switch k.String() {
	case "up", "k":
		if v.modeSel == 0 {
			return v.setFocus(sDefMode)
		}
		v.modeSel--
	case "down", "j":
		v.modeSel = min(len(modes)-1, v.modeSel+1)
	case "enter", "*":
		m.st.Settings.DefaultMode = sel
		v.defMode.SetValue(sel)
		m.save()
		m.info(sel + " is now the default mode")
	case "n":
		return m.newModePrompt()
	case "r":
		return m.askText("Rename mode", "New name for "+sel, sel, func(name string) tea.Cmd {
			if err := m.st.RenameMode(sel, name); err != nil {
				m.fail(err)
				return nil
			}
			if m.mode == sel {
				m.mode = strings.TrimSpace(name)
			}
			v.defMode.SetValue(m.st.Settings.DefaultMode)
			m.save()
			m.info("Renamed " + sel + " to " + strings.TrimSpace(name))
			return nil
		})
	case "d":
		m.ask(fmt.Sprintf("Delete mode %q? (y/n)", sel), func() tea.Cmd {
			if err := m.st.DeleteMode(sel); err != nil {
				m.fail(err)
				return nil
			}
			if m.mode == sel {
				m.mode = m.st.Settings.DefaultMode
			}
			v.defMode.SetValue(m.st.Settings.DefaultMode)
			m.save()
			m.info("Deleted mode " + sel)
			return nil
		})
	case "b":
		return m.backupPrompt()
	}
	return nil
}

type backupDoneMsg struct {
	path string
	keys int
	err  error
}

// backupPrompt asks for a file and a passphrase (twice), then backs up.
func (m *Model) backupPrompt() tea.Cmd {
	home, _ := os.UserHomeDir()
	def := filepath.Join(home, "pangea-backup-"+time.Now().Format("2006-01-02")+".pgb")
	return m.askText("Back up Pangea", "Save the backup to", def, func(path string) tea.Cmd {
		path = store.ExpandHome(strings.TrimSpace(path))
		return m.askSecret("Back up Pangea", "Passphrase for the backup\n(it contains your private keys and passwords;\nyou need it to restore)", func(p1 string) tea.Cmd {
			return m.askSecret("Back up Pangea", "Repeat the passphrase", func(p2 string) tea.Cmd {
				if p1 != p2 {
					m.fail(errors.New("passphrases do not match — backup not written"))
					return nil
				}
				st, vlt := m.st, m.vault
				m.info("Writing backup…")
				return func() tea.Msg {
					n, err := backup.Create(path, p1, st, vlt)
					return backupDoneMsg{path, n, err}
				}
			})
		})
	})
}

func (m *Model) viewSettings() []string {
	v := m.settings
	ox := sidebarW + 1
	label := func(i int, s string) string {
		if v.focus == i {
			return stAccent.Bold(true).Render(fmt.Sprintf("› %-28s", s))
		}
		return fmt.Sprintf("  %-28s", s)
	}
	lines := []string{"", stBold.Render("  Settings"), ""}
	y := len(lines)
	lines = append(lines, label(sLocal, "SFTP default local folder")+v.local.View())
	m.addZone(ox, y, m.mainW(), 1, func(bool) tea.Cmd { return v.setFocus(sLocal) })
	y = len(lines)
	lines = append(lines, label(sDefMode, "Default mode")+v.defMode.View())
	m.addZone(ox, y, m.mainW(), 1, func(bool) tea.Cmd { return v.setFocus(sDefMode) })
	if v.err != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(bad).Render("  "+v.err))
	}

	lines = append(lines, "", label(sModes, "Modes")+stDim.Render("enter default · r rename · d delete · n new"))
	for i, mode := range m.st.Settings.Modes {
		row := fmt.Sprintf("    %-20s %3d host%s", trunc(mode, 20), m.st.ModeCount(mode), plural(m.st.ModeCount(mode)))
		if mode == m.st.Settings.DefaultMode {
			row += stAccent.Render("  default")
		}
		if v.focus == sModes && i == v.modeSel {
			row = stSel.Render(fit(row, m.mainW()-4))
		}
		y := len(lines)
		lines = append(lines, row)
		i := i
		m.addZone(ox, y, m.mainW(), 1, func(bool) tea.Cmd { v.setFocus(sModes); v.modeSel = i; return nil })
	}

	lines = append(lines, "", "  "+stBold.Render("Backup"))
	y = len(lines)
	btn := lipgloss.NewStyle().Background(accent).Foreground(lipgloss.Color("#1a1b26")).Bold(true).Render(" Back up now  b ")
	lines = append(lines, "  "+btn+stDim.Render("   hosts, modes, settings, known_hosts, passwords and key files — encrypted"))
	m.addZone(ox+2, y, lipgloss.Width(btn), 1, func(bool) tea.Cmd { return m.backupPrompt() })
	lines = append(lines, stDim.Render("  Restore on another machine:  pangea restore FILE"),
		"", stDim.Render("  Saved passwords are encrypted; the key is kept in your system keyring."))
	return lines
}
