package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// refresh is how often the view re-reads state. Decisions arrive far slower
// than this, so anything quicker just burns CPU redrawing identical frames.
const refresh = 250 * time.Millisecond

// tab identifies a view. The headless commands still exist for scripting and
// long background runs; these are the same capabilities made navigable.
type tab int

const (
	tabLive tab = iota
	tabWallets
	tabLedger
	tabFeed
	tabConfig
	tabCount
)

var tabNames = [tabCount]string{"Live", "Wallets", "Ledger", "Feed", "Config"}

type tickMsg time.Time

// model is the root Bubble Tea model. It owns no domain data — every view reads
// from State or loads on demand, so the UI and the daemon cannot disagree.
type model struct {
	state  *State
	snap   Snapshot
	active tab

	width, height int
	paused        bool
	quitting      bool

	// per-view state
	live    liveView
	wallets walletsView
	ledger  ledgerView
	feed    feedView
}

// Run starts the TUI and blocks until the user quits or ctx is cancelled.
// onQuit is invoked afterwards so the caller can stop the daemon.
//
// ctx must be threaded in: without it a --seconds deadline would stop the feed
// but leave the UI running forever on a dead pipeline.
func Run(ctx context.Context, state *State, onQuit func()) error {
	m := model{state: state, snap: state.Snapshot()}
	cfg := state.Snapshot().Config
	m.wallets = newWalletsView(cfg.RPC, cfg.LedgerPath)
	m.ledger = newLedgerView(cfg.LedgerPath)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if onQuit != nil {
		onQuit()
	}
	// A cancelled context is the deadline firing, not a failure.
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (m model) Init() tea.Cmd { return tea.Batch(tick(), m.ledger.load()) }

func tick() tea.Cmd {
	return tea.Tick(refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "tab", "right", "l":
			m.active = (m.active + 1) % tabCount
			return m, m.onEnterTab()
		case "shift+tab", "left", "h":
			m.active = (m.active + tabCount - 1) % tabCount
			return m, m.onEnterTab()
		case "1", "2", "3", "4", "5":
			m.active = tab(msg.String()[0] - '1')
			return m, m.onEnterTab()
		case "p":
			m.paused = !m.paused
			return m, nil
		}
		// Unhandled keys belong to the active view.
		return m.routeKey(msg)

	case tickMsg:
		if !m.paused {
			m.snap = m.state.Snapshot()
		}
		return m, tick()

	case walletsMsg:
		m.wallets.apply(msg)
		return m, nil

	case ledgerMsg:
		m.ledger.apply(msg)
		return m, nil
	}
	return m, nil
}

// onEnterTab kicks off any load the newly-focused view needs.
func (m model) onEnterTab() tea.Cmd {
	switch m.active {
	case tabLedger:
		return m.ledger.load()
	case tabWallets:
		if !m.wallets.loaded && !m.wallets.loading {
			m.wallets.loading = true
			return m.wallets.loadCmd(m.snap.Config.LedgerPath)
		}
	}
	return nil
}

func (m model) routeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.active {
	case tabLive:
		m.live.key(msg)
	case tabWallets:
		return m, m.wallets.key(msg, m.snap.Config.LedgerPath)
	case tabFeed:
		m.feed.key(msg)
	case tabLedger:
		if msg.String() == "r" {
			return m, m.ledger.load()
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	width := m.width
	if width < 60 {
		width = 100
	}
	inner := width - 4

	var b strings.Builder
	b.WriteString(m.header(inner))
	b.WriteString("\n")
	b.WriteString(m.tabBar(inner))
	b.WriteString("\n")

	body := m.height - 8
	if body < 6 {
		body = 20
	}
	switch m.active {
	case tabLive:
		b.WriteString(m.live.render(m.snap, inner, body))
	case tabWallets:
		b.WriteString(m.wallets.render(inner, body))
	case tabLedger:
		b.WriteString(m.ledger.render(inner))
	case tabFeed:
		b.WriteString(m.feed.render(m.snap, inner, body))
	case tabConfig:
		b.WriteString(renderConfig(m.snap, inner))
	}

	b.WriteString("\n")
	b.WriteString(m.footer(inner))
	return b.String()
}

func (m model) header(width int) string {
	mode := styleOK.Render("SHADOW — nothing is signed")
	if m.snap.Live {
		mode = styleBad.Render("LIVE — real transactions")
	}
	left := styleTitle.Render("hoodsniper") + "  " + mode
	right := styleDim.Render(fmt.Sprintf("watching %d · up %s",
		m.snap.Watching, m.snap.Uptime.Round(time.Second)))
	return left + gap(width, left, right) + right
}

func (m model) tabBar(width int) string {
	var parts []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %d %s ", i+1, name)
		if tab(i) == m.active {
			parts = append(parts, styleTabActive.Render(label))
		} else {
			parts = append(parts, styleTab.Render(label))
		}
	}
	bar := strings.Join(parts, "")
	if m.paused {
		bar += styleWarn.Render("  [paused]")
	}
	return bar
}

func (m model) footer(width int) string {
	common := " q quit · tab/1-5 switch · p pause"
	var view string
	switch m.active {
	case tabLive:
		view = " · a approved-only"
	case tabWallets:
		view = " · s score · enter drill-in · esc back"
	case tabLedger:
		view = " · r refresh"
	case tabFeed:
		view = " · d discover/stream"
	}
	return styleDim.Render(common + view)
}

// gap pads between a left- and right-aligned segment.
func gap(width int, left, right string) string {
	n := width - lipgloss.Width(left) - lipgloss.Width(right)
	if n < 1 {
		n = 1
	}
	return strings.Repeat(" ", n)
}

// short abbreviates an address to keep rows readable at narrow widths.
func short(addr string) string {
	if len(addr) < 12 {
		return addr
	}
	return addr[:8] + "…" + addr[len(addr)-4:]
}
