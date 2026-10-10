package tui

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kannan983/pangea/internal/health"
	"github.com/kannan983/pangea/internal/store"
)

const (
	cardMinW  = 28
	cardH     = 4
	cardGap   = 2
	hostsHead = 5 // search box (3) + toolbar (1) + blank (1)
)

type hostsView struct {
	search textinput.Model
	sel    int // index into cells
	scroll int // first visible virtual line
}

func newHostsView() hostsView {
	ti := textinput.New()
	ti.Prompt = "  "
	ti.Placeholder = "Find a host or ssh user@hostname..."
	return hostsView{search: ti}
}

// cell is one host (list row or card), positioned in virtual lines.
type cell struct {
	host        int // index in store.Hosts
	vy, x, w, h int
}

type hostsLayout struct {
	cells []cell
	lines []string // rendered virtual lines
}

// visibleHosts returns store indexes of the current mode's hosts that match
// the search, best match first.
func (m *Model) visibleHosts() []int {
	q := strings.TrimSpace(m.hosts.search.Value())
	type scored struct{ i, s int }
	var out []scored
	for i, h := range m.st.Hosts {
		if h.Mode != m.mode {
			continue
		}
		s := 0
		if q != "" {
			s = fuzzyScore(q, strings.Join(append([]string{h.Label, h.Hostname, h.User}, h.Tags...), " "))
			if s < 0 {
				continue
			}
		}
		out = append(out, scored{i, s})
	}
	hs := m.st.Hosts
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].s != out[b].s {
			return out[a].s > out[b].s
		}
		if q != "" && hs[out[a].i].Uses != hs[out[b].i].Uses {
			return hs[out[a].i].Uses > hs[out[b].i].Uses
		}
		return strings.ToLower(hs[out[a].i].Label) < strings.ToLower(hs[out[b].i].Label)
	})
	idx := make([]int, len(out))
	for i, o := range out {
		idx[i] = o.i
	}
	return idx
}

func (m *Model) layoutHosts() hostsLayout {
	var L hostsLayout
	q := strings.TrimSpace(m.hosts.search.Value())
	hosts := m.visibleHosts()
	if q != "" {
		L.lines = append(L.lines, stBold.Render(fmt.Sprintf(" Results for %q in %s", q, m.mode)))
	} else {
		L.lines = append(L.lines, stBold.Render(" "+m.mode)+stDim.Render(fmt.Sprintf("  %d host%s", len(hosts), plural(len(hosts)))))
	}
	if len(hosts) == 0 {
		switch {
		case q != "" && quickTarget(q) != nil:
			L.lines = append(L.lines, "", stDim.Render(" No saved host matches. Press enter to connect to ")+stAccent.Render(q))
		case q != "":
			L.lines = append(L.lines, "", stDim.Render(" No hosts match."))
		default:
			L.lines = append(L.lines, "", stDim.Render(" No hosts in "+m.mode+" yet. Press n to add one, or i to import ~/.ssh/config."))
		}
		return L
	}
	if m.st.Settings.GridView {
		m.layoutGrid(&L, hosts)
	} else {
		m.layoutList(&L, hosts)
	}
	return L
}

func (m *Model) layoutList(L *hostsLayout, hosts []int) {
	w := m.mainW() - 2
	nameW := max(12, w*3/10)
	addrW := max(14, w*3/10)
	tagW := max(0, w-nameW-addrW-6-12-10)
	// Rows are " " + mark (2) + badge (3) + " name address tags status last-used".
	L.lines = append(L.lines, stDim.Render(fit(fmt.Sprintf("       %-*s %-*s %-*s %-9s %s", nameW, "NAME", addrW, "ADDRESS", tagW, "TAGS", "STATUS", "LAST USED"), w)))
	for i, hi := range hosts {
		h := m.st.Hosts[hi]
		addr := h.Addr()
		if h.User != "" {
			addr = h.User + "@" + addr
		}
		sel := m.isSel(i)
		badge := lipgloss.NewStyle().Background(lipgloss.Color(colorFor(h.Mode + h.Label))).
			Foreground(lipgloss.Color("#ffffff")).Bold(true).Render(" " + initial(h.Label) + " ")
		pre := fmt.Sprintf(" %-*s %-*s %-*s ", nameW, trunc(h.Label, nameW), addrW, trunc(addr, addrW),
			tagW, trunc(strings.Join(h.Tags, ", "), tagW))
		status := m.healthBadge(h.ID)
		post := " " + ago(h.LastUsed)
		mark := "  "
		if sel {
			mark = stAccent.Render("▌ ")
			pre, post = stSel.Render(pre), stSel.Render(fit(post, w-5-lipgloss.Width(pre)-9))
		}
		rest := pre + status + post
		L.cells = append(L.cells, cell{host: hi, vy: len(L.lines), x: 1, w: w, h: 1})
		L.lines = append(L.lines, " "+mark+badge+rest)
	}
}

