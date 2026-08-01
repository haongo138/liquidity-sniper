package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/antono/hoodsniper/internal/pnl"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// The Wallets view answers the project's open question: whose trades are worth
// copying. Loading is split in two because the costs differ by orders of
// magnitude — counting the ledger is ~350ms and local, while scoring needs a
// receipt per trade and gets rate-limited on a public node. Counting happens on
// entry; scoring only when asked.

// scoreCandidates bounds how many wallets one scoring pass covers.
const scoreCandidates = 12

type walletRow struct {
	Addr    common.Address
	Ledger  int // transactions seen in the ledger
	Scored  bool
	Summary pnl.Summary
	Err     string
}

type walletsMsg struct {
	rows    []walletRow
	scoring bool
	err     error
	note    string
}

type walletsView struct {
	rows       []walletRow
	cursor     int
	detail     bool
	loaded     bool
	loading    bool
	scoring    bool
	note       string
	err        string
	rpcURL     string
	ledgerPath string
}

func newWalletsView(rpcURL, ledgerPath string) walletsView {
	return walletsView{rpcURL: rpcURL, ledgerPath: ledgerPathOr(ledgerPath)}
}

func (v *walletsView) apply(m walletsMsg) {
	if m.rows != nil {
		v.rows = m.rows
		v.loaded = true
	}
	v.loading = false
	v.scoring = m.scoring
	v.note = m.note
	if m.err != nil {
		v.err = m.err.Error()
	}
}

func (v *walletsView) key(msg tea.KeyMsg, ledgerPath string) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < len(v.rows)-1 {
			v.cursor++
		}
	case "enter":
		if len(v.rows) > 0 {
			v.detail = !v.detail
		}
	case "esc":
		v.detail = false
	case "s":
		if !v.scoring && len(v.rows) > 0 {
			v.scoring = true
			v.note = "scoring…"
			return v.scoreCmd()
		}
	case "r":
		if !v.loading {
			v.loading = true
			return v.loadCmd(ledgerPath)
		}
	}
	return nil
}

// loadCmd counts each wallet's ledger history. Cheap and local.
func (v walletsView) loadCmd(path string) tea.Cmd {
	if path == "" {
		path = "ledger.jsonl"
	}
	return func() tea.Msg {
		counts, err := loadLedgerCounts(path)
		if err != nil {
			return walletsMsg{err: err}
		}
		rows := make([]walletRow, 0, len(counts))
		for a, n := range counts {
			rows = append(rows, walletRow{Addr: common.HexToAddress(a), Ledger: n})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Ledger > rows[j].Ledger })
		if len(rows) > 200 {
			rows = rows[:200]
		}
		return walletsMsg{rows: rows,
			note: fmt.Sprintf("%d wallets from %s — press s to score the top %d",
				len(counts), path, scoreCandidates)}
	}
}

// scoreCmd measures P&L for the busiest candidates.
//
// This is the expensive path and the one that fails on a rate-limited node, so
// each wallet's error is captured per row rather than aborting the pass — a
// wallet that could not be measured must never be displayed as a losing one.
func (v walletsView) scoreCmd() tea.Cmd {
	rows := make([]walletRow, len(v.rows))
	copy(rows, v.rows)
	rpcURL := v.rpcURL
	ledgerPath := v.ledgerPath

	return func() tea.Msg {
		ctx := context.Background()
		client, err := rpc.DialContext(ctx, rpcURL)
		if err != nil {
			return walletsMsg{err: err}
		}
		defer client.Close()

		byWallet, err := pnl.LoadLedger(ledgerPath)
		if err != nil {
			return walletsMsg{err: err}
		}

		n := scoreCandidates
		if n > len(rows) {
			n = len(rows)
		}
		for i := 0; i < n; i++ {
			txs := byWallet[rows[i].Addr]
			if len(txs) == 0 {
				continue
			}
			internal, _ := pnl.FetchInternalETH(rows[i].Addr, len(txs)*2)
			trades, err := pnl.Measure(ctx, client, rows[i].Addr, txs, internal)
			if err != nil {
				rows[i].Err = err.Error()
				continue
			}
			rows[i].Summary = pnl.Summarize(rows[i].Addr, trades)
			rows[i].Scored = true
		}

		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.Scored != b.Scored {
				return a.Scored
			}
			if !a.Scored {
				return a.Ledger > b.Ledger
			}
			return a.Summary.MatchedReturnPct() > b.Summary.MatchedReturnPct()
		})
		return walletsMsg{rows: rows, note: "scored — verdicts below the sample floor are withheld"}
	}
}

