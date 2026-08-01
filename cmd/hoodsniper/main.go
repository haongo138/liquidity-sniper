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
	"github.com/antono/hoodsniper/internal/exec"
	"github.com/antono/hoodsniper/internal/feed"
	"github.com/antono/hoodsniper/internal/filter"
	"github.com/antono/hoodsniper/internal/holdtime"
	"github.com/antono/hoodsniper/internal/ladder"
	"github.com/antono/hoodsniper/internal/monitor"
	"github.com/antono/hoodsniper/internal/position"
	"github.com/antono/hoodsniper/internal/shadow"
	"github.com/antono/hoodsniper/internal/wallet"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
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
	// Measures both their hold time and our latency, so the gate compares two
	// observed numbers rather than a threshold pulled from the air.
	holds := holdtime.New()
	holdRatio, holdSamples := cfg.HoldGate()
	// Prime from collected history. Without it the gate cannot judge anyone
	// until three round trips have been watched live, which takes hours.
	if rt, err := holds.SeedFromLedger(*ledgerPath); err == nil && rt > 0 {
		log.Info("hold-time gate seeded", "round_trips", rt, "ledger", *ledgerPath)
	}
	fcfg := cfg.FilterConfig()
	tradeSize := cfg.TradeSizeWei()
	client := feed.NewClient(resolveFeed(cfg.Feed), cfg.ChainID, log)

	// Execution is armed only when live is set AND a key is present. Missing
	// either falls back to shadow, loudly — never silently the other way.
	var (
		executor *exec.Executor
		book     *position.Book
	)
	if cfg.Live {
		signer, err := wallet.Load(cfg.ChainID)
		if err != nil {
			return fmt.Errorf("live: true but no usable signing key: %w", err)
		}
		raw, err := rpc.DialContext(ctx, cfg.RPC)
		if err != nil {
			return fmt.Errorf("dialing rpc for execution: %w", err)
		}
		defer raw.Close()

		executor = exec.New(raw, signer, cfg.ExecConfig())
		book = position.NewBook(cfg.ExitRules())
		rules := cfg.ExitRules()

		mode := "DRY RUN — trades simulated and signed, never broadcast"
		if !cfg.IsDryRun() {
			mode = "LIVE — real transactions will be broadcast"
		}
		log.Warn("execution armed", "mode", mode,
			"address", signer.Address().Hex(),
			"chain_id", cfg.ChainID,
			"max_trade_eth", cfg.MaxTradeETH,
			"daily_loss_limit_eth", cfg.DailyLossLimitETH,
			"slippage_bps", cfg.SlippageBps,
			"take_profit_pct", rules.TakeProfitPct,
			"stop_loss_pct", rules.StopLossPct,
			"max_hold", rules.MaxHold,
			"follow_kol_sell", rules.FollowKOLSell)
	} else {
		log.Info("shadow mode — no transactions will be signed",
			"watching", len(watch), "trade_size_eth", cfg.TradeSizeETH, "log", cfg.ShadowLog)
	}

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
			go evaluate(ctx, log, rpcClient, rec, cache, ladders, holds, holdRatio, holdSamples,
				executor, book, fcfg, tradeSize, b, tx, from, state, !*tui)
		}
	}

	// Price and time triggers need a clock of their own: they fire when nothing
	// is happening, which is exactly when no feed event would wake them.
	if executor != nil && book != nil {
		go watchExits(ctx, log, executor, book, rpcClient, cache)
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
	if w, rt, ls := holds.Stats(); rt > 0 {
		fmt.Printf("hold samples   %d round trips across %d wallets, %d latency samples\n", rt, w, ls)
	}
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
	if book != nil {
		openN, closed := book.Stats()
		fmt.Printf("positions      %d open", openN)
		for r, n := range closed {
			fmt.Printf(", %d closed(%s)", n, r)
		}
		fmt.Println()
		if openN > 0 {
			fmt.Printf("WARNING        %d position(s) still open at shutdown — they are NOT\n", openN)
			fmt.Printf("               closed automatically. Exit them manually.\n")
		}
	}
	fmt.Printf("log            %s\n", cfg.ShadowLog)
	return nil
}

