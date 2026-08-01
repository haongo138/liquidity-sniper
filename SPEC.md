# SPEC: `hoodsniper` — KOL Backrun Bot for Robinhood Chain

**Status:** Phases 0 and 1 complete and verified against live mainnet. Gate **PASSED** (p50 edge 763ms vs a 100ms bar) — see [PHASE0-RESULTS.md](PHASE0-RESULTS.md); usage and measured latencies in [README.md](README.md). Phase 2 (live execution) is deliberately not armed: it ships with Phase 4 (exits), because a bot that can buy but not sell is worse than no bot.

---

## Context

The ask was a Go CLI/web tool to **front-run** a KOL wallet's buy/sell swaps on Robinhood Chain, gated by rug-check filters (min liquidity, LP burnt/locked, etc.).

Research killed the front-running premise. It is not difficult on this chain — it is **architecturally impossible**:

| Property | Robinhood Chain | Consequence |
|---|---|---|
| Mempool | **None public** | You cannot see a pending tx |
| Ordering | **FCFS by arrival time**, not priority-fee auction | Paying more gas moves you nowhere |
| Timeboost / express lane | **Not enabled** (chain is explicitly FCFS) | Priority cannot be bought |
| Sequencer feed | Fires **after ordering *and* execution** (carries a populated `blockHash`) | By the time you see it, the block is built |

Chainstack's reference feed decoder states it directly: *"There is nothing to front-run. The feed reports what the sequencer has already decided and already executed. You're reading, not racing."*

**The real, achievable edge:** the sequencer feed hands you the KOL's swap **~100ms+ before any RPC, explorer, or indexer can surface it** — every other consumer must re-execute the block first. You cannot beat the KOL, but you can beat everyone reacting via RPC polling.

So this spec builds a **latency-advantaged backrunner**: detect the KOL's swap on the feed, vet the token, and land your own swap in the next block ahead of the slow crowd.

A side benefit of the pivot: backrunning does not degrade the KOL's fill the way front-running would. There is no victim in the execution path.

---

## Decisions (confirmed)

| Axis | Choice |
|---|---|
| Strategy | Backrun on Robinhood Chain via sequencer feed |
| Token universe | Permissionless ERC-20s (Uniswap v2/v3/v4) — rug filters fully apply |
| Interface | Headless Go daemon + read-only local web dashboard |
| First milestone | **Shadow mode** — signs nothing, logs would-be trades |

---

## Go / No-Go Gate (Phase 0 — do this before building anything else)

The entire project rests on one unmeasured number: **how many milliseconds earlier does the feed show a tx than the RPC?**

Phase 0 measures it. For each observed tx: record `t_feed` (decoded from feed) and `t_rpc` (first non-null `eth_getTransactionByHash` from a warm node), report the distribution.

- **p50 edge < ~30ms → stop.** There is no tradeable advantage; do not write Phases 1–4.
- **p50 edge > ~100ms → proceed.**

This gate is non-negotiable. Everything downstream is worthless if the edge isn't real.

---

## Architecture

```
wss://feed.mainnet.chain.robinhood.com
        │  (one upstream connection — RH rate-limits per CLIENT, not per connection)
        ▼
  Nitro relay (Docker, offchainlabs/nitro)  ── re-serves locally, unlimited consumers
        ▼
┌─────────────────────────────────────────────────────────┐
│ hoodsniper daemon (Go)                                   │
│                                                          │
│  feed ──► decode ──► watchlist match ──► swap decode     │
│                            │                             │
│                            ▼                             │
│                      filter chain (tiered)               │
│                            │                             │
│                    ┌───────┴───────┐                     │
│                 shadow          execute                  │
│                    └───────┬───────┘                     │
│                            ▼                             │
│                    store (SQLite) ──► web dashboard      │
└─────────────────────────────────────────────────────────┘
        │
        ▼  RPC (Alchemy: https://robinhood-mainnet.g.alchemy.com/v2/{KEY})
```

**Why the local relay:** Robinhood rate-limits per client, not per connection. Opening N sockets splits one budget N ways. The official relay costs ~23MB RAM and ~1.6% of a core.

### Feed decoding — key implementation decision

Do **not** import `github.com/offchainlabs/nitro/broadcastclient`. Nitro's `go.mod` carries a `replace` directive pointing go-ethereum at a vendored fork, which makes it hostile to use as a library alongside upstream `go-ethereum`.

**Instead:** decode the broadcaster wire format directly (~150 LOC). It is JSON over WebSocket; the L2 message payload is base64. Handle only the kinds that matter — `L2MessageKind_SignedTx (4)` and `L2MessageKind_Batch (3)` — and use plain `go-ethereum` for tx types, RLP, and signing.

Sender recovery: `types.Sender(types.LatestSignerForChainID(big.NewInt(4663)), tx)`.

On connect, send header `Arbitrum-Requested-Sequence-Number: <last seen>`. Omitting it replays ~1,200 backlog messages (~124s of staleness); an out-of-range value replays the **entire** backlog.

---

## Latency Budget

The race is against other feed consumers, not the KOL. Realistic window: 50–300ms. Filters are tiered so the hot path never blocks on a chain of RPC calls.

| Tier | Cost | Checks | Blocking? |
|---|---|---|---|
| 0 | ~0ms (in-memory) | KOL wallet match, known-router match, token allow/blocklist, **vetted-token cache** | Yes |
| 1 | ~10–20ms (**one** multicall) | Reserves/liquidity, LP burn+lock balances, total supply, decimals | Yes |
| 2 | async, post-entry | Honeypot sell-sim, holder concentration, tax measurement, source verification | No — feeds exit logic, not entry |

