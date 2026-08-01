# hoodsniper

Copy-trades KOL wallets on Robinhood Chain (EVM, chain 4663) by tapping the
sequencer feed directly.

**It is a backrunner, not a front-runner.** Front-running is impossible on this
chain — see [SPEC.md](SPEC.md) for the evidence. The edge is over everyone else
reacting via RPC, never over the wallet being copied.

## Status

| Phase | State |
|---|---|
| 0 — Feed tap + latency gate | **Done.** Gate passed, see [PHASE0-RESULTS.md](PHASE0-RESULTS.md) |
| 1 — Decode + filters + shadow mode | **Done.** Verified against live mainnet |
| 2 — Live execution | **Not armed.** `live: true` is rejected at startup |
| 3 — Dashboard | Not started |
| 4 — Exit strategy | Not started |

Phase 2 is deliberately not built yet: a bot that can buy but has no exit logic
(Phase 4) is worse than no bot. Those two land together or not at all.

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
| `min_trade_eth` | 0 | Ignores dust from the watched wallet |
| `min_liquidity_eth` / `max_liquidity_eth` | 1 | WETH side of the deepest pool |
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

## KNOWN BUG: the liquidity filter is blind to Uniswap V4

**`min_liquidity_eth` is unreliable for exactly the flow this tool targets.** Do
not trust its verdicts, and do not arm execution until this is fixed.

`findDeepestPool` locates pools by asking the V2 and V3 factories for a pool
address, then reading `WETH.balanceOf(pool)`. **That approach cannot work for
V4**, which is a *singleton*: every pool lives inside the one PoolManager
contract, so there is no per-pool address whose balance means anything.

Caught in a live shadow run. Token `0xaF3D76f1…` was rejected ~15 times over
~180 blocks with liquidity pinned at 0.2200 ETH while wallets actively traded
it. Liquidity that never moves under active trading is not liquidity being
traded. Checking directly:

| Venue | Depth |
|---|---|
| V2 pair | none |
| V3 fee=500 pool | **0.2186 WETH** — dormant dust, what the filter measured |
| V4 PoolManager | **1,433.93 tokens + 1,074.06 WETH** (aggregate) — where trading happens |

So the filter gated on an abandoned dust pool and rejected every trade. These
are **false rejections**, and the bot routers route into V4, so this hits the
majority of the flow the tool is aimed at.

### Why the obvious fix was rejected

Reading V4 per-pool state means reconstructing
`poolId = keccak256(abi.encode(currency0, currency1, fee, tickSpacing, hooks))`
and `extsload`-ing the PoolManager. I tried it against a PoolKey taken from a
real transaction and scanned mapping slots 0–15: every read returned zero, so
either the PoolKey or the storage layout is wrong. Both would be guesses layered
on the already-heuristic bot-router decode, and a wrong guess fails silently as
a plausible-looking number.

### The fix worth building instead

**Simulate the trade with `eth_call` rather than measuring a pool.** Ask the
router what a sell of our intended size actually returns. This is:

- **Venue-agnostic** — works for V2, V3, V4 and the bot routers, with no pool
  discovery, no storage layout, and no PoolKey reconstruction.
- **The number that actually matters** — "what would I get out" rather than
  "how much is nominally in there".
- **A honeypot check for free** — a sell simulation that reverts is a token you
  cannot exit, which is the single most valuable filter and is currently not
  implemented at all.

Cost is one `eth_call`, replacing the current balance read. It subsumes
`min_liquidity`, honeypot detection and tax measurement in one call.

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