func (v walletsView) render(width, rows int) string {
	title := styleHeader.Render("wallets") +
		styleDim.Render("  ranked by measured P&L, not activity")

	if v.err != "" {
		return styleBox.Width(width).Render(title + "\n\n " +
			styleWarn.Render(v.err) + "\n\n" +
			styleDim.Render(" collect a ledger first:  go run ./cmd/collect --ledger ledger.jsonl"))
	}
	if !v.loaded {
		return styleBox.Width(width).Render(title + "\n\n " + styleDim.Render("loading ledger…"))
	}
	if v.detail && v.cursor < len(v.rows) {
		return styleBox.Width(width).Render(v.renderDetail(v.rows[v.cursor], width))
	}

	var b strings.Builder
	b.WriteString(title + "\n")
	if v.note != "" {
		b.WriteString(styleDim.Render(" "+v.note) + "\n")
	}
	b.WriteString(styleDim.Render(fmt.Sprintf("\n  %-44s %7s %6s %10s %8s %6s",
		"WALLET", "LEDGER", "TRIPS", "NET ETH", "RETURN", "WIN%")) + "\n")
	legend := styleDim.Render("  ✓ clears fee drag · ? too few round trips to call")

	body := rows - 6
	if body < 3 {
		body = 8
	}
	for i := 0; i < len(v.rows) && i < body; i++ {
		r := v.rows[i]
		line := fmt.Sprintf("  %-44s %7d %6s %10s %8s %6s",
			r.Addr.Hex(), r.Ledger, "—", "—", "—", "—")
		switch {
		case r.Err != "":
			// The row prefix is 55 columns; the rest is the error's budget.
			// Wrapping here would push every later row off the box.
			line = fmt.Sprintf("  %-44s %7d %s", r.Addr.Hex(), r.Ledger,
				styleWarn.Render("SKIPPED: "+truncate(r.Err, width-68)))
		case r.Scored:
			s := r.Summary
			ret, win, net := "—", "—", "—"
			if s.MatchedRoundTrips > 0 {
				ret = fmt.Sprintf("%+.1f%%", s.MatchedReturnPct())
				win = fmt.Sprintf("%.0f%%", s.WinRate())
				net = pnl.ETH(s.MatchedNet())
			}
			// A compact marker rather than a phrase: the full address has to
			// stay visible so it can be pasted into the watchlist, and spelling
			// out "clears fee drag" on every row overflows the box.
			mark := "  "
			switch {
			case s.CopyViable():
				mark = styleOK.Render(" ✓")
			case s.MatchedRoundTrips > 0 && s.MatchedRoundTrips < 3:
				mark = styleDim.Render(" ?")
			}
			line = fmt.Sprintf("  %-44s %7d %6d %10s %8s %6s%s",
				r.Addr.Hex(), r.Ledger, s.MatchedRoundTrips, net, ret, win, mark)
		}
		if i == v.cursor {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(legend + "\n")
	if v.scoring {
		b.WriteString(" " + styleWarn.Render("scoring — this needs a non-rate-limited RPC") + "\n")
	}
	return styleBox.Width(width).Render(b.String())
}

func (v walletsView) renderDetail(r walletRow, width int) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("wallet") + "  " + r.Addr.Hex() + "\n\n")
	b.WriteString(fmt.Sprintf(" ledger transactions : %d\n", r.Ledger))
	if r.Err != "" {
		b.WriteString("\n " + styleWarn.Render("not measurable: "+r.Err) + "\n")
		return b.String()
	}
	if !r.Scored {
		b.WriteString("\n " + styleDim.Render("not scored yet — press s on the list") + "\n")
		return b.String()
	}

	s := r.Summary
	b.WriteString(fmt.Sprintf(" trades              : %d (%d buys, %d sells)\n",
		s.Trades, s.Buys, s.Sells))
	b.WriteString(fmt.Sprintf(" complete round trips: %d", s.MatchedRoundTrips))
	if s.Unmeasurable > 0 {
		b.WriteString(styleDim.Render(fmt.Sprintf("   (%d unmeasurable — native-ETH V4)", s.Unmeasurable)))
	}
	b.WriteString("\n")
	if s.MatchedRoundTrips == 0 {
		b.WriteString("\n " + styleWarn.Render("no position has both legs inside the window — unmeasurable") + "\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf(" realised P&L        : %s ETH\n", pnl.ETH(s.MatchedNet())))
	b.WriteString(fmt.Sprintf(" return on spend     : %+.1f%%\n", s.MatchedReturnPct()))
	b.WriteString(fmt.Sprintf(" win rate            : %.0f%% (%d/%d)\n",
		s.WinRate(), s.MatchedWins, s.MatchedRoundTrips))

	verdict := styleBad.Render(fmt.Sprintf("does NOT clear the ~%.0f%% copier fee drag", pnl.FeeDragPct))
	if s.CopyViable() {
		verdict = styleOK.Render(fmt.Sprintf("clears the ~%.0f%% copier fee drag", pnl.FeeDragPct))
	}
	if s.MatchedRoundTrips < 3 {
		verdict = styleWarn.Render("too few round trips to call — one trade dominates the mean")
	}
	b.WriteString("\n " + verdict + "\n")

	b.WriteString("\n" + styleDim.Render(fmt.Sprintf("  %-44s %10s %10s %10s",
		"TOKEN", "SPENT", "RECV", "NET")) + "\n")
	for i, p := range s.Positions {
		if i >= 12 {
			break
		}
		status := "closed"
		switch {
		case p.Sells == 0:
			status = "open"
		case p.Buys == 0:
			status = "truncated"
		}
		b.WriteString(fmt.Sprintf("  %-44s %10s %10s %10s  %s\n",
			p.Token.Hex(), pnl.ETH(p.Spent), pnl.ETH(p.Received),
			pnl.ETH(p.Net()), styleDim.Render(status)))
	}
	b.WriteString("\n" + styleDim.Render(" esc to go back") + "\n")
	return b.String()
}

// loadLedgerCounts tallies transactions per wallet without building the full
// transaction list, which keeps the Ledger and Wallets views cheap to open.
func loadLedgerCounts(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	counts := map[string]int{}
	seen := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var e struct {
			Hash string `json:"hash"`
			From string `json:"from"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.Hash == "" {
			continue
		}
		if _, dup := seen[e.Hash]; dup {
			continue
		}
		seen[e.Hash] = struct{}{}
		counts[e.From]++
	}
	return counts, sc.Err()
}

func ledgerPathOr(p string) string {
	if p == "" {
		return "ledger.jsonl"
	}
	return p
}

func truncate(s string, n int) string {
	if n < 8 {
		n = 8
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
