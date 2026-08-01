# hoodsniper

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-00ADD8.svg?logo=go&logoColor=white)](go.mod)
[![Chain 4663](https://img.shields.io/badge/chain-Robinhood%204663-1ce783.svg)](https://docs.robinhood.com/chain/connecting)
[![Tests 110](https://img.shields.io/badge/tests-110%20passing-brightgreen.svg)](#verification)
[![Execution: dry-run](https://img.shields.io/badge/execution-dry--run%20default-orange.svg)](#execution)

Copy-trades KOL wallets on Robinhood Chain (EVM, chain 4663) by tapping the
sequencer feed directly.

**It is a backrunner, not a front-runner.** Front-running is impossible on this
chain — see [SPEC.md](SPEC.md) for the evidence. The edge is over everyone else
reacting via RPC, never over the wallet being copied.

> [!WARNING]
> **Execution is built but unproven — no transaction has ever been sent.** It
> defaults to dry run, and turning that off should follow a dry run you have read.
> No wallet has yet been
> shown profitable enough to be worth copying after the ~4% round-trip fee drag,
> and wallet ranking is still noisy at achievable sample sizes. Live execution is
> deliberately not armed. Trading on-chain risks total loss of funds; nothing
> here is financial advice, and the MIT licence's "AS IS, WITHOUT WARRANTY"
> applies in full.

## Status

| Phase | State |
|---|---|
| 0 — Feed tap + latency gate | **Done.** Gate passed, see [PHASE0-RESULTS.md](PHASE0-RESULTS.md) |
| 1 — Decode + filters + shadow mode | **Done.** Verified against live mainnet |
| 2 — Live execution | **Built, dry-run by default.** Never validated against a real trade |
| 3 — Dashboard | **Done.** Five-tab TUI |
| 4 — Exit strategy | **Done.** Ships with execution, not after |

Phases 2 and 4 landed together, as promised: a bot that can buy but has no exit
logic is worse than no bot.

**No transaction has ever been sent by this code.** It is built, unit-tested and
its guards are verified, but it has never traded — not on mainnet, not on
testnet. Treat the first real run as the test it is.

## Quick start

```bash
go test ./...

# 1. Watch the raw feed
go run ./cmd/feedtap --feed mainnet --seconds 30 --all

# 2. Find out which contracts carry the flow
go run ./cmd/feedtap --feed mainnet --seconds 60 --discover 20

# 3. See decoded swaps
go run ./cmd/feedtap --feed mainnet --seconds 60 --swaps

# 4. Shadow-trade watched wallets (signs nothing)
cp hoodsniper.yaml my.yaml    # then edit `watch:` with your KOL wallets
go run ./cmd/hoodsniper --config my.yaml

# 5. …or watch it live in a terminal UI
go run ./cmd/hoodsniper --config my.yaml --tui
```

### The TUI

`--tui` opens a five-tab dashboard over everything the CLIs do:

| Tab | Shows |
|---|---|
| **1 Live** | Feed status, decode rate with the router-layout alarm, latency percentiles, cache hit rate, scrolling decisions with filter reasons |
| **2 Wallets** | Ledger wallets ranked by measured P&L; `enter` drills into per-token positions; `s` scores the busiest candidates |
| **3 Ledger** | Collector depth — size, swaps, wallets, and how many have enough history to rank |
| **4 Feed** | Raw router traffic; `d` toggles the discover tally that catches a new router |
| **5 Config** | Loaded settings, watchlist, and filter thresholds with their caveats |

Keys: `q` quit · `tab`/`1`-`5` switch · `p` pause · plus per-view keys shown in
the footer.

**The CLIs are unchanged.** The TUI is a front-end, not a replacement — long
collection runs, background jobs and piping into scripts all still work exactly
as before. Anything the TUI does, a command can do headless.

Two deliberate limits. Config is **read-only**: a file that gates money should
not be editable by a stray keystroke. And wallet scoring is on-demand rather
than automatic, because it costs a receipt per trade and will hit `429` on a
public node — counting the ledger is local and instant, so that happens on open.

It needs an interactive terminal; piping or redirecting output fails with a
message telling you to drop the flag. Logs go to `hoodsniper.log` in TUI mode
rather than stdout, since writing to the screen would corrupt the frame.

The daemon and the UI read the same `internal/monitor.State`, so they cannot
disagree — the TUI is a second renderer, not a second pipeline.

For sustained runs use the local relay — Robinhood rate-limits **per client, not
per connection**, so opening several sockets splits one budget several ways:

```bash
docker compose up -d --wait relay
# then set `feed: ws://127.0.0.1:9642` in your config
```

## Picking wallets (do this before anything else)

Wallets must be chosen by **measured profitability**, never by activity. The
three chosen for being busy all lost money — the best managed −4.45%, another
went 0-for-4 on round trips — and a copier pays ~4% per round trip in fees
before any slippage, so copying them loses money faster than they do.

```bash
go run ./cmd/pnl 0xWALLET          # inspect a wallet you already have
go run ./cmd/scout                 # find and rank candidates
go run ./cmd/collect --ledger ledger.jsonl   # accumulate history (run for hours)
go run ./cmd/scout --ledger ledger.jsonl     # score from that history
```

**Sample size is the binding constraint.** The explorer serves ~50 transactions
per address, which yields 2–6 complete round trips — far too few to separate
skill from luck. Re-measuring a four-wallet shortlist flipped one from **+67.7%
to −19.4%**, and another's entire edge came from a single trade returning +2200%
on 0.015 ETH. `cmd/scout` now withholds a verdict below `--min-trips`.

**Scoring needs a real RPC key.** The public endpoint returns `429 Too Many
Requests` after roughly one batch of 50 receipts, and a wallet with 424 trades
needs nine. Point `--rpc` at Alchemy or another provider before scoring, or
every wallet comes back skipped. Collection is unaffected — it uses the feed and
makes no RPC calls at all.

`cmd/collect` removes that ceiling by recording every router swap off the feed
to an append-only ledger: **4,653 swaps and 343 wallets with ≥5 transactions in
45 seconds**. It stores only what the calldata already gives (no receipts, no
state reads, one WebSocket), so it is cheap enough to leave running for days.
Receipts are fetched later, only for the wallets actually being scored. Budget
roughly 80 MB of ledger per hour.

## How it works

```
sequencer feed (wss)  ──►  decode  ──►  watchlist  ──►  swap decode
                                                            │
                                              tier 0 (0ms, in-memory)
                                                            │
                                              tier 1 (1 RTT, cached profile)
                                                            │
                                                    shadow.jsonl
```

Tier 0 rejects on direction, block/allowlist and trade size before any network
call — measured at **0.2–0.5ms**. Tier 1 costs one round trip for the liquidity
read.

## Latency is round-trip-bound

This is the single most important operational fact, and it was measured, not
assumed:

| Path | Latency | Round trips |
|---|---|---|
| Tier-0 rejection | 0.2–0.5 ms | 0 |
| Tier-1, cached profile | **252 ms** | 1 |
| Tier-1, cold token | 1077–1159 ms | 3 |

Baseline RTT to the public RPC from the test host was **~250ms**, so every one
of those figures is dominated by network distance, not computation. The profile
cache exists because of this: pool address, decimals, supply and owner barely
change, so they are cached for 10 minutes, while liquidity — the only figure
that moves per trade — is always read fresh in a single call.

**Co-location beats every code optimisation available here.** On a node in the
same region, the same work costs roughly 10ms warm and 60ms cold. Running from
far away with a cold cache costs more than the entire latency edge, meaning you
lose the race you are trying to win. Deploy near a dedicated provider.

## Filters

Configured in YAML, evaluated as pure functions, every verdict recorded with a
reason in `shadow.jsonl`.

| Filter | Tier | Notes |
|---|---|---|
| `token_blocklist` / `token_allowlist` | 0 | Free |
| `allow_sells` | 0 | Direction gate |
| `ladder_window_seconds` | 0 | Collapses a laddered entry into one signal (see below) |
| `min_hold_ratio` / `min_hold_samples` | 0 | Skips wallets that trade faster than we can follow (see below) |
| `min_trade_eth` | 0 | Ignores dust from the watched wallet |
| `min_liquidity_eth` / `max_liquidity_eth` | 1 | ETH-side depth of the deepest pool across V2, V3 and V4 |
| `require_lp_secured` + `min_lp_burned_pct` | 1 | **V2 only** — see below |
| `require_renounced` | 1 | `owner() == 0x0` |

### On "LP burnt or locked"

This filter only means something on Uniswap V2, where LP positions are fungible
ERC-20s that can be sent to a burn address. **V3 has no LP token** — liquidity is
held as NonfungiblePositionManager NFTs, so there is nothing to burn.

That matters here because the active speculative flow on this chain is
overwhelmingly V3, mostly in the 1% fee tier. Sampled tokens had a V3 pool and
no V2 pair at all.

So the filter reports `n/a` on V3 rather than silently passing. A check that
could not run is not the same as a check that succeeded, and conflating the two
would quietly disable your rug protection.

The V2 factory does hold 26,856 pairs, so the check is live for that long tail.

## Verified deployments

Resolved by calling each contract, not copied from a blog post. See
`internal/chain/addresses.go` for the call that proved each one.

| Contract | Address |
|---|---|
| WETH | `0x0bd7d308f8e1639fab988df18a8011f41eacad73` |
| SwapRouter02 (V3) | `0xCaf681a66D020601342297493863E78C959E5cb2` |
| UniswapV3Factory | `0x1f7d7550b1b028f7571e69a784071f0205fd2efa` |
| V2 Router | `0x89e5DB8B5aA49aA85AC63f691524311AEB649eba` |
| V2 Factory | `0x8bceaa40b9acdfaedf85adf4ff01f5ad6517937f` |
| UniversalRouter | `0x8876789976dEcBfCbBbe364623C63652db8C0904` |
| V4 PoolManager | `0x8366a39cc670b4001a1121b8f6a443a643e40951` |

Decode coverage is **93.9%** of router-bound calls on live traffic. The
remainder is mostly UniversalRouter `execute`, which is not yet decoded — it is
~30x lower volume than SwapRouter02.

If detections ever go quiet, re-run `--discover`: a stale router address
produces zero hits and no error, which is the worst failure mode available.

## Third-party bot routers (heuristic decode)

A lot of retail flow does not touch Uniswap directly. `0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc`
is an EIP-1967 proxy (impl `0x73a160aa`) that routes into the V4 PoolManager,
used by 151 distinct wallets in a 75s sample. It is **unverified with no
published ABI**, so `internal/decode/botrouter.go` is explicitly a heuristic:

- Only two fields are read positionally — word 2 (`amountIn`) and word 3
  (`amountOutMinimum`) — because those are the only ones stable across every
  sampled layout. Both an 18-word single-hop and a 42-word three-hop form exist,
  and every other field moves between them.
- The traded token is **not** read from a fixed offset. Address-shaped words are
  extracted in calldata order and resolved against the pool registry; whichever
  has a real WETH pool wins. A sell takes the first match, a buy the last.
- Direction comes from `tx.Value`: native ETH in means a buy.

Measured at **100% decode rate (23/23)** on live traffic, validated by a golden
test built from a real mainnet transaction whose receipt confirms word 2 equals
the exact token amount transferred.

**This will break eventually.** The proxy owner can change the layout without
notice, and the symptom is decodes silently failing. The daemon tracks a decode
rate and prints a warning below 80% — treat that as the alarm.

Cost: resolving candidates means testing several addresses against the pool
registry, measured at **1250–1500ms** versus ~250ms for a direct Uniswap decode.
The negative results are cached, so this improves as the cache warms, but it is
another reason to co-locate.

## Execution

Armed with `live: true` **and** a key in the environment. Missing either falls
back to shadow — loudly, and never silently the other way.

```bash
# The key never goes in the YAML. A 0600 file is preferred to an env var,
# which anything that can read /proc or run `ps e` can see.
echo -n "<hex key>" > ~/.hoodsniper.key && chmod 600 ~/.hoodsniper.key
export HOODSNIPER_PRIVATE_KEY_FILE=~/.hoodsniper.key

go run ./cmd/hoodsniper --config my.yaml     # dry_run defaults to true
```

Startup prints exactly what is armed:

```
execution armed  mode="DRY RUN — trades simulated and signed, never broadcast"
  address=0xd400… chain_id=46630 max_trade_eth=0.05 daily_loss_limit_eth=0.25
  take_profit_pct=50 stop_loss_pct=30 max_hold=10m0s follow_kol_sell=true
```

**Every trade is simulated with `eth_call` before it is signed.** A simulation
that reverts is not sent. That costs one round trip and catches honeypots, dead
slippage floors and encoding bugs before any gas is spent.

Four refusals, each closing a way to lose money quietly:

| Refusal | Why |
|---|---|
| Slippage is never zero | The copied wallets send `amountOutMinimum=0` and bet on speed. We arrive **after** them by design, so copying that hands a free option to whoever is in between. Bad config falls back to 5%, never to none. |
| V4 tokens are not traded | Their depth is now measured correctly, but V4 encoding goes through UniversalRouter's command buffer and is not implemented. Refusing is honest; mis-encoding is not. |
| `dry_run` defaults to **true** | Arming should not start spending on the strength of an unset field. |
| Live requires an exit trigger | Config load fails if take-profit, stop-loss, max-hold and follow-KOL-sell are all off. That combination opens positions it can never close. |

Plus a per-trade ceiling, a daily-loss kill switch that is **one-way** for the
process lifetime (a later profit does not re-arm it — that should be a human
decision), and a shutdown warning if positions are still open.

### Exits

Four independent triggers, because each covers a failure the others miss:

- **The wallet sells** — they know something we do not; the observed pattern is
  a de-risk sell that recovers the stake.
- **Take profit** — the wallet may never sell, or may sell where we cannot see.
- **Stop loss** — the wallet may be wrong, or we entered far worse than they did,
  which is structural for a backrunner.
- **Max hold** — covers the case none of the above fire: a token that stops
  trading, a wallet that goes quiet, a position held forever.

Price and time triggers run on their own 15s clock, because they fire when
nothing is happening — exactly when no feed event would wake them. Positions are
valued by **simulating the sell**, which accounts for fee-on-transfer and range
effects that a price calculation misses; a quote that reverts is a honeypot
signal, and max-hold still rescues the position.

## Hold-time gate

Copying a wallet you are structurally too slow to follow is a **guaranteed**
loss, not a probabilistic one: if they are out in a second and detection alone
costs two, the position is entered after the move and exited after the reversal,
every time.

Both halves of the comparison are measured, and neither is a constant:

- **Their hold time**, per wallet, from observed buy→sell pairs. The two sampled
  wallets had medians of **13s and 33s** — a single global threshold would be
  wrong for both.
- **Our latency**, from decisions that actually did the work. Measured p90 was
  **1.9–2.7s**, dominated by round-trip distance, so it changes with deployment.

Default floor is **5x**: we enter late by roughly our latency and exit late by
the same, so a hold of N times our latency captures about (N−2)/N of the move.
At 5x that is 60%, leaving room for the ~4% fee drag. Below 3x the arithmetic
stops working.

The gate is **per-trade, not per-wallet**. One sampled wallet had only 2 of 11
trades under 3 seconds; excluding it outright would have discarded nine good
signals to avoid two bad ones. It applies **only to entries** — declining an exit
because the wallet trades fast would strand a position already opened.

Live, with the tracker seeded from 46,852 ledger round trips:

```
median hold 28.487s is 15.1x our p90 latency 1.891s   → pass
```

**Seed it from the ledger.** Hold times can only be learned from completed round
trips, so without seeding the gate is inert for hours. The daemon primes itself
from `--ledger` at startup; run `cmd/collect` first and it works on the first
decision.

Below `min_hold_samples` the check reports **n/a**, never a pass — a gate that
could not run must not look like one that approved.

## Ladder consolidation

Watched wallets slice a position into equal clips. One observed entry was **five
identical 0.03 ETH buys of the same token inside ~3 seconds**, totalling 0.15
ETH. Mirroring each clip pays the bot router's 1% fee five times — roughly 5% on
entry alone, before the pool fee and before the exit, which is more than the
edge being chased.

Waiting for the ladder to finish and then sizing up is not an option: detection
is ~250ms warm and the ladder took ~3 seconds, so waiting trades away the entire
timing advantage. Instead the **first clip acts and the rest are suppressed** —
our position size is our own risk parameter, not a mirror of theirs.

Measured over 150s against the 40 busiest wallets:

```
ladders 115 opened, 65 clips suppressed  (avoided 1.6x fee multiplication)
```

Suppressed clips cost **0.2–1.7ms** — they are rejected before the tier-1 state
read, so a clip we will not act on never pays for a round trip. They are
recorded in the shadow log with `ladder_clip` and the ladder's running total,
never silently dropped: a signal that vanishes without explanation is
indistinguishable from a bug.

A **sell is never suppressed by a preceding buy** — direction is part of the
key. Swallowing an exit would leave a position with no way out.

`ladder_window_seconds` defaults to 60 (measured round trips were 97s and 142s,
so a genuine re-entry falls outside it). Set it negative to disable, which is
how the fee drag that would have been paid can be measured. Unset means the
default rather than off, because silently mirroring every clip is the more
expensive mistake.

## Uniswap V4 support

V4 is a **singleton**: every pool lives inside one PoolManager, so there is no
per-pool address and `WETH.balanceOf(pool)` — the measure used for V2 and V3 —
is meaningless. Ignoring that made the filter gate on whichever dormant V3 dust
pool happened to exist. One token was rejected fifteen times at a fixed
**0.2200 ETH** while its real V4 depth was **112.4220 ETH**, a 511x error, and
tokens with *no* V2/V3 pool at all were invisible entirely.

Two measured facts made it tractable:

1. **V4 denominates native ETH as `address(0)`, not WETH.** Discovery that looks
   for WETH pairs finds nothing — which is why V4 pools were invisible.
2. **The PoolManager emits `Initialize` with the poolId as its first indexed
   topic.** Reconstructing the poolId by hashing a guessed PoolKey failed (every
   storage read returned zero); reading it from the event cannot.

Depth then comes from the pool's stored liquidity and price. The storage slot
was found by scanning against a poolId taken from a live `Swap` event until the
value matched that event's own `liquidity` field — slot 6, offset 3 — rather
than assumed. Since V4 holds concentrated liquidity there is no single reserve,
so depth is the virtual reserve at the current price:

```
ETH is currency0:  amount0 = L · 2^96 / sqrtPriceX96
ETH is currency1:  amount1 = L · sqrtPriceX96 / 2^96
```

Treat it as an order of magnitude, not an exact figure: it overstates depth for
a range ending near the current tick and understates a wide one. A pool with no
active liquidity at the current price reports **zero depth** rather than "no
pool found" — it should be rejected as thin, not as unseeable.

Verify against mainnet:

```bash
V4_LIVE=1 go test ./internal/chain -run TestV4 -v
```

## Known risks

1. **Feed messages are soft confirmations.** A sequencer failover can reorg what
   you saw. Confirm against a node before treating a position as real.
2. **ArbOS 61 compliance filter.** An authorised party can register a tx hash
   and the chain voids it — included in a block, `status 0x0`, no logs, **gas
   fully burned**. This applies to your transactions, and P&L accounting must
   model it.
3. **Stock Tokens are geo-restricted** (not offered to US persons; also blocked
   in the UK, Canada, Switzerland, the UAE). This tool targets permissionless
   ERC-20s, so the restriction is informational.
4. **Public endpoints are rate-limited** and documented as not for production.

## Verification

```bash
go build ./...
go vet ./...
go test -race ./...        # 110 tests
```

Coverage is weighted toward the things that fail silently:

- **Wire decoder** — round-trips real signed transactions through the Nitro
  batch format, including corrupt-segment skipping and the `signatureV2` field
  name that makes a signed feed look unsigned if misread.
- **Calldata decoding** — cross-validated against go-ethereum's own ABI encoder,
  plus hostile inputs (truncated bodies, out-of-range offsets) that must error
  rather than panic. Calldata is attacker-controlled.
- **Bot router heuristic** — a golden test built from a real mainnet transaction
  whose receipt confirms the decoded amount.
- **Filters** — that `n/a` never masquerades as a pass, since an unrunnable check
  is not a passed check.
- **TUI** — every tab rendered through the real `View()` at four terminal widths.
  Render the frames yourself without a terminal:
  ```bash
  go test -c -o /tmp/montest ./internal/monitor
  PREVIEW=1 /tmp/montest -test.run TestPreview -test.v
  ```

There is no CI, so no build badge — one would assert a check that nobody runs.

## Licence

[MIT](LICENSE) © 2026 haongo138