Rule: **Tier 1 is a single batched multicall.** If a check cannot fit in that multicall, it belongs in Tier 2.

---

## Filters

| Filter | Tier | Config key |
|---|---|---|
| Min pool liquidity (ETH) | 1 | `min_liquidity_eth` |
| LP burnt or locked | 1 | `require_lp_secured` (checks `0x…dEaD`, `0x0`, known locker contracts; `min_lp_locked_pct`) |
| Token age / block age | 1 | `min_token_age_blocks` |
| Max buy/sell tax | 2 | `max_buy_tax_bps`, `max_sell_tax_bps` |
| Honeypot (sell simulation) | 2 | `reject_honeypot` |
| Holder concentration | 2 | `max_top_holder_pct` |
| Ownership renounced | 2 | `require_renounced` |
| Explicit allow/blocklist | 0 | `token_allowlist`, `token_blocklist` |

Filters are **pure functions** returning a new `Decision` value — no mutation of the input intent (per `coding-style.md`). Every decision is persisted with its reason, so shadow-mode output is auditable.

---

## Chain-Specific Risks (must be handled, not deferred)

1. **Feed messages are soft confirmations.** Nothing is settled. A sequencer failover can reorg what you saw. Confirm against a node before treating a position as real. Detect reorgs by comparing decoded tx sets against the envelope's `blockHash` at a sequence number already seen.
2. **ArbOS 61 compliance filter.** An authorised party can register a tx hash and the chain **voids it** — still included in a block, but `status 0x0`, no logs, **gas fully burned**. This applies to *your* transactions. Implement the `is_filtered_call(tx_hash)` `eth_call` probe and account for burned gas in P&L.
3. **Rate limits.** Public endpoints are documented as rate-limited and explicitly not for production. Use Alchemy (recommended provider) or QuickNode/dRPC/Blockdaemon/Validation Cloud.
4. **Geo-restriction.** Robinhood Stock Tokens are not offered to US persons, and are blocked in the UK, Canada, Switzerland, the UAE, and sanctioned jurisdictions. This spec targets permissionless ERC-20s rather than Stock Tokens, but the restriction is worth knowing before funding a wallet.
5. **Key handling.** Private key from env var or file with `0600` perms — never in YAML, never in the repo, never reachable from the dashboard.

---

## Layout

Many small files, organised by domain (per `coding-style.md`):

```
cmd/hoodsniper/main.go
internal/config/      YAML load + strict validation at startup
internal/feed/        relay client, wire decode, sender recovery, reorg detect
internal/watchlist/   KOL wallets, hot reload
internal/decode/      router calldata → SwapIntent (v2/v3/v4 + UniversalRouter)
internal/filter/      tiered filter chain, pure Decision functions
internal/exec/        build/sign/submit, nonce management, slippage
internal/position/    position tracking, exit rules
internal/shadow/      paper-trade recorder + counterfactual fill pricing
internal/store/       SQLite: trades, decisions, latency samples
internal/web/         read-only dashboard (go:embed)
docker-compose.yml    Nitro relay
```

**Router addresses are deliberately not hardcoded in this spec** — resolve Uniswap v2/v3/v4 + UniversalRouter deployments on chain 4663 from the Uniswap deployments registry and verify each on `robinhoodchain.blockscout.com` at implementation time. Do not guess them.

---

## Phases

| # | Deliverable | Done when |
|---|---|---|
| **0** | Feed tap + latency benchmark | Prints KOL txs live; reports p50/p95 feed-vs-RPC edge. **Go/no-go gate.** |
| **1** | Swap decode + filter chain + shadow mode | Logs every would-be trade with entry price, filter verdict, and reason. Zero signing. |
| **2** | Live execution | Works end-to-end on **testnet (chain 46630)** first, then mainnet with per-trade ETH cap + daily-loss kill switch. |
| **3** | Dashboard + persistence | SQLite-backed; live feed, positions, P&L, filter decisions, latency chart. |
| **4** | Exit strategy | Take-profit / stop-loss / trailing / time-based; KOL-sell-triggered exit. |

---

## Verification

- **Phase 0:** run against mainnet feed read-only for 1 hour; assert p50 latency edge exceeds the gate. Unit-test the wire decoder against captured feed frames (golden files).
- **Phase 1:** replay captured frames through the filter chain; assert decisions are deterministic and reproducible. Table tests per filter, including a known-honeypot and a known-locked-LP token.
- **Phase 2:** testnet 46630 end-to-end — fund a throwaway wallet, watch a wallet you control, confirm a real swap lands. Verify the compliance-filter probe and the kill switch both fire correctly.
- **Ongoing:** every phase leaves one runnable check (`go test ./...`); no framework beyond stdlib `testing`.

---

## Sources

- [Robinhood Chain — Connecting](https://docs.robinhood.com/chain/connecting)
- [chainstacklabs/robinhood-chain-sequencer-feed](https://github.com/chainstacklabs/robinhood-chain-sequencer-feed) — reference decoder
- [Chainstack: What is Robinhood Chain?](https://chainstack.com/what-is-robinhood-chain/) — sequencing model
- [Uniswap is Live on Robinhood Chain](https://blog.uniswap.org/robinhood-chain-is-live)
- [Arbitrum: How to read the sequencer feed](https://docs.arbitrum.io/run-arbitrum-node/sequencer/read-sequencer-feed)
- [Arbitrum Timeboost](https://docs.arbitrum.io/how-arbitrum-works/timeboost/gentle-introduction) — not enabled on RH Chain