// evaluate runs the filter chain for one matched transaction and records it.
func evaluate(
	ctx context.Context, log *slog.Logger, rpcClient *chain.Client,
	rec *shadow.Recorder, cache *chain.Cache, ladders *ladder.Tracker,
	holds *holdtime.Tracker, holdRatio float64, holdSamples int,
	executor *exec.Executor, book *position.Book,
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

	// Mirror an exit before any entry filtering. An exit must never be blocked
	// by a gate designed to be selective about entries — being choosy about
	// getting out is how a position becomes permanent.
	if executor != nil && book != nil && intent.Direction == decode.DirectionSell {
		if ex, ok := book.OnKOLSell(intent.Token()); ok {
			closePosition(ctx, log, executor, book, rpcClient, cache, ex)
		}
	}

	// Record the round trip regardless of what we decide. Gating a trade must
	// not stop us learning how fast the wallet actually turns over, or the gate
	// would starve itself of the evidence it needs.
	now := time.Now()
	switch intent.Direction {
	case decode.DirectionBuy:
		holds.ObserveEntry(from, intent.Token(), now)
	case decode.DirectionSell:
		holds.ObserveExit(from, intent.Token(), now)
	}

	// Ladder consolidation runs after tier 0 and before the state read: a
	// suppressed clip must still be recorded, but must not pay for an RPC
	// round trip it will not act on.
	t0 := filter.Tier0(fcfg, intent)
	decision := t0

	// The hold gate only applies to entries: declining to copy an exit because
	// the wallet trades fast would strand a position we already opened.
	var hv holdtime.Verdict
	if t0.Approved && intent.Direction == decode.DirectionBuy {
		hv = holds.Check(from, holdRatio, holdSamples)
		// The check is always recorded, including when it cannot yet be
		// evaluated. A gate that silently contributes nothing is
		// indistinguishable from a gate that is broken.
		check := filter.Check{Name: "hold_time", Verdict: filter.NotApplicable, Reason: hv.Reason()}
		switch {
		case hv.Applicable && hv.Pass:
			check.Verdict = filter.Pass
		case hv.Applicable:
			check.Verdict = filter.Reject
		}
		decision = filter.Merge(t0, filter.Decision{
			Approved: check.Verdict != filter.Reject,
			Checks:   []filter.Check{check},
		})
	}

	var lad ladder.Verdict
	if decision.Approved {
		lad = ladders.Observe(from, intent.Token(), string(intent.Direction),
			intent.AmountIn, time.Now())
		if !lad.Act {
			decision = filter.Merge(decision, filter.Decision{
				Approved: false,
				Checks:   []filter.Check{{Name: "ladder", Verdict: filter.Reject, Reason: lad.Reason()}},
			})
		}
	}

	// Only pay for the state read if tier 0 let it through and this is not a
	// suppressed ladder clip. The bot-router path has already fetched it while
	// resolving the token.
	tradeablePath := decision.Approved
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

	// Record our latency only once the decision is complete. Sampling it at the
	// top of this function captured decode time alone — ~340us rather than the
	// ~250ms a real decision costs — which made the hold gate compare against a
	// number three orders of magnitude too small and pass everything.
	detectLatency := time.Since(b.ReceivedAt)
	// Only decisions that walked the full path count towards our latency. A
	// tier-0 or ladder rejection costs under a millisecond and never touches
	// the network, so including it understates what a real trade would cost.
	if tradeablePath {
		holds.ObserveLatency(detectLatency)
	}

	tokenState := state
	record := shadow.Build(b.SeqNum, tx.Hash(), from, intent, state, decision,
		tradeSize, detectLatency, time.Now(), lad.Clip, lad.LadderTotalWei)
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

	// Only an approved entry reaches the executor. Everything above this line
	// runs identically in shadow mode, so arming execution changes what happens
	// after a decision, never how the decision is made.
	if executor == nil || book == nil || !decision.Approved ||
		intent.Direction != decode.DirectionBuy {
		return
	}
	openPosition(ctx, log, executor, book, from, intent, tokenState, tradeSize)
}

