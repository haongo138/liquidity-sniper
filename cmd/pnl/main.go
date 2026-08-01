// Command pnl measures whether specific wallets actually make money.
//
// Use cmd/scout to FIND profitable wallets; use this to inspect ones you
// already have, with per-position detail.
//
//	go run ./cmd/pnl 0x85b6... 0x4d96...
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/antono/hoodsniper/internal/pnl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

const defaultRPC = "https://rpc.mainnet.chain.robinhood.com"

func main() {
	rpcURL := flag.String("rpc", defaultRPC, "RPC endpoint")
	maxTx := flag.Int("max", 60, "max transactions to examine per wallet")
	flag.Parse()

	wallets := flag.Args()
	if len(wallets) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pnl [--rpc URL] [--max N] <wallet> [wallet...]")
		os.Exit(2)
	}

	ctx := context.Background()
	client, err := rpc.DialContext(ctx, *rpcURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dialing rpc:", err)
		os.Exit(1)
	}
	defer client.Close()

	for _, w := range wallets {
		if !common.IsHexAddress(w) {
			fmt.Fprintf(os.Stderr, "skipping %q: not an address\n", w)
			continue
		}
		addr := common.HexToAddress(w)
		txs, err := pnl.FetchTxs(addr, *maxTx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", w, err)
			continue
		}
		// Ignore the error: internal traces are an enhancement, and a wallet
		// that never received native ETH is still fully measurable without them.
		internal, _ := pnl.FetchInternalETH(addr, *maxTx*4)
		trades, err := pnl.Measure(ctx, client, addr, txs, internal)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", w, err)
			continue
		}
		print(pnl.Summarize(addr, trades))
	}
}

func print(s pnl.Summary) {
	fmt.Printf("\n=== %s ===\n", s.Wallet.Hex())
	if s.Trades == 0 {
		fmt.Println("  no measurable trades")
		return
	}
	fmt.Printf("  trades          : %d (%d buys, %d sells) over %d tokens\n",
		s.Trades, s.Buys, s.Sells, len(s.Positions))
	fmt.Printf("  window          : %s\n", s.Window.Round(time.Minute))

	fmt.Printf("\n  %-44s %10s %10s %10s %s\n", "TOKEN", "SPENT", "RECV", "NET", "STATUS")
	for _, p := range s.Positions {
		status := "closed"
		switch {
		case p.Sells == 0:
			status = "OPEN — buy only, P&L unknown"
		case p.Buys == 0:
			status = "TRUNCATED — sold a pre-existing bag"
		case p.Bought.Cmp(p.Sold) > 0:
			status = "partial exit"
		}
		fmt.Printf("  %-44s %10s %10s %10s %s\n",
			p.Token.Hex(), pnl.ETH(p.Spent), pnl.ETH(p.Received), pnl.ETH(p.Net()), status)
	}

	// Only complete round trips are trustworthy. A buy whose sell fell outside
	// the window looks like a total loss; a sell whose buy fell outside looks
	// like free money. Neither belongs in a P&L figure.
	fmt.Printf("\n  complete round trips : %d\n", s.MatchedRoundTrips)
	if s.MatchedRoundTrips == 0 {
		fmt.Println("  VERDICT: unmeasurable — no position has both legs in the window.")
		return
	}
	fmt.Printf("  realised P&L    : %s ETH\n", pnl.ETH(s.MatchedNet()))
	fmt.Printf("  return on spend : %+.1f%%\n", s.MatchedReturnPct())
	fmt.Printf("  win rate        : %.0f%% (%d/%d)\n", s.WinRate(), s.MatchedWins, s.MatchedRoundTrips)

	if s.CopyViable() {
		fmt.Printf("  VERDICT: clears the ~%.0f%% copier fee drag.\n", pnl.FeeDragPct)
	} else {
		fmt.Printf("  VERDICT: does NOT clear the ~%.0f%% fee drag — copying loses money.\n", pnl.FeeDragPct)
	}
}