func (m *Model) layoutGrid(L *hostsLayout, hosts []int) {
	w := m.mainW() - 2
	cols := max(1, (w+cardGap)/(cardMinW+cardGap))
	cw := (w - cardGap*(cols-1)) / cols
	for r := 0; r*cols < len(hosts); r++ {
		row := hosts[r*cols : min(len(hosts), (r+1)*cols)]
		vy := len(L.lines)
		cards := make([][]string, len(row))
		for c, hi := range row {
			h := m.st.Hosts[hi]
			sub := "ssh"
			if h.User != "" {
				sub += ", " + h.User
			}
			for _, t := range h.Tags {
				sub += ", " + t
			}
			cards[c] = card(cw, initial(h.Label), h.Label, m.healthDot(h.ID), sub, colorFor(h.Mode+h.Label), m.isSel(r*cols+c))
			L.cells = append(L.cells, cell{host: hi, vy: vy, x: 1 + c*(cw+cardGap), w: cw, h: cardH})
		}
		for line := range cardH {
			var b strings.Builder
			b.WriteString(" ")
			for c, cd := range cards {
				if c > 0 {
					b.WriteString(strings.Repeat(" ", cardGap))
				}
				b.WriteString(cd[line])
			}
			L.lines = append(L.lines, b.String())
		}
	}
}

// ago is a short "last used" time.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// isSel is used while laying out, before cells exist; it compares indexes.
func (m *Model) isSel(i int) bool {
	return !m.hosts.search.Focused() && i == m.hosts.sel
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func initial(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}

var palette = []string{"#e95420", "#d70a53", "#f5a623", "#2e86de", "#10ac84", "#8854d0", "#0abde3", "#ee5253"}

func colorFor(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return palette[h.Sum32()%uint32(len(palette))]
}

// card renders a 4-line bordered card of width w.
func card(w int, badge, title, dot, sub, color string, selected bool) []string {
	border := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
	if selected {
		border = lipgloss.NewStyle().Foreground(accent).Bold(true)
	}
	inner := w - 2
	b := lipgloss.NewStyle().Background(lipgloss.Color(color)).Foreground(lipgloss.Color("#ffffff")).Bold(true).Render(" " + badge + " ")
	t := stBold.Render(trunc(title, inner-8))
	if selected {
		t = stBold.Foreground(accent).Render(trunc(title, inner-8))
	}
	t += " " + dot
	return []string{
		border.Render("╭" + strings.Repeat("─", inner) + "╮"),
		border.Render("│") + fit(" "+b+" "+t, inner) + border.Render("│"),
		border.Render("│") + fit("      "+stDim.Render(trunc(sub, inner-7)), inner) + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", inner) + "╯"),
	}
}

var quickRe = regexp.MustCompile(`^(?:ssh\s+)?(?:([^@\s]+)@)?([A-Za-z0-9._-]+)(?::(\d+))?$`)

// quickTarget parses "user@host[:port]" typed into the search box. A bare
// word without '.', '@' or ':' is not treated as a host.
func quickTarget(q string) *store.Host {
	sm := quickRe.FindStringSubmatch(strings.TrimSpace(q))
	if sm == nil || (sm[1] == "" && sm[3] == "" && !strings.Contains(sm[2], ".") && sm[2] != "localhost") {
		return nil
	}
	h := &store.Host{Label: strings.TrimPrefix(strings.TrimSpace(q), "ssh "), Hostname: sm[2], User: sm[1]}
	if sm[3] != "" {
		h.Port, _ = strconv.Atoi(sm[3])
	}
	return h
}

// ---- update ----

func (m *Model) updateHosts(msg tea.Msg) tea.Cmd {
	hv := &m.hosts
	L := m.layoutHosts()
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelDown {
			hv.scroll = min(hv.scroll+3, max(0, len(L.lines)-m.viewportH()))
		} else {
			hv.scroll = max(0, hv.scroll-3)
		}
		return nil
	case tea.PasteMsg:
		if !hv.search.Focused() {
			hv.search.Focus()
		}
	case tea.KeyPressMsg:
		if hv.search.Focused() {
			return m.searchKey(msg, L)
		}
		return m.gridKey(msg, L)
	}
	if hv.search.Focused() {
		var cmd tea.Cmd
		hv.search, cmd = hv.search.Update(msg)
		return cmd
	}
	return nil
}

