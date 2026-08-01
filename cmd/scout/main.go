// Command scout finds wallets worth copying by measuring profitability rather
// than assuming activity implies skill.
//
// This is the tool the project was missing. The first three wallets were chosen
// because they traded often; measurement later showed the best of them at
// -4.45% and another at 0-for-4 on round trips. Copying either would convert
// their loss into a larger one, because a copier pays ~4% per round trip in
// fees before any slippage.
//
// Method: enumerate every wallet that traded through a router, reconstruct each
// one's P&L from receipt logs, and rank by return on COMPLETE round trips only
// — positions where both the buy and the sell fall inside the observed window.
// Truncated positions are excluded rather than guessed at.
//
//	go run ./cmd/scout --candidates 400 --top 15
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/antono/hoodsniper/internal/pnl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

const defaultRPC = "https://rpc.mainnet.chain.robinhood.com"

// routers whose traffic identifies a trading wallet.
var routers = []common.Address{
	common.HexToAddress("0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc"), // bot router
	common.HexToAddress("0xCaf681a66D020601342297493863E78C959E5cb2"), // SwapRouter02
	common.HexToAddress("0x8876789976dEcBfCbBbe364623C63652db8C0904"), // UniversalRouter
}

func main() {
	rpcURL := flag.String("rpc", defaultRPC, "RPC endpoint")
	candidates := flag.Int("candidates", 300, "router transactions to scan for candidate wallets")
	minTrades := flag.Int("min-trades", 4, "ignore wallets with fewer transactions than this")
	maxPerWallet := flag.Int("max-per-wallet", 60, "transactions to examine per wallet")
	top := flag.Int("top", 15, "wallets to evaluate, most active first")
	workers := flag.Int("workers", 4, "concurrent wallet evaluations")
	ledger := flag.String("ledger", "", "score from a cmd/collect ledger instead of the explorer")
	minTrips := flag.Int("min-trips", 3, "minimum complete round trips to report a verdict")
	flag.Parse()

	ctx := context.Background()
	client, err := rpc.DialContext(ctx, *rpcURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dialing rpc:", err)
		os.Exit(1)
	}
	defer client.Close()

	var (
		ranked     []candidate
		fromLedger map[common.Address][]pnl.Tx
	)
	if *ledger != "" {
		byWallet, err := pnl.LoadLedger(*ledger)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reading ledger:", err)
			os.Exit(1)
		}
		fromLedger = byWallet
		counts := map[common.Address]int{}
		for w, txs := range byWallet {
			counts[w] = len(txs)
		}
		ranked = byActivity(counts, *minTrades)
		fmt.Printf("ledger %s: %d wallets, %d with >=%d transactions\n",
			*ledger, len(byWallet), len(ranked), *minTrades)
	} else {
		fmt.Printf("scanning %d router transactions for candidate wallets...\n", *candidates)
		counts := discover(*candidates)
		if len(counts) == 0 {
			fmt.Fprintln(os.Stderr, "no candidates found")
			os.Exit(1)
		}
		ranked = byActivity(counts, *minTrades)
		fmt.Printf("found %d wallets, evaluating the %d most active\n", len(counts), min(len(ranked), *top))
	}
	if len(ranked) == 0 {
		fmt.Fprintln(os.Stderr, "no candidates met the activity threshold")
		os.Exit(1)
	}
	if len(ranked) > *top {
		ranked = ranked[:*top]
	}
	fmt.Println()

	results := evaluate(ctx, client, ranked, *maxPerWallet, *workers, fromLedger)
	minRoundTrips = *minTrips

	// Rank by return on matched round trips. Wallets without enough complete
	// round trips sort last: an unmeasured wallet is not a good wallet.
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if (a.MatchedRoundTrips >= 3) != (b.MatchedRoundTrips >= 3) {
			return a.MatchedRoundTrips >= 3
		}
		return a.MatchedReturnPct() > b.MatchedReturnPct()
	})

	report(results)
}

// discover enumerates wallets that sent transactions to any known router.
func discover(limit int) map[common.Address]int {
	counts := map[common.Address]int{}
	per := limit / len(routers)
	if per < 50 {
		per = 50
	}
	for _, r := range routers {
		txs, err := pnl.FetchIncoming(r, per)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  (router %s: %v)\n", r.Hex()[:10], err)
			continue
		}
		for _, t := range txs {
			if t.From == "" {
				continue
			}
			counts[common.HexToAddress(t.From)]++
		}
	}
	return counts
}

type candidate struct {
	addr common.Address
	seen int
}

