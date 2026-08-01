// Command collect records every router swap on the chain to an append-only
// ledger, so wallet scoring stops being limited by the explorer's window.
//
// Why this exists: ranking wallets by measured P&L is the right criterion, but
// the explorer serves only ~50 transactions per address, which yields 2-6
// complete round trips per wallet. At that sample size the ranking is noise —
// re-measuring a shortlist flipped one wallet from +67.7% to -19.4%. Separating
// skill from luck needs dozens of round trips, which needs history nobody is
// serving. The feed carries it for free; this writes it down.
//
// Run it for days. It is cheap: no receipts, no state reads, one WebSocket.
//
//	go run ./cmd/collect --ledger ledger.jsonl
//	go run ./cmd/scout --ledger ledger.jsonl     # score once it has depth
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/antono/hoodsniper/internal/feed"
	"github.com/antono/hoodsniper/internal/pnl"
)

func main() {
	var (
		feedURL = flag.String("feed", "mainnet", "'mainnet', 'testnet', or a ws:// URL")
		chainID = flag.Int64("chain-id", chain.MainnetChainID, "L2 chain id")
		path    = flag.String("ledger", "ledger.jsonl", "append-only ledger path")
		seconds = flag.Int("seconds", 0, "stop after N seconds (0 = until Ctrl-C)")
		every   = flag.Duration("progress", 5*time.Minute, "how often to log progress")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ledger, err := pnl.OpenLedger(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "opening ledger:", err)
		os.Exit(1)
	}
	defer ledger.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer cancel()
	}

	url := *feedURL
	switch url {
	case "mainnet":
		url = feed.MainnetFeed
	case "testnet":
		url = feed.TestnetFeed
	}
	client := feed.NewClient(url, *chainID, log)

	var seen, recorded atomic.Uint64
	start := time.Now()
	log.Info("collecting", "ledger", *path, "feed", url)

	// Periodic progress, so a multi-day run shows it is alive without
	// printing a line per swap.
	go func() {
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Info("progress", "recorded", recorded.Load(),
					"seen", seen.Load(), "elapsed", time.Since(start).Round(time.Second))
			}
		}
	}()

	runErr := client.Run(ctx, func(b feed.Block) {
		for _, tx := range b.Txs {
			seen.Add(1)
			to := tx.To()
			if to == nil || (!chain.IsKnownRouter(*to) && !decode.IsBotRouter(*to)) {
				continue
			}
			from, err := client.Sender(tx)
			if err != nil {
				continue
			}

			e := pnl.Entry{
				At: b.ReceivedAt, Seq: b.SeqNum, Hash: tx.Hash().Hex(),
				From: from.Hex(), To: to.Hex(), Value: tx.Value().String(),
			}
			// Decode when we can — it costs nothing and makes the ledger
			// readable without re-deriving intent later. A failure is fine:
			// the hash alone is enough to fetch a receipt at scoring time.
			if intent, err := decode.Swap(tx); err == nil {
				e.Token = intent.Token().Hex()
				e.Dir = string(intent.Direction)
				e.Amount = intent.AmountIn.String()
			} else if bot, err := decode.BotRouterSwap(tx); err == nil {
				e.Dir = string(bot.Direction)
				e.Amount = bot.AmountIn.String()
			}

			if err := ledger.Append(e); err != nil {
				log.Error("ledger append failed", "err", err)
				continue
			}
			recorded.Add(1)
		}
	})

	if ctx.Err() == nil && runErr != nil {
		fmt.Fprintln(os.Stderr, "feed error:", runErr)
	}
	fmt.Printf("\n─── collector summary ───\n")
	fmt.Printf("ran        %s\n", time.Since(start).Round(time.Second))
	fmt.Printf("txs seen   %d\n", seen.Load())
	fmt.Printf("recorded   %d router swaps\n", recorded.Load())
	fmt.Printf("ledger     %s\n", *path)
}
