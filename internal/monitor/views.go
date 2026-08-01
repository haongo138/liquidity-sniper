package monitor

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	styleTitle     = lipgloss.NewStyle().Bold(true)
	styleDim       = lipgloss.NewStyle().Faint(true)
	styleOK        = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleBad       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleHeader    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111"))
	styleTab       = lipgloss.NewStyle().Faint(true)
	styleTabActive = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("231")).Background(lipgloss.Color("62"))
	styleBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	styleSel = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("238"))
)

// ─── Live ────────────────────────────────────────────────────────────────────

type liveView struct{ approvedOnly bool }

func (v *liveView) key(msg tea.KeyMsg) {
	if msg.String() == "a" {
		v.approvedOnly = !v.approvedOnly
	}
}

func (v liveView) render(s Snapshot, width, rows int) string {
	link := styleBad.Render("disconnected")
	if s.Connected {
		link = styleOK.Render("connected")
	}
	feed := styleHeader.Render("feed") + "   " + link +
		styleDim.Render(fmt.Sprintf("   seq %d · blocks %d · txs %d · watched %d",
			s.LastSeq, s.Blocks, s.Txs, s.Matched))

	decode := fmt.Sprintf("%.1f%% (%d ok / %d failed)", s.DecodeRate, s.DecodeOK, s.DecodeNG)
	if s.DecodeAlarm() {
		decode = styleWarn.Render(decode + "  ROUTER LAYOUT MAY HAVE CHANGED")
	} else if s.DecodeOK > 0 {
		decode = styleOK.Render(decode)
	}
	lat := fmt.Sprintf("p50 %.0fms · p90 %.0fms · max %.0fms", s.P50, s.P90, s.Max)
	if s.P90 > 2000 {
		lat = styleWarn.Render(lat)
	}
	health := styleHeader.Render("health") + "\n" +
		" decode   " + decode + "\n" +
		" latency  " + lat + "\n" +
		" cache    " + styleDim.Render(fmt.Sprintf("%.0f%% (%d hits / %d misses)",
		s.CacheRate, s.CacheHits, s.CacheMisses)) + "\n" +
		" verdicts " + styleOK.Render(fmt.Sprintf("%d approved", s.Approved)) +
		styleDim.Render(fmt.Sprintf(" · %d rejected", s.Rejected))

	title := styleHeader.Render("decisions")
	if v.approvedOnly {
		title += styleDim.Render("  [approved only]")
	}
	body := rows - 10
	if body < 3 {
		body = 5
	}
	var lines []string
	for i := len(s.Recent) - 1; i >= 0 && len(lines) < body; i-- {
		d := s.Recent[i]
		if v.approvedOnly && !d.Approved {
			continue
		}
		lines = append(lines, renderDecision(d, width))
	}
	if len(lines) == 0 {
		lines = append(lines, styleDim.Render("  waiting for a watched wallet to trade…"))
	}

	return styleBox.Width(width).Render(feed) + "\n" +
		styleBox.Width(width).Render(health) + "\n" +
		styleBox.Width(width).Render(title+"\n"+strings.Join(lines, "\n"))
}

func renderDecision(d Decision, width int) string {
	mark := styleBad.Render("REJECT ")
	if d.Approved {
		mark = styleOK.Render("APPROVE")
	}
	lat := fmt.Sprintf("%6.0fms", d.LatencyMS)
	if d.LatencyMS > 2000 {
		lat = styleWarn.Render(lat)
	} else {
		lat = styleDim.Render(lat)
	}
	head := fmt.Sprintf(" %s %-4s %s %s ", mark, d.Direction, short(d.Token), lat)
	reason := d.Reason
	if budget := width - lipgloss.Width(head) - 2; budget > 8 && len(reason) > budget {
		reason = reason[:budget-1] + "…"
	}
	return head + styleDim.Render(reason)
}

// ─── Feed ────────────────────────────────────────────────────────────────────

type feedView struct{ discover bool }

func (v *feedView) key(msg tea.KeyMsg) {
	if msg.String() == "d" {
		v.discover = !v.discover
	}
}

func (v feedView) render(s Snapshot, width, rows int) string {
	if v.discover {
		title := styleHeader.Render("discover") +
			styleDim.Render("  top contract/selector pairs by call volume")
		head := fmt.Sprintf("  %-44s %-12s %8s %8s", "CONTRACT", "SELECTOR", "CALLS", "SENDERS")
		lines := []string{styleDim.Render(head)}
		for i, t := range s.Tally {
			if i >= rows-4 {
				break
			}
			lines = append(lines, fmt.Sprintf("  %-44s %-12s %8d %8d",
				t.To, t.Selector, t.N, t.Senders))
		}
		if len(s.Tally) == 0 {
			lines = append(lines, styleDim.Render("  no router traffic observed yet…"))
		}
		lines = append(lines, "", styleDim.Render(
			"  a router missing here that should be present is why decode rates fall"))
		return styleBox.Width(width).Render(title + "\n" + strings.Join(lines, "\n"))
	}

	title := styleHeader.Render("feed") + styleDim.Render("  raw router-bound traffic")
	var lines []string
	for i := len(s.RawRecent) - 1; i >= 0 && len(lines) < rows-3; i-- {
		t := s.RawRecent[i]
		mark := styleDim.Render("·")
		if !t.Decoded {
			mark = styleWarn.Render("?")
		}
		lines = append(lines, fmt.Sprintf(" %s seq %d  %s → %s  %s",
			mark, t.Seq, short(t.From), short(t.To), styleDim.Render(t.Selector)))
	}
	if len(lines) == 0 {
		lines = append(lines, styleDim.Render("  waiting for router traffic…"))
	}
	return styleBox.Width(width).Render(title + "\n" + strings.Join(lines, "\n"))
}