func byActivity(counts map[common.Address]int, min int) []candidate {
	var out []candidate
	for a, n := range counts {
		if n >= min {
			out = append(out, candidate{a, n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seen > out[j].seen })
	return out
}

// minRoundTrips is the sample floor below which a verdict is withheld. Two to
// six round trips cannot separate skill from luck — one trade dominates the
// mean — so anything under this is reported as unproven rather than ranked.
var minRoundTrips = 3

// evaluate measures each candidate concurrently. When fromLedger is non-nil the
// transaction list comes from the collector's ledger, which is unbounded by the
// explorer's rolling window; receipts are still fetched per wallet.
func evaluate(ctx context.Context, client *rpc.Client, cands []candidate,
	maxTx, workers int, fromLedger map[common.Address][]pnl.Tx) []pnl.Summary {

	var (
		mu   sync.Mutex
		out  []pnl.Summary
		wg   sync.WaitGroup
		sem  = make(chan struct{}, workers)
		done int
	)
	for _, c := range cands {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var txs []pnl.Tx
			if fromLedger != nil {
				// Use the wallet's entire ledger history. The per-wallet cap
				// exists for the explorer path, where each transaction costs a
				// request; here receipts are batched, so more history is nearly
				// free and is the whole reason the ledger exists. Truncating to
				// the most recent N also slices positions in half, which is
				// exactly the bias the ledger was built to remove.
				txs = fromLedger[c.addr]
			} else {
				var err error
				txs, err = pnl.FetchTxs(c.addr, maxTx)
				if err != nil {
					return
				}
			}
			if len(txs) == 0 {
				return
			}
			internal, _ := pnl.FetchInternalETH(c.addr, maxTx*4)
			trades, err := pnl.Measure(ctx, client, c.addr, txs, internal)
			if err != nil {
				// A dropped wallet must say why. Returning quietly here is what
				// made a failing receipt fetch look like a wallet with no trades.
				mu.Lock()
				done++
				fmt.Fprintf(os.Stderr, "  [%d/%d] %s SKIPPED: %v\n",
					done, len(cands), c.addr.Hex()[:12], err)
				mu.Unlock()
				return
			}
			s := pnl.Summarize(c.addr, trades)

			mu.Lock()
			out = append(out, s)
			done++
			fmt.Printf("  [%d/%d] %s  %d trades, %d round trips\n",
				done, len(cands), c.addr.Hex()[:12], s.Trades, s.MatchedRoundTrips)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out
}

func report(results []pnl.Summary) {
	fmt.Printf("\n%-44s %7s %6s %6s %9s %8s %6s\n",
		"WALLET", "TRADES", "TRIPS", "UNMSR", "NET ETH", "RETURN", "WIN%")
	fmt.Println(repeat('-', 96))

	var viable []pnl.Summary
	unmeasured := 0
	for _, s := range results {
		ret, win, net := "n/a", "n/a", "n/a"
		if s.MatchedRoundTrips > 0 {
			ret = fmt.Sprintf("%+.1f%%", s.MatchedReturnPct())
			win = fmt.Sprintf("%.0f%%", s.WinRate())
			net = pnl.ETH(s.MatchedNet())
		} else if s.Unmeasurable > 0 {
			unmeasured++
		}
		mark := ""
		switch {
		case s.MatchedRoundTrips > 0 && s.MatchedRoundTrips < minRoundTrips:
			mark = "  (unproven — too few round trips)"
		case s.CopyViable() && s.MatchedRoundTrips >= minRoundTrips:
			mark = "  <-- clears fee drag"
			viable = append(viable, s)
		}
		fmt.Printf("%-44s %7d %6d %6d %9s %8s %6s%s\n",
			s.Wallet.Hex(), s.Trades, s.MatchedRoundTrips, s.Unmeasurable,
			net, ret, win, mark)
	}
	if unmeasured > 0 {
		fmt.Printf("\nUNMSR = round trips whose proceeds left no Transfer log (native-ETH V4\n")
		fmt.Printf("pools pay in real ETH, not WETH). %d wallets are unmeasurable this way —\n", unmeasured)
		fmt.Printf("that is an unknown, NOT a loss. Needs internal-transaction traces.\n")
	}

	fmt.Printf("\n%d of %d wallets clear the ~%.0f%% fee drag on >=3 complete round trips.\n",
		len(viable), len(results), pnl.FeeDragPct)
	if len(viable) == 0 {
		fmt.Println("\nNo wallet here is worth copying. Widen the scan, or reconsider the")
		fmt.Println("strategy: copying a break-even trader is a loss once fees are paid.")
		return
	}
	fmt.Println("\nAdd to hoodsniper.yaml `watch:` — but re-measure before trusting,")
	fmt.Println("since a short window can flatter a lucky wallet.")
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

var _ = time.Now
