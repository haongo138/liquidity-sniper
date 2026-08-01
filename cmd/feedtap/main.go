// Command feedtap is Phase 0 of hoodsniper: the go/no-go gate.
//
// It taps the Robinhood Chain sequencer feed, prints transactions from watched
// wallets, and measures how much earlier the feed sees a transaction than the
// RPC does. That latency edge is the entire premise of the project — if it is
// small, nothing downstream is worth building.
//
//	go run ./cmd/feedtap --rpc https://... --seconds 3600
//	go run ./cmd/feedtap --rpc https://... --watch 0xabc...,0xdef...
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antono/hoodsniper/internal/bench"
	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/antono/hoodsniper/internal/discover"
	"github.com/antono/hoodsniper/internal/feed"
	"github.com/ethereum/go-ethereum/common"
)

// gate thresholds from SPEC.md.
const (
	gateFloor   = 30 * time.Millisecond
	gateProceed = 100 * time.Millisecond
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		feedURL    = flag.String("feed", feed.LocalRelay, "feed endpoint: 'mainnet', 'testnet', or a ws:// URL (default: local relay)")
		rpcURL     = flag.String("rpc", "", "RPC endpoint for the latency probe (required for the gate measurement)")
		chainID    = flag.Int64("chain-id", feed.MainnetChainID, "L2 chain id (4663 mainnet, 46630 testnet)")
		watchRaw   = flag.String("watch", "", "comma-separated wallet addresses to report on")
		seconds    = flag.Int("seconds", 0, "stop after N seconds (0 = run until Ctrl-C)")
		probePer   = flag.Int("probe-per-block", 1, "how many txs per block to latency-probe (0 disables probing)")
		probeEvery = flag.Duration("probe-interval", 5*time.Millisecond, "RPC poll cadence; bounds measurement resolution")
		probeCap   = flag.Int("probe-concurrency", 8, "max in-flight latency probes")
		verboseTxs = flag.Bool("all", false, "print every tx, not just watched wallets")
		discoverN  = flag.Int("discover", 0, "tally the top N (contract, selector) pairs by volume instead of printing txs")
		showSwaps  = flag.Bool("swaps", false, "decode and print swaps routed through known routers")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	url := resolveFeed(*feedURL)
	watch := parseWatchlist(*watchRaw)
	if len(watch) > 0 {
		log.Info("watching wallets", "count", len(watch))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer cancel()
	}

	var probe *bench.Probe
	if *rpcURL != "" && *probePer > 0 {
		p, err := bench.NewProbe(ctx, *rpcURL, *probeEvery, 30*time.Second, *probeCap)
		if err != nil {
			return fmt.Errorf("dialing rpc: %w", err)
		}
		probe = p
		log.Info("latency probe active", "rpc_poll", *probeEvery, "per_block", *probePer)
	} else {
		log.Warn("no --rpc given: printing feed only, NOT measuring the gate")
	}

	client := feed.NewClient(url, *chainID, log)

	var tally *discover.Tally
	if *discoverN > 0 {
		tally = discover.New()
		log.Info("discovery mode", "top_n", *discoverN)
	}

	var blocks, txs, hits, routed, decoded atomic.Uint64
	start := time.Now()

	err := client.Run(ctx, func(b feed.Block) {
		blocks.Add(1)
		txs.Add(uint64(len(b.Txs)))

		for i, tx := range b.Txs {
			// Sender recovery is the expensive field (~0.1ms). Skip it entirely
			// when nothing needs it.
			if len(watch) > 0 || *verboseTxs || tally != nil || *showSwaps {
				from, err := client.Sender(tx)
				if err != nil {
					continue
				}
				if tally != nil {
					tally.Observe(tx, from)
				}
				if *showSwaps {
					if to := tx.To(); to != nil && chain.IsKnownRouter(*to) {
						routed.Add(1)
						if intent, err := decode.Swap(tx); err == nil {
							decoded.Add(1)
							printSwap(b, from, intent)
						}
					}
				}
				if watch[from] {
					hits.Add(1)
					printHit(b, tx, from)
				} else if *verboseTxs {
					printTx(b, tx, from)
				}
			}

			if probe != nil && i < *probePer {
				probe.Sample(ctx, tx.Hash(), b.ReceivedAt)
			}
		}
	})

	if ctx.Err() == nil && err != nil {
		return err
	}

	elapsed := time.Since(start)
	fmt.Printf("\n─── feedtap summary ───\n")
	fmt.Printf("ran            %s\n", elapsed.Round(time.Second))
	fmt.Printf("blocks         %d\n", blocks.Load())
	fmt.Printf("transactions   %d\n", txs.Load())
	if len(watch) > 0 {
		fmt.Printf("watched hits   %d\n", hits.Load())
	}

	if tally != nil {
		reportDiscovery(tally, *discoverN)
	}

	if *showSwaps {
		r, d := routed.Load(), decoded.Load()
		fmt.Printf("router-bound   %d\n", r)
		fmt.Printf("decoded        %d", d)
		if r > 0 {
			fmt.Printf("  (%.1f%% of router-bound calls)", 100*float64(d)/float64(r))
		}
		fmt.Println()
	}

	if probe == nil {
		fmt.Printf("\ngate           NOT MEASURED (re-run with --rpc)\n")
		return nil
	}

	probe.Wait()
	reportGate(probe.Stats())
	return nil
}