// ─── Ledger ──────────────────────────────────────────────────────────────────

type ledgerMsg struct {
	path    string
	bytes   int64
	entries int
	wallets int
	deep    map[int]int
	err     error
}

type ledgerView struct {
	path string
	msg  ledgerMsg
	busy bool
}

func newLedgerView(path string) ledgerView {
	if path == "" {
		path = "ledger.jsonl"
	}
	return ledgerView{path: path}
}

func (v *ledgerView) apply(m ledgerMsg) { v.msg, v.busy = m, false }

// load counts the ledger off the main goroutine. Scanning 250k lines takes
// ~350ms, which would visibly stall the UI if done inline.
func (v ledgerView) load() tea.Cmd {
	path := v.path
	return func() tea.Msg {
		out := ledgerMsg{path: path, deep: map[int]int{}}
		fi, err := os.Stat(path)
		if err != nil {
			out.err = err
			return out
		}
		out.bytes = fi.Size()

		byWallet, err := loadLedgerCounts(path)
		if err != nil {
			out.err = err
			return out
		}
		out.wallets = len(byWallet)
		for _, n := range byWallet {
			out.entries += n
			for _, k := range []int{5, 10, 20, 40, 100, 200} {
				if n >= k {
					out.deep[k]++
				}
			}
		}
		return out
	}
}

func (v ledgerView) render(width int) string {
	title := styleHeader.Render("ledger") + styleDim.Render("  "+v.path)
	if v.msg.err != nil {
		return styleBox.Width(width).Render(title + "\n\n " +
			styleWarn.Render("not readable: "+v.msg.err.Error()) + "\n\n" +
			styleDim.Render(" start one with:  go run ./cmd/collect --ledger "+v.path))
	}
	if v.msg.entries == 0 {
		return styleBox.Width(width).Render(title + "\n\n " +
			styleDim.Render("loading…"))
	}

	m := v.msg
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(fmt.Sprintf(" %-22s %s\n", "size", humanBytes(m.bytes)))
	b.WriteString(fmt.Sprintf(" %-22s %s\n", "swaps recorded", comma(m.entries)))
	b.WriteString(fmt.Sprintf(" %-22s %s\n", "distinct wallets", comma(m.wallets)))
	b.WriteString("\n" + styleDim.Render(" wallets by history depth") + "\n")
	for _, k := range []int{5, 10, 20, 40, 100, 200} {
		b.WriteString(fmt.Sprintf("   >=%-4d txs  %s\n", k, comma(m.deep[k])))
	}

	// The explorer caps at ~50 transactions per wallet, which is what made
	// ranking noise. This is the number that says whether that is fixed.
	ready := m.deep[100]
	verdict := styleWarn.Render("not enough depth to rank wallets yet — keep collecting")
	if ready >= 20 {
		verdict = styleOK.Render(fmt.Sprintf(
			"%d wallets have >=100 trades — deep enough to rank", ready))
	}
	b.WriteString("\n " + verdict + "\n")
	return styleBox.Width(width).Render(b.String())
}

// ─── Config ──────────────────────────────────────────────────────────────────

func renderConfig(s Snapshot, width int) string {
	c := s.Config
	title := styleHeader.Render("config") + styleDim.Render("  "+c.Path)

	var b strings.Builder
	b.WriteString(title + "\n\n")
	for _, row := range [][2]string{
		{"feed", c.Feed}, {"rpc", c.RPC},
		{"chain id", fmt.Sprintf("%d", c.ChainID)},
		{"trade size", fmt.Sprintf("%g ETH", c.TradeSizeETH)},
		{"mode", map[bool]string{true: "LIVE", false: "shadow (nothing signed)"}[c.Live]},
	} {
		b.WriteString(fmt.Sprintf(" %-14s %s\n", row[0], row[1]))
	}

	b.WriteString("\n" + styleDim.Render(" watchlist") + "\n")
	if len(c.Watch) == 0 {
		b.WriteString(styleWarn.Render("   empty — nothing will ever match\n"))
	}
	for _, w := range c.Watch {
		b.WriteString("   " + w + "\n")
	}

	b.WriteString("\n" + styleDim.Render(" filters") + "\n")
	for _, f := range c.Filters {
		note := ""
		if f.Note != "" {
			note = styleDim.Render("  " + f.Note)
		}
		b.WriteString(fmt.Sprintf("   %-22s %-10s%s\n", f.Name, f.Value, note))
	}
	b.WriteString("\n" + styleDim.Render(
		" read-only. Edit the YAML and restart — a file that gates money should not\n"+
			" be editable by a stray keystroke.") + "\n")
	return styleBox.Width(width).Render(b.String())
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func humanBytes(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func comma(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, out...), ",")
}
