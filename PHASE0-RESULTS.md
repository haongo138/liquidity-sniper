# Phase 0 Results — Go/No-Go Gate

**Verdict: PROCEED.** The latency edge is real and large.

Run 2026-07-31 from Bangkok against Robinhood Chain mainnet (chain 4663), public
endpoints, 90 seconds.

## Feed decode

| Metric | Value |
|---|---|
| Blocks decoded | 1,361 |
| Transactions decoded | 15,077 |
| Undecodable frames | 0 |
| Undecodable L2 messages | 0 |

The wire decoder was validated against live mainnet: sequential block numbers,
well-formed tx hashes, recoverable senders. `go test ./...` covers the format
itself (single SignedTx, batches, corrupt-segment skip, depth guard, oversize
rejection, and the `signatureV2` envelope field name).

## Latency edge (feed vs RPC)

| Metric | Value |
|---|---|
| Samples | 390 (4 timed out) |
| min | 249 ms |
| **p50** | **763 ms** |
| p95 | 1.026 s |
| max | 1.068 s |

Gate thresholds from `SPEC.md`: stop below 30ms, proceed above 100ms. **p50 of
763ms clears the bar by ~7.6x.**

## How to read these numbers

They are directionally right and quantitatively soft. Two measurement artefacts,
both of which I verified rather than assumed:

1. **RTT dominates resolution.** Baseline round-trip to
   `rpc.mainnet.chain.robinhood.com` from this host measured **248–268ms p50**
   over 12 samples. Polls are serialised, so the effective resolution is ~250ms,
   not the configured 10ms. The reported p50 of 763ms is roughly "the third
   poll" — the true value lies somewhere in a ±250ms band.

2. **The 249ms minimum is a floor artefact.** It equals exactly one RTT, meaning
   the RPC already knew about the transaction when the first poll landed. Those
   samples measure network distance, not re-execution lag.

Netting the ~250ms of network distance out of the 763ms p50 puts the real
re-execution lag somewhere around **~500ms**. That is still five times the
proceed threshold, so the verdict does not depend on resolving the ambiguity.

**Before sizing any strategy on this number**, re-run from a host co-located
with a dedicated provider (Alchemy is Robinhood's recommended one) rather than
the rate-limited public endpoint. The decision is settled; the magnitude is not.

## Reproduce

```bash
go test ./...

# decode only
go run ./cmd/feedtap --feed mainnet --seconds 25 --all

# the gate measurement
go run ./cmd/feedtap --feed mainnet \
  --rpc https://rpc.mainnet.chain.robinhood.com \
  --seconds 90 --probe-per-block 1 --probe-interval 10ms --probe-concurrency 4

# via the local relay (preferred for sustained runs — RH rate-limits per client)
docker compose up -d --wait relay
go run ./cmd/feedtap --feed ws://127.0.0.1:9642 --rpc <your-node> --seconds 3600
```

## What this does not tell you

The edge is over **other feed consumers and RPC pollers** — never over the KOL.
The sequencer orders and executes before broadcasting, so the ordering is fixed
before you see anything. Phase 1 must be built as a backrunner; nothing measured
here changes that.