// printSwap renders one decoded swap. Amounts stay in wei — this is a decoder
// validation view, not a trading UI.
func printSwap(b feed.Block, from common.Address, s decode.SwapIntent) {
	fmt.Printf("%-5s %-16s seq %d  %s\n        in  %s %s\n        out %s (min %s)\n",
		s.Direction, s.Venue, b.SeqNum, from.Hex(),
		s.AmountIn.String(), s.TokenIn.Hex(),
		s.TokenOut.Hex(), s.AmountOutMin.String())
}

// reportDiscovery prints the busiest (contract, selector) pairs. Distinct
// senders separates a real router from one bot hammering its own contract.
func reportDiscovery(t *discover.Tally, n int) {
	fmt.Printf("\n─── top %d destinations by call volume (%d keyed calls) ───\n", n, t.Total())
	fmt.Printf("%-44s %-12s %8s %8s\n", "CONTRACT", "SELECTOR", "CALLS", "SENDERS")
	for _, c := range t.Top(n) {
		fmt.Printf("%-44s 0x%-10x %8d %8d\n", c.To.Hex(), c.Selector, c.N, c.Senders)
	}
}

// reportGate prints the measurement and the SPEC.md verdict.
func reportGate(s bench.Stats) {
	fmt.Printf("\n─── latency edge (feed vs RPC) ───\n")
	if s.N == 0 {
		fmt.Printf("no samples (%d misses) — cannot evaluate the gate\n", s.Misses)
		return
	}
	fmt.Printf("samples        %d (%d timed out)\n", s.N, s.Misses)
	fmt.Printf("min            %s\n", s.Min.Round(time.Millisecond))
	fmt.Printf("p50            %s\n", s.P50.Round(time.Millisecond))
	fmt.Printf("p95            %s\n", s.P95.Round(time.Millisecond))
	fmt.Printf("max            %s\n", s.Max.Round(time.Millisecond))

	fmt.Printf("\nverdict        ")
	switch {
	case s.P50 < gateFloor:
		fmt.Printf("STOP — p50 %s is below the %s floor.\n", s.P50.Round(time.Millisecond), gateFloor)
		fmt.Printf("               There is no tradeable edge. Do not build Phases 1-4.\n")
	case s.P50 < gateProceed:
		fmt.Printf("MARGINAL — p50 %s sits between %s and %s.\n", s.P50.Round(time.Millisecond), gateFloor, gateProceed)
		fmt.Printf("               Sample longer before committing.\n")
	default:
		fmt.Printf("PROCEED — p50 %s clears the %s bar.\n", s.P50.Round(time.Millisecond), gateProceed)
	}
}

func printHit(b feed.Block, tx interface{ Hash() common.Hash }, from common.Address) {
	fmt.Printf("★ seq %d  %s  from %s\n", b.SeqNum, tx.Hash().Hex(), from.Hex())
}

func printTx(b feed.Block, tx interface{ Hash() common.Hash }, from common.Address) {
	fmt.Printf("  seq %d  %s  from %s\n", b.SeqNum, tx.Hash().Hex(), from.Hex())
}

// resolveFeed expands the shorthand names to real endpoints.
func resolveFeed(v string) string {
	switch v {
	case "mainnet":
		return feed.MainnetFeed
	case "testnet":
		return feed.TestnetFeed
	default:
		return v
	}
}

// parseWatchlist builds a set from a comma-separated address list.
func parseWatchlist(raw string) map[common.Address]bool {
	out := map[common.Address]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out[common.HexToAddress(part)] = true
	}
	return out
}
