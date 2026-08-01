package monitor

import (
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/antono/hoodsniper/internal/pnl"
	"github.com/ethereum/go-ethereum/common"
)

// TestPreview renders every tab to stdout so the layout can be reviewed without
// a terminal. Bubble Tea needs a real TTY, but View() is a pure function of a
// snapshot, so the frames below are exactly what the UI draws.
//
//	go test ./internal/monitor -run TestPreview -v
func TestPreview(t *testing.T) {
	if os.Getenv("PREVIEW") == "" {
		t.Skip("set PREVIEW=1 to render the TUI frames")
	}

	s := New("shadow.jsonl", false, 1)
	s.SetConnected(true)
	s.SetCache(49, 35)
	s.SetConfig(Config{
		Path: "kol.yaml", Feed: "mainnet",
		RPC:     "https://rpc.mainnet.chain.robinhood.com",
		ChainID: 4663, TradeSizeETH: 0.01, LedgerPath: "ledger.jsonl",
		Watch: []string{"0x3c86D511803AfB9b7478A15527274f9B464bab89"},
		Filters: []ConfigRow{
			{Name: "min_liquidity_eth", Value: "0.25", Note: "blind to V4 — see README KNOWN BUG"},
			{Name: "require_lp_secured", Value: "false", Note: "V2 only; reports n/a on V3"},
			{Name: "min_trade_eth", Value: "0.0005", Note: "buys only; a sell's input is in tokens"},
			{Name: "allow_sells", Value: "true"},
		},
	})

	// Numbers taken from the real shadow run so the preview is representative.
	for i := 0; i < 40; i++ {
		s.ObserveBlock(24382700+uint64(i), 12)
		s.ObserveDecode(true)
	}
	s.ObserveMatch()
	for _, d := range []Decision{
		{At: time.Now(), Approved: true, Direction: "buy", Seq: 24382770,
			Token: "0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64", LatencyMS: 260, Reason: "approved"},
		{At: time.Now(), Approved: true, Direction: "buy", Seq: 24382758,
			Token: "0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64", LatencyMS: 259, Reason: "approved"},
		{At: time.Now(), Approved: false, Direction: "sell", Seq: 24368725,
			Token: "0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9", LatencyMS: 826,
			Reason: "min_liquidity: 0.2200 ETH below floor 0.2500 ETH"},
		{At: time.Now(), Approved: true, Direction: "buy", Seq: 24382740,
			Token: "0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64", LatencyMS: 1686, Reason: "approved"},
		{At: time.Now(), Approved: false, Direction: "sell", Seq: 24369001,
			Token: "0x26879c326b7664F9FeE7fa83E9274EEA800eaD7c", LatencyMS: 884,
			Reason: "direction: sell, and allow_sells is off"},
	} {
		s.Add(d)
	}
	for _, tx := range []RawTx{
		{Seq: 24382770, From: "0x85b605b47a5323912615cb8Af834BB1c4716b794",
			To: "0xCaf681a66D020601342297493863E78C959E5cb2", Selector: "0x04e45aaf", Decoded: true},
		{Seq: 24382771, From: "0x5C87ecc36BB97380469be09ef95dC4DAf50165ec",
			To: "0xCaf681a66D020601342297493863E78C959E5cb2", Selector: "0xac9650d8", Decoded: true},
		{Seq: 24382772, From: "0x4d9644d05FE2123b4eAfa8d7fD31B0EA430726f3",
			To: "0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc", Selector: "0x4d819a2a", Decoded: true},
		{Seq: 24382773, From: "0xe0e2c78bB80763A3E5E685F0d64322B9F6292268",
			To: "0x8876789976dEcBfCbBbe364623C63652db8C0904", Selector: "0x24856bc3", Decoded: false},
	} {
		for i := 0; i < 40; i++ {
			s.ObserveRouterTx(tx)
		}
	}

	m := model{state: s, snap: s.Snapshot(), width: 104, height: 30}
	m.wallets = walletsView{loaded: true, rpcURL: "…", ledgerPath: "ledger.jsonl",
		note: "16890 wallets from ledger.jsonl — press s to score the top 12",
		rows: previewRows()}
	m.ledger = ledgerView{path: "ledger.jsonl", msg: ledgerMsg{
		path: "ledger.jsonl", bytes: 85_899_345, entries: 253_961, wallets: 16_890,
		deep: map[int]int{5: 7739, 10: 6134, 20: 2794, 40: 816, 100: 291, 200: 195}}}

	for i, name := range tabNames {
		m.active = tab(i)
		if tab(i) == tabFeed {
			m.feed.discover = true // show the tally, it is the more useful of the two
		}
		fmt.Printf("\n╔══ TAB %d: %s %s\n", i+1, name,
			"══════════════════════════════════════════════════════")
		fmt.Println(m.View())
	}

	// Also render the Wallets drill-in, which is a separate frame.
	m.active = tabWallets
	m.wallets.detail = true
	m.wallets.cursor = 0
	fmt.Printf("\n╔══ TAB 2: Wallets — drill-in (enter) %s\n",
		"═════════════════════════════════════")
	fmt.Println(m.View())
}

// previewRows builds a scored wallet table using figures measured this session,
// including a rate-limited failure so the SKIPPED rendering is visible.
func previewRows() []walletRow {
	mk := func(hex string, ledger, trips, wins int, spentETH, recvETH float64) walletRow {
		s := pnlSummaryFor(hex, trips, wins, spentETH, recvETH)
		return walletRow{Addr: addrOf(hex), Ledger: ledger, Scored: true, Summary: s}
	}
	return []walletRow{
		mk("0x3c86D511803AfB9b7478A15527274f9B464bab89", 312, 6, 4, 0.696, 1.087),
		mk("0x5638484ba2d2F1D1D35020572B0Aa439a9869192", 288, 5, 3, 0.420, 0.532),
		mk("0x85b605b47a5323912615cb8Af834BB1c4716b794", 425, 8, 4, 1.892, 1.808),
		{Addr: addrOf("0x713378ACC7aaB5536646C41AfBE6718577998a83"), Ledger: 6156,
			Err: "receipt batch 100-150 of 6156: 429 Too Many Requests"},
		{Addr: addrOf("0xe0e2c78bB80763A3E5E685F0d64322B9F6292268"), Ledger: 424,
			Scored: true, Summary: pnlSummaryFor("0xe0e2c78bB80763A3E5E685F0d64322B9F6292268", 2, 2, 0.02, 0.0225)},
	}
}

func addrOf(h string) common.Address { return common.HexToAddress(h) }

func wei(f float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil)
	return v
}

func pnlSummaryFor(hex string, trips, wins int, spent, recv float64) pnl.Summary {
	return pnl.Summary{
		Wallet: addrOf(hex), Trades: trips * 3, Buys: trips * 2, Sells: trips,
		MatchedSpent: wei(spent), MatchedReceived: wei(recv),
		MatchedRoundTrips: trips, MatchedWins: wins,
		Positions: []pnl.Position{{
			Token: addrOf("0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64"),
			Spent: wei(0.15), Received: wei(0.147), Bought: big.NewInt(1), Sold: big.NewInt(1),
			Buys: 5, Sells: 1,
		}, {
			Token: addrOf("0x5DD7184e28121837Ede59E7a3185C8697f90b172"),
			Spent: wei(0.09), Received: wei(0.104), Bought: big.NewInt(1), Sold: big.NewInt(1),
			Buys: 2, Sells: 2,
		}},
	}
}
