// Command hoodsniper watches KOL wallets on Robinhood Chain and copies their
// swaps.
//
// It is a backrunner, not a front-runner. The sequencer orders and executes
// before it broadcasts, so the watched wallet's transaction is already settled
// when we see it. The edge is over everyone else reacting via RPC — measured at
// several hundred milliseconds in PHASE0-RESULTS.md — never over the wallet
// being copied.
//
//	hoodsniper --config hoodsniper.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/config"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/antono/hoodsniper/internal/feed"
	"github.com/antono/hoodsniper/internal/filter"
	"github.com/antono/hoodsniper/internal/ladder"
	"github.com/antono/hoodsniper/internal/monitor"
	"github.com/antono/hoodsniper/internal/shadow"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// stateReadTimeout bounds the tier-1 batch. Past this the race is lost anyway,
// so failing fast beats holding a slot open.
const stateReadTimeout = 3 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "hoodsniper.yaml", "path to the YAML config")
	seconds := flag.Int("seconds", 0, "stop after N seconds (0 = until Ctrl-C)")
	tui := flag.Bool("tui", false, "render a live terminal UI instead of streaming lines")
	ledgerPath := flag.String("ledger", "ledger.jsonl", "ledger used by the TUI's Wallets and Ledger views")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// The TUI owns the screen, so log lines must not be written to it. Send
	// them to a file instead of interleaving with the rendered frames.
	logDst := io.Writer(os.Stderr)
	if *tui {
		lf, err := os.OpenFile("hoodsniper.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer lf.Close()
		logDst = lf
	}
	log := slog.New(slog.NewTextHandler(logDst, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Live execution is Phase 2. Refuse rather than silently shadow-trading
	// while the operator believes real orders are going out.
	if cfg.Live {
		return fmt.Errorf("live: true is not supported yet — execution lands in Phase 2. " +
			"Set live: false to run in shadow mode")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
		defer cancel()
	}

	rpcClient, err := chain.Dial(ctx, cfg.RPC)
	if err != nil {
		return fmt.Errorf("dialing rpc: %w", err)
	}
	defer rpcClient.Close()

	rec, err := shadow.NewRecorder(cfg.ShadowLog)
	if err != nil {
		return fmt.Errorf("opening shadow log: %w", err)
	}
	defer rec.Close()

	watch := cfg.WatchSet()
	cache := chain.NewCache()
	// Collapses a laddered entry into one signal. Mirroring five clips of the
	// same position pays the router's 1% fee five times, which alone exceeds
	// the edge — see internal/ladder.
	ladders := ladder.New(cfg.LadderWindow())
	fcfg := cfg.FilterConfig()
	tradeSize := cfg.TradeSizeWei()
	client := feed.NewClient(resolveFeed(cfg.Feed), cfg.ChainID, log)

	log.Info("shadow mode — no transactions will be signed",
		"watching", len(watch), "trade_size_eth", cfg.TradeSizeETH, "log", cfg.ShadowLog)

	state := monitor.New(cfg.ShadowLog, cfg.Live, len(watch))
	state.SetConfig(monitorConfig(*cfgPath, *ledgerPath, cfg))
	var seen, matched atomic.Uint64
	start := time.Now()

	handle := func(b feed.Block) {
		state.SetConnected(true)
		state.ObserveBlock(b.SeqNum, len(b.Txs))
		state.SetCache(cacheStats(cache))
		seen.Add(uint64(len(b.Txs)))
		for _, tx := range b.Txs {
			to := tx.To()
			// Tier 0, cheapest first: a non-router destination costs one map
			// lookup and skips the expensive sender recovery entirely.
			if to == nil || (!chain.IsKnownRouter(*to) && !decode.IsBotRouter(*to)) {
				continue
			}
			from, err := client.Sender(tx)
			if err != nil {
				continue
			}
			// Record all router traffic, not just watched wallets: the Feed
			// view's tally is what surfaces a new router before the decode
			// rate silently drops.
			sel := ""
			if d := tx.Data(); len(d) >= 4 {
				sel = fmt.Sprintf("0x%x", d[:4])
			}
			decodable := false
			if decode.IsBotRouter(*to) {
				_, err := decode.BotRouterSwap(tx)
				decodable = err == nil
			} else {
				_, err := decode.Swap(tx)
				decodable = err == nil
			}
			state.ObserveRouterTx(monitor.RawTx{
				At: b.ReceivedAt, Seq: b.SeqNum, From: from.Hex(),
				To: to.Hex(), Selector: sel, Decoded: decodable,
			})

			if !watch[from] {
				continue
			}
			matched.Add(1)
			state.ObserveMatch()
			// The state read blocks, so hand each match to its own goroutine:
			// one slow token must not delay the next block's detection.
			go evaluate(ctx, log, rpcClient, rec, cache, ladders, fcfg, tradeSize, b, tx, from, state, !*tui)
		}
	}

	var runErr error
	if *tui {
		// Run the pipeline in the background; the TUI owns the main goroutine.
		tuiCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			if err := client.Run(tuiCtx, handle); err != nil {
				state.SetConnected(false)
			}
		}()
		if err := monitor.Run(ctx, state, cancel); err != nil {
			// The TUI needs a real terminal. Piping or redirecting output is a
			// normal thing to try, so say what to do instead of leaking the
			// underlying /dev/tty error.
			if strings.Contains(err.Error(), "TTY") {
				return fmt.Errorf("--tui needs an interactive terminal (got: %w).\n"+
					"Drop --tui to stream decisions as plain lines instead", err)
			}
			return err
		}
	} else {
		runErr = client.Run(ctx, handle)
	}

	if ctx.Err() == nil && runErr != nil {
		return runErr
	}

	approved, rejected := rec.Totals()
	fmt.Printf("\n─── hoodsniper shadow summary ───\n")
	fmt.Printf("ran            %s\n", time.Since(start).Round(time.Second))
	fmt.Printf("txs seen       %d\n", seen.Load())
	fmt.Printf("watched swaps  %d\n", matched.Load())
	fmt.Printf("approved       %d\n", approved)
	fmt.Printf("rejected       %d\n", rejected)
	hits, misses := cache.Stats()
	fmt.Printf("profile cache  %d hits / %d misses\n", hits, misses)
	if lad, sup := ladders.Stats(); lad > 0 || sup > 0 {
		fmt.Printf("ladders        %d opened, %d clips suppressed", lad, sup)
		if lad > 0 {
			// Each suppressed clip is one router fee not paid.
			fmt.Printf("  (avoided %.1fx fee multiplication)", float64(lad+sup)/float64(lad))
		}
		fmt.Println()
	}

	dh, dm := decodeHits.Load(), decodeMisses.Load()
	if total := dh + dm; total > 0 {
		rate := 100 * float64(dh) / float64(total)
		fmt.Printf("decode rate    %.1f%% (%d ok / %d failed)\n", rate, dh, dm)
		// The bot routers are unverified upgradeable proxies. A layout change
		// shows up here first, as decodes quietly failing.
		if rate < 80 {
			fmt.Printf("WARNING        decode rate below 80%% — a router may have changed its\n")
			fmt.Printf("               calldata layout. Re-run `feedtap --discover` and re-verify.\n")
		}
	}
	fmt.Printf("log            %s\n", cfg.ShadowLog)
	return nil
}

// evaluate runs the filter chain for one matched transaction and records it.
func evaluate(
	ctx context.Context, log *slog.Logger, rpcClient *chain.Client,
	rec *shadow.Recorder, cache *chain.Cache, ladders *ladder.Tracker,
	fcfg filter.Config, tradeSize *big.Int,
	b feed.Block, tx *types.Transaction, from common.Address,
	mon *monitor.State, echo bool,
) {
	readCtx, cancel := context.WithTimeout(ctx, stateReadTimeout)
	defer cancel()

	var (
		intent decode.SwapIntent
		state  chain.TokenState
		err    error
	)
	if decode.IsBotRouter(*tx.To()) {
		intent, state, err = resolveBotSwap(readCtx, rpcClient, cache, tx)
	} else {
		intent, err = decode.Swap(tx)
	}
	if err != nil {
		decodeMisses.Add(1)
		mon.ObserveDecode(false)
		// A watched wallet calling a router we cannot read is worth knowing
		// about: for the bot routers it is the expected symptom of the proxy
		// changing its calldata layout under us.
		log.Warn("watched wallet, undecodable swap",
			"kol", from.Hex(), "tx", tx.Hash().Hex(), "to", tx.To().Hex())
		return
	}
	decodeHits.Add(1)
	mon.ObserveDecode(true)

	// Ladder consolidation runs after tier 0 and before the state read: a
	// suppressed clip must still be recorded, but must not pay for an RPC
	// round trip it will not act on.
	t0 := filter.Tier0(fcfg, intent)
	decision := t0

	var lad ladder.Verdict
	if t0.Approved {
		lad = ladders.Observe(from, intent.Token(), string(intent.Direction),
			intent.AmountIn, time.Now())
		if !lad.Act {
			decision = filter.Merge(t0, filter.Decision{
				Approved: false,
				Checks:   []filter.Check{{Name: "ladder", Verdict: filter.Reject, Reason: lad.Reason()}},
			})
		}
	}

	// Only pay for the state read if tier 0 let it through and this is not a
	// suppressed ladder clip. The bot-router path has already fetched it while
	// resolving the token.
	if decision.Approved {
		if state.Token == (common.Address{}) {
			state, err = rpcClient.FetchState(readCtx, cache, intent.Token())
			if err != nil {
				log.Warn("state read failed", "token", intent.Token().Hex(), "err", err)
				return
			}
		}
		decision = filter.Merge(t0, filter.Tier1(fcfg, state))
	}

	record := shadow.Build(b.SeqNum, tx.Hash(), from, intent, state, decision,
		tradeSize, time.Since(b.ReceivedAt), time.Now(), lad.Clip, lad.LadderTotalWei)
	if err := rec.Write(record); err != nil {
		log.Error("writing shadow record", "err", err)
	}
	mon.Add(monitor.Decision{
		At: record.At, Approved: record.Approved, Direction: record.Direction,
		Token: record.Token, KOL: record.KOL, Seq: record.SeqNum,
		LatencyMS: record.DetectLatencyMS, Reason: record.Reason,
	})
	// Printing would corrupt the TUI's frame, so only the streaming renderer
	// writes to stdout.
	if echo {
		fmt.Println(record.Line())
	}
}

// cacheStats adapts the chain cache's counters for the monitor.
func cacheStats(c *chain.Cache) (int, int) { return c.Stats() }

// monitorConfig flattens the loaded config for the TUI's Config view. The
// filter notes carry the caveats that matter at a glance — chiefly that
// "LP burnt or locked" cannot be evaluated on V3 or V4 at all.
func monitorConfig(path, ledger string, c config.Config) monitor.Config {
	f := c.Filters
	return monitor.Config{
		Path: path, Live: c.Live, Feed: c.Feed, RPC: c.RPC, ChainID: c.ChainID,
		Watch: c.Watch, TradeSizeETH: c.TradeSizeETH, LedgerPath: ledger,
		Filters: []monitor.ConfigRow{
			{Name: "min_liquidity_eth", Value: fmt.Sprintf("%g", f.MinLiquidityETH),
				Note: "deepest pool across V2/V3/V4"},
			{Name: "max_liquidity_eth", Value: fmt.Sprintf("%g", f.MaxLiquidityETH)},
			{Name: "require_lp_secured", Value: fmt.Sprintf("%t", f.RequireLPSecured),
				Note: "V2 only; n/a on V3 and V4"},
			{Name: "min_lp_burned_pct", Value: fmt.Sprintf("%g", f.MinLPBurnedPct)},
			{Name: "require_renounced", Value: fmt.Sprintf("%t", f.RequireRenounced)},
			{Name: "min_trade_eth", Value: fmt.Sprintf("%g", f.MinTradeETH),
				Note: "buys only; a sell's input is in tokens"},
			{Name: "allow_sells", Value: fmt.Sprintf("%t", f.AllowSells)},
			{Name: "ladder_window_seconds", Value: fmt.Sprintf("%d", f.LadderWindowSecs),
				Note: "0 = 60s default; collapses clip ladders"},
			{Name: "token_blocklist", Value: fmt.Sprintf("%d entries", len(f.Blocklist))},
			{Name: "token_allowlist", Value: fmt.Sprintf("%d entries", len(f.Allowlist))},
		},
	}
}

// decodeHits and decodeMisses drive the health warning. A bot router is an
// unverified upgradeable proxy, so the layout can change without notice; the
// symptom is a decode rate that quietly falls to zero.
var decodeHits, decodeMisses atomic.Uint64

// resolveBotSwap finishes a heuristic bot-router decode by testing each
// candidate address against the pool registry. The first one with a real WETH
// pool is the traded token; ordering matters, so a sell takes the first match
// (the token being sold enters the route first) and a buy the last.
//
// Candidate lookups go through the profile cache, so a repeat token is free and
// a wrong guess costs one cached miss rather than a fresh round trip.
func resolveBotSwap(
	ctx context.Context, rpcClient *chain.Client, cache *chain.Cache, tx *types.Transaction,
) (decode.SwapIntent, chain.TokenState, error) {
	bot, err := decode.BotRouterSwap(tx)
	if err != nil {
		return decode.SwapIntent{}, chain.TokenState{}, err
	}

	candidates := bot.Candidates
	if bot.Direction == decode.DirectionBuy {
		// Reverse so the last candidate — the route's final output — wins.
		candidates = make([]common.Address, len(bot.Candidates))
		for i, a := range bot.Candidates {
			candidates[len(bot.Candidates)-1-i] = a
		}
	}

	for _, cand := range candidates {
		state, err := rpcClient.FetchState(ctx, cache, cand)
		if err != nil {
			continue
		}
		if state.Pool == nil {
			continue // not a traded token, just some contract in the calldata
		}
		return bot.Resolve(cand), state, nil
	}
	return decode.SwapIntent{}, chain.TokenState{}, decode.ErrNotASwap
}

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
