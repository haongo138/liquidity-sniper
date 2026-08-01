# Phase 1 Results — Shadow Run

Run 2026-08-01 against Robinhood Chain mainnet, watching three KOL wallets.
Stopped early (external kill), 29 decisions recorded in `kol-longrun.jsonl`.

## What worked

| Metric | Result |
|---|---|
| **Decode rate** | **100.0%** (29/29) |
| Venue | 100% `universal-router` (the 0x65050A9b bot router) |
| Profile cache | 49 hits / 35 misses (58%) |
| Distinct tokens | 5 |

The heuristic bot-router decoder — reverse-engineered from an unverified proxy
with no ABI — did not miss a single transaction across two variant layouts.

## What didn't

**All 18 rejections came from the broken filter.** Every one reads
`min_liquidity: 0.21–0.23 ETH below floor 0.2500 ETH`, which is the exact
signature of the V4 blindness documented in README: a dormant V3 dust pool being
measured while the real liquidity sits in the V4 singleton.

**62% of all decisions were made by a filter known to be wrong**, and they are
almost certainly false rejections. The approve/reject split from this run must
not be treated as a strategy signal.

## Latency

| | |
|---|---|
| min | 250 ms |
| p50 | 853 ms |
| p90 | **2748 ms** |
| max | 2878 ms |
| warm-cache (<400ms) | 10/29 = 34% |

Warm decisions land at ~250–260ms, one RTT. The p90 tail is cold-cache candidate
resolution: each unknown address in bot-router calldata costs a pool lookup.

Against measured hold times — median **13s** for `0x85b605b4`, **33s** for
`0x4d9644d0` — a p50 of 853ms is ~6% of the trade and workable. The p90 of 2.7s
is ~20% of a 13s hold, which is where latency actually hurts.

## Per wallet

| Wallet | Decisions | Approved | Trade rate |
|---|---|---|---|
| `0x85b605b4…` | 17 | 9 | ~1 per 4.4 min |
| `0x4d9644d0…` | 12 | 2 | ~1 per 26 min |
| `0xe28601…` | **0** | 0 | idle the entire run |

The originally-supplied wallet produced no data at all; it has been idle since
2026-07-31 14:18Z.

## The one clean round trip

`0x85b605b4` on token `0x8CA07e1b…`:

```
5 × buy  0.0300 ETH   over ~3s    (liq 7.17 ETH)
1 × sell 1,590,857 tok ~142s later (liq 9.84 ETH)
```

Mechanical equal-clip laddering into a 0.15 ETH position, exit ~142s later.
Pool WETH rose 7.17 → 9.84 while they held, so ~2.5 ETH of other people's
buying arrived and they sold into it. That inflow is the actual edge, not speed.

**Design consequence:** copying a 5-clip ladder naively pays the router's 1% fee
five times. Ladders must be collapsed into a single sized entry, or fee drag
alone exceeds the edge.

## Blocking items before Phase 2

1. **V4 liquidity measurement** — replace pool-balance reads with an `eth_call`
   trade simulation (venue-agnostic, and yields a honeypot check for free).
2. **Ladder consolidation** — treat repeated same-token buys inside a short
   window as one signal.
3. **Hold-time gate** — skip trades where the KOL's recent median hold is not a
   comfortable multiple of our measured latency.

---

# Wallet Selection — Measured, Then Re-measured

## The activity-selected wallets lost money

The three wallets originally watched were chosen for trading often. Measured on
complete round trips only:

| Wallet | Round trips | Profitable | Return |
|---|---|---|---|
| `0x85b605b4` | 8 | 4 (50%) | **-4.45%** |
| `0x4d9644d0` | 4 | 0 (0%) | negative |
| `0xe28601` | 0 | — | unmeasurable, idle |

**4 of 12 matched round trips were profitable.** A copier pays ~4% per round
trip in fees, so none of these are copyable. `cmd/scout` was built to select on
profitability instead.

## Two measurement bugs, both found by disbelieving the output

1. **Missed sell proceeds.** Only the bot router's pattern was counted (WETH
   burned, native ETH forwarded). SwapRouter02 pays sellers in WETH *directly*,
   so every Uniswap-router seller scored exactly **-100%**. They were unreadable,
   not unprofitable.
2. **Invisible native ETH.** V4 denominates ETH as `address(0)`, not WETH, so an
   ETH-paired V4 sale emits **no Transfer log at all**. Fixed by reading
   internal transactions.

The tell for both was a suspiciously round `-100.0%`. A number that looks like a
catastrophe is more often a bug.

## The ranking does not survive re-measurement

| Wallet | scout | re-measure |
|---|---|---|
| `0x8F5537Bb` | +67.7%, 80% win | **-19.4%, 40% win** |
| `0x5638484b` | +40.1%, 67% win | +289%, 33% win (3 trips) |
| `0x3c86D511` | +38.6%, 67% win | +56.0%, 60% win |
| `0xD96304E3` | +6.7%, 60% win | 2 trips, below floor |

**Only 1 of 4 held.** Two causes:

- **Selection bias.** Excluding positions whose proceeds were invisible dropped
  the worst-looking ones, biasing scores upward. Once internal traces made them
  measurable, they turned out to be losses.
- **Sample size.** Two to six round trips cannot separate skill from luck; a
  single trade dominates the mean. `0x5638484b`'s +289% is one position
  returning +2200% on 0.015 ETH.

## Conclusion

Wallet selection is **not solved**. Ranking by measured P&L is the right
criterion, but at this sample size the ranking is noise. Making it trustworthy
needs dozens of round trips per wallet, which needs deeper history than the
explorer's rolling window provides — an archive node or a long-running collector.

**Do not arm execution against this shortlist.** The honest state is one wallet
that held up twice on 5-6 round trips, which is suggestive and nothing more.