func (m *Model) searchKey(k tea.KeyPressMsg, L hostsLayout) tea.Cmd {
	hv := &m.hosts
	switch k.String() {
	case "enter":
		if len(L.cells) > 0 {
			return m.connect(m.st.Hosts[L.cells[0].host], nil)
		}
		if h := quickTarget(hv.search.Value()); h != nil {
			return m.connect(*h, nil)
		}
		return nil
	case "esc":
		if hv.search.Value() != "" {
			hv.search.SetValue("")
		} else {
			hv.search.Blur()
		}
		hv.sel, hv.scroll = 0, 0
		return nil
	case "down":
		hv.search.Blur()
		hv.sel = 0
		return nil
	case "tab":
		m.focusSidebar()
		return nil
	}
	var cmd tea.Cmd
	hv.search, cmd = hv.search.Update(k)
	hv.sel, hv.scroll = 0, 0
	return cmd
}

func (m *Model) gridKey(k tea.KeyPressMsg, L hostsLayout) tea.Cmd {
	hv := &m.hosts
	if cmd := m.eggKey(k.String()); cmd != nil {
		return cmd
	}
	n := len(L.cells)
	if hv.sel >= n {
		hv.sel = max(0, n-1)
	}
	var cur *cell
	if n > 0 {
		cur = &L.cells[hv.sel]
	}
	switch k.String() {
	case "/":
		return hv.search.Focus()
	case "tab":
		m.focusSidebar()
		return nil
	case "esc", "backspace":
		if hv.search.Value() != "" {
			hv.search.SetValue("")
			hv.sel, hv.scroll = 0, 0
		}
		return nil
	case "n":
		return m.openForm(store.Host{Mode: m.mode}, false)
	case "i":
		return m.importSSHConfig()
	case "m":
		m.nextMode()
		return nil
	case "M":
		return m.newModePrompt()
	case "v":
		m.st.Settings.GridView = !m.st.Settings.GridView
		m.save()
		return nil
	}
	if n == 0 { // empty list: only ↑ (back to search) does anything
		if s := k.String(); s == "up" || s == "k" {
			return hv.search.Focus()
		}
		return nil
	}
	switch k.String() {
	case "right", "l":
		hv.sel = min(n-1, hv.sel+1)
	case "left", "h":
		hv.sel = max(0, hv.sel-1)
	case "down", "j":
		hv.sel = nearest(L.cells, hv.sel, +1)
	case "up", "k":
		if cur != nil && cur.vy == L.cells[0].vy {
			hv.search.Focus()
			return nil
		}
		hv.sel = nearest(L.cells, hv.sel, -1)
	case "home", "g":
		hv.sel = 0
	case "end", "G":
		hv.sel = max(0, n-1)
	}
	if cur == nil {
		return nil
	}
	cur = &L.cells[hv.sel]
	m.keepVisible(*cur)
	h := m.st.Hosts[cur.host]
	switch k.String() {
	case "enter":
		return m.connect(h, nil)
	case "s":
		return m.openSFTP(h)
	case "e":
		return m.openForm(h, false)
	case "c":
		return m.openForm(h, true)
	case "d":
		m.ask(fmt.Sprintf("Delete host %q? (y/n)", h.Label), func() tea.Cmd {
			m.st.Delete(h.ID)
			m.save()
			m.vault.Set(h.ID, "")
			if err := m.vault.Save(); err != nil {
				m.fail(err)
				return nil
			}
			m.info("Deleted " + h.Label)
			return nil
		})
	}
	return nil
}