// exitCheckInterval is how often open positions are re-valued. Each check costs
// one simulated sell per position, so this trades RPC load against how late a
// stop-loss can fire.
const exitCheckInterval = 15 * time.Second

// watchExits fires the price and time triggers. The KOL-sell trigger is handled
// on the feed path instead, because it is driven by an event rather than a clock.
func watchExits(ctx context.Context, log *slog.Logger, executor *exec.Executor,
	book *position.Book, rpcClient *chain.Client, cache *chain.Cache) {

	t := time.NewTicker(exitCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		for _, p := range book.Positions() {
			st, err := rpcClient.FetchState(ctx, cache, p.Token)
			if err != nil || st.Pool == nil {
				continue
			}
			// A quote that errors means the sell cannot execute. Rather than
			// treat that as "unchanged", it is passed through as an unknown
			// value so the max-hold trigger can still rescue the position.
			value, qErr := executor.QuoteSell(ctx, st.Pool, p.Token, p.Tokens)
			if qErr != nil {
				log.Warn("position cannot be quoted — possible honeypot",
					"token", p.Token.Hex(), "err", qErr)
				value = nil
			}
			if ex, ok := book.Check(p, value, now); ok {
				log.Info("exit triggered", "token", p.Token.Hex(),
					"reason", ex.Reason, "detail", ex.Detail, "pnl_pct", ex.PnLPct)
				closePosition(ctx, log, executor, book, rpcClient, cache, ex)
				if ex.ValueWei != nil && p.CostWei != nil {
					executor.RecordPnL(new(big.Int).Sub(ex.ValueWei, p.CostWei))
				}
			}
		}
	}
}

// openPosition buys and books the result.
func openPosition(ctx context.Context, log *slog.Logger, executor *exec.Executor,
	book *position.Book, kol common.Address,
	intent decode.SwapIntent, tokenState chain.TokenState, size *big.Int) {

	if halted, why := executor.Halted(); halted {
		log.Error("entry skipped, execution halted", "reason", why)
		return
	}
	token := intent.Token()
	res, err := executor.Buy(ctx, tokenState.Pool, token, size, nil)
	if err != nil {
		// A reverted simulation is the expected outcome for a honeypot, so this
		// is information rather than a malfunction.
		log.Warn("entry not executed", "token", token.Hex(), "reason", res.Reason, "err", err)
		return
	}
	log.Info("entry executed", "token", token.Hex(), "tx", res.Hash.Hex(),
		"amount_in", res.AmountIn, "min_out", res.MinOut, "sent", res.Sent,
		"dry_run", !res.Sent)

	book.Open(position.Position{
		Token: token, KOL: kol, FeeTier: tokenState.Pool.FeeTier,
		Pool: tokenState.Pool.Address, CostWei: res.AmountIn,
		Tokens: res.MinOut, OpenedAt: time.Now(), EntryTx: res.Hash,
	})
}

// closePosition sells and books the exit.
func closePosition(ctx context.Context, log *slog.Logger, executor *exec.Executor,
	book *position.Book, rpcClient *chain.Client, cache *chain.Cache, ex position.Exit) {

	p := ex.Position
	st, err := rpcClient.FetchState(ctx, cache, p.Token)
	if err != nil || st.Pool == nil {
		log.Error("exit blocked, cannot resolve pool", "token", p.Token.Hex(), "err", err)
		return
	}
	res, err := executor.Sell(ctx, st.Pool, p.Token, p.Tokens, nil)
	if err != nil {
		// The position stays open on failure. Dropping it here would lose track
		// of tokens we still hold.
		log.Error("exit failed, position still open", "token", p.Token.Hex(),
			"reason", ex.Reason, "err", err)
		return
	}
	log.Info("exit executed", "token", p.Token.Hex(), "reason", ex.Reason,
		"detail", ex.Detail, "tx", res.Hash.Hex(), "sent", res.Sent)
	book.Close(p.Token, ex.Reason)
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
			{Name: "min_hold_ratio", Value: fmt.Sprintf("%g", f.MinHoldRatio),
				Note: "0 = 5x default; skips wallets too fast to follow"},
			{Name: "min_hold_samples", Value: fmt.Sprintf("%d", f.MinHoldSamples)},
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
