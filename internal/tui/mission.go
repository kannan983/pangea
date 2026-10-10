package tui

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kannan983/pangea/internal/procstat"
)

// The 1202 easter egg: type 1202 on the Hosts screen. A DSKY-style panel
// with Pangea's own CPU and memory, for Margaret Hamilton.

const eggCode = "1202"

type missionView struct {
	opened  time.Time
	lastCPU time.Duration
	lastAt  time.Time
	cpu     []float64 // % of one core, one sample per second
	ticking bool
}

type missionTickMsg struct{ v *missionView }

func (m *Model) openMission() tea.Cmd {
	v := &missionView{opened: time.Now(), lastCPU: procstat.CPU(), lastAt: time.Now()}
	m.mission, m.form, m.settings = v, nil, nil
	return m.missionTick()
}

func (m *Model) missionTick() tea.Cmd {
	v := m.mission
	if v == nil || v.ticking {
		return nil
	}
	v.ticking = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return missionTickMsg{v} })
}

// sample records CPU use since the last tick; ticking stops once the panel
// is closed, so it costs nothing when not shown.
func (m *Model) missionSample(v *missionView) tea.Cmd {
	v.ticking = false
	if m.mission != v {
		return nil
	}
	now, c := time.Now(), procstat.CPU()
	if dt := now.Sub(v.lastAt); dt > 0 {
		v.cpu = append(v.cpu, 100*float64(c-v.lastCPU)/float64(dt))
		if len(v.cpu) > 60 {
			v.cpu = v.cpu[1:]
		}
	}
	v.lastCPU, v.lastAt = c, now
	return m.missionTick()
}

// eggKey collects digits typed on the hosts grid.
func (m *Model) eggKey(k string) tea.Cmd {
	if len(k) != 1 || k[0] < '0' || k[0] > '9' {
		m.eggBuf = ""
		return nil
	}
	m.eggBuf += k
	if len(m.eggBuf) > len(eggCode) {
		m.eggBuf = m.eggBuf[len(m.eggBuf)-len(eggCode):]
	}
	if m.eggBuf == eggCode {
		m.eggBuf = ""
		return m.openMission()
	}
	return nil
}

func sparkline(vals []float64, w int) string {
	bars := []rune("▁▂▃▄▅▆▇█")
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	top := 5.0 // scale to at least 5% so idle noise stays low
	for _, v := range vals {
		top = max(top, v)
	}
	var b strings.Builder
	for _, v := range vals {
		b.WriteRune(bars[min(len(bars)-1, int(v/top*float64(len(bars)-1)+0.5))])
	}
	return b.String()
}

func (m *Model) viewMission() []string {
	v := m.mission
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mem, rssOK := procstat.RSS()
	memLabel := "MEMORY MB (RSS)"
	if !rssOK {
		mem, memLabel = ms.Sys, "MEMORY MB (GO)"
	}
	cpu := 0.0
	if n := len(v.cpu); n > 0 {
		cpu = v.cpu[n-1]
	}
	sessions, transfers := 0, 0
	for _, t := range m.tabs {
		for _, p := range t.panes {
			if !p.sess.Closed() {
				sessions++
			}
		}
		if t.sftp != nil {
			for _, tr := range t.sftp.transfers {
				if tr.running() {
					transfers++
				}
			}
		}
	}

	green := lipgloss.NewStyle().Foreground(lipgloss.Color("#9ece6a")).Bold(true)
	light := lipgloss.NewStyle().Foreground(amber).Bold(true)
	off := stDim
	box := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
	reg := func(label string, val float64, digits string) string {
		sign := "+"
		if val < 0 {
			sign = "-"
		}
		return fmt.Sprintf("  %s  %s", green.Render(fmt.Sprintf("%s%"+digits, sign, val)), stDim.Render(label))
	}
	acty := off.Render("COMP ACTY")
	if cpu > 0.5 && time.Now().Second()%2 == 0 {
		acty = light.Render("COMP ACTY")
	}
	alarm := off.Render("PROG ALARM")
	if cpu > 80 {
		alarm = light.Render("PROG ALARM")
	}

	w := 46
	row := func(s string) string { return box.Render("│") + " " + fit(s, w-4) + " " + box.Render("│") }
	lines := []string{
		"",
		"  " + light.Render("1202 PROGRAM ALARM") + stDim.Render("  — executive overflow? not here."),
		"",
		"  " + box.Render("╭"+strings.Repeat("─", w-2)+"╮"),
		"  " + row(acty+"   "+alarm+"   "+stDim.Render("PROG ")+green.Render("63")),
		"  " + row(stDim.Render("VERB ")+green.Render("16")+stDim.Render("   NOUN ")+green.Render("36")+stDim.Render("   PANGEA")),
		"  " + row(box.Render(strings.Repeat("─", w-4))),
		"  " + row(reg("CPU % OF ONE CORE", cpu, "06.1f")),
		"  " + row(reg(memLabel, float64(mem)/(1<<20), "06.1f")),
		"  " + row(reg("GOROUTINES", float64(runtime.NumGoroutine()), "05.0f")),
		"  " + row(box.Render(strings.Repeat("─", w-4))),
		"  " + row(stDim.Render("CPU 60s ")+green.Render(sparkline(v.cpu, w-13))),
		"  " + box.Render("╰"+strings.Repeat("─", w-2)+"╯"),
		"",
		stDim.Render(fmt.Sprintf("  heap %.1f MB · GC runs %d · sessions %d · transfers %d · panel open %s",
			float64(ms.HeapAlloc)/(1<<20), ms.NumGC, sessions, transfers, time.Since(v.opened).Round(time.Second))),
		"",
		"  " + stBold.Render("For Margaret Hamilton."),
		"",
		"  She led the MIT team that wrote the on-board flight software for Apollo.",
		"  On 20 July 1969, during Apollo 11's descent, the guidance computer raised",
		"  1202 and 1201 alarms: it was being asked to do more than it could.",
		"  The software's priority scheduling dropped the less important work, kept",
		"  the landing tasks running, and Eagle landed.",
		"",
		stDim.Render("  Pangea tries to stay that light.   esc to return"),
	}
	return lines
}