// nearest finds the card in the next row up (dir -1) or down (+1) closest
// in x to the current one.
func nearest(cells []cell, from, dir int) int {
	cur := cells[from]
	best, bestRow := from, -1
	for i, c := range cells {
		if (dir > 0 && c.vy <= cur.vy) || (dir < 0 && c.vy >= cur.vy) {
			continue
		}
		closerRow := bestRow == -1 || (dir > 0 && c.vy < bestRow) || (dir < 0 && c.vy > bestRow)
		sameRow := c.vy == bestRow
		if closerRow || (sameRow && abs(c.x-cur.x) < abs(cells[best].x-cur.x)) {
			best, bestRow = i, c.vy
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (m *Model) viewportH() int { return max(1, m.mainH()-hostsHead) }

func (m *Model) keepVisible(c cell) {
	hv := &m.hosts
	top := c.vy
	if top <= 2 {
		top = 0 // keep the header in view
	}
	if top < hv.scroll {
		hv.scroll = top
	} else if c.vy+c.h > hv.scroll+m.viewportH() {
		hv.scroll = c.vy + c.h - m.viewportH()
	}
}

func (m *Model) importSSHConfig() tea.Cmd {
	home, err := os.UserHomeDir()
	if err != nil {
		m.fail(err)
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		m.fail(err)
		return nil
	}
	defer f.Close()
	n, err := m.st.Import(f, m.mode)
	if err != nil {
		m.fail(err)
		return nil
	}
	m.save()
	m.info(fmt.Sprintf("Imported %d host(s) from ~/.ssh/config into %s. Your file was not changed.", n, m.mode))
	return nil
}

// ---- view ----

func (m *Model) viewHosts() []string {
	hv := &m.hosts
	w := m.mainW() - 2
	ox := sidebarW + 1
	boxColor := lipgloss.Color("#3b4261")
	if hv.search.Focused() {
		boxColor = accent
	}
	box := lipgloss.NewStyle().Foreground(boxColor)
	lines := []string{
		" " + box.Render("╭"+strings.Repeat("─", w-2)+"╮"),
		" " + box.Render("│") + fit(hv.search.View(), w-2) + box.Render("│"),
		" " + box.Render("╰"+strings.Repeat("─", w-2)+"╯"),
	}
	m.addZone(ox, 0, w, 3, func(bool) tea.Cmd { return hv.search.Focus() })

	// toolbar buttons
	var tb strings.Builder
	tb.WriteString(" ")
	x := ox + 1
	button := func(label string, fn func() tea.Cmd) {
		s := " " + label + " "
		tb.WriteString(lipgloss.NewStyle().Background(lipgloss.Color("#292e42")).Render(s) + " ")
		m.addZone(x, 3, lipgloss.Width(s), 1, func(bool) tea.Cmd { return fn() })
		x += lipgloss.Width(s) + 1
	}
	button("+ New host  n", func() tea.Cmd { return m.openForm(store.Host{Mode: m.mode}, false) })
	button("⇣ Import ~/.ssh/config  i", m.importSSHConfig)
	view := "▦ Grid view  v"
	if m.st.Settings.GridView {
		view = "☰ List view  v"
	}
	button(view, func() tea.Cmd { m.st.Settings.GridView = !m.st.Settings.GridView; m.save(); return nil })
	button("⇄ Mode: "+m.mode+"  m", func() tea.Cmd { m.nextMode(); return nil })
	lines = append(lines, tb.String(), "")

	L := m.layoutHosts()
	vh := m.viewportH()
	hv.scroll = min(hv.scroll, max(0, len(L.lines)-vh))
	for i := hv.scroll; i < len(L.lines) && i < hv.scroll+vh; i++ {
		lines = append(lines, L.lines[i])
	}
	for ci, c := range L.cells {
		y := c.vy - hv.scroll + hostsHead
		if c.vy < hv.scroll || y+c.h > hostsHead+vh {
			continue
		}
		ci, c := ci, c
		m.addZone(ox+c.x, y, c.w, c.h, func(double bool) tea.Cmd {
			hv.search.Blur()
			hv.sel = ci
			if !double {
				return nil
			}
			return m.connect(m.st.Hosts[c.host], nil)
		})
	}
	return lines
}

// healthDot is a colored ● for a host's last health check.
func (m *Model) healthDot(id string) string {
	r, ok := m.health[id]
	if !ok {
		return stDim.Render("○")
	}
	switch r.State {
	case health.Up:
		return lipgloss.NewStyle().Foreground(good).Render("●")
	case health.Slow:
		return lipgloss.NewStyle().Foreground(amber).Render("●")
	}
	return lipgloss.NewStyle().Foreground(bad).Render("●")
}

// healthBadge is the 9-cell STATUS column: "● 12ms", "● down".
func (m *Model) healthBadge(id string) string {
	r, ok := m.health[id]
	text := "…"
	switch {
	case !ok:
	case r.State == health.Down:
		text = "down"
	default:
		text = fmt.Sprintf("%dms", r.RTT.Milliseconds())
	}
	return m.healthDot(id) + " " + fit(text, 7)
}
