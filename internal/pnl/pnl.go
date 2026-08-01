// Package pnl measures whether a wallet actually makes money.
//
// Every wallet in this project was originally chosen by ACTIVITY. That was the
// wrong criterion: the three wallets picked that way turned out to lose money,
// and copying a loser converts a small loss into a larger one once fees are
// added. This package exists so wallets can be selected by measured
// profitability instead.
//
// Method: reconstruct trades from ERC-20 Transfer logs in each receipt.
// Diffing native ETH balances would be simpler, but the public node prunes
// state after ~15,000 blocks while logs are retained, so receipts are the only
// viable source for anything older than about 25 minutes.
package pnl

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// ExplorerAPI is the Blockscout instance for Robinhood Chain.
	ExplorerAPI = "https://robinhoodchain.blockscout.com/api/v2"
	// transferTopic is Transfer(address,address,uint256).
	transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	// FeeDragPct is what a copier pays per round trip: ~1% bot router each way
	// plus ~1% pool each way. Returns below this are negative to copy.
	FeeDragPct = 4.0
)

var (
	wethAddr = common.HexToAddress("0x0bd7d308f8e1639fab988df18a8011f41eacad73")
	zeroAddr = common.Address{}
)

// Trade is one router interaction reconstructed from its receipt.
//
// Value alone cannot classify a trade. Routers differ in how they return
// proceeds — the bot router unwraps WETH and forwards native ETH (a burn to the
// zero address), while SwapRouter02 sends WETH straight to the seller. Reading
// only the burn made every SwapRouter02 seller look like it received nothing,
// scoring real wallets at exactly -100%. Direction is therefore taken from
// token flow, and value from every ETH and WETH leg.
type Trade struct {
	Hash      string
	Block     uint64
	When      time.Time
	ETHIn     *big.Int // native ETH plus WETH the wallet gave up
	ETHOut    *big.Int // WETH received, or unwrapped on the wallet's behalf
	Token     common.Address
	TokensIn  *big.Int
	TokensOut *big.Int
}

// IsBuy reports whether the wallet ended up with more of the token than it
// started with.
func (t Trade) IsBuy() bool { return t.TokensIn.Cmp(t.TokensOut) > 0 }

// Position aggregates one wallet's activity in one token.
type Position struct {
	Token           common.Address
	Spent, Received *big.Int
	Bought, Sold    *big.Int
	Buys, Sells     int
}

// Net returns received minus spent.
func (p Position) Net() *big.Int { return new(big.Int).Sub(p.Received, p.Spent) }

// Matched reports whether both legs of the round trip fall inside the observed
// window. Only these positions carry a trustworthy P&L: a buy whose sell fell
// outside looks like a total loss, and a sell whose buy fell outside looks like
// free money. Truncated positions must never be counted as either.
func (p Position) Matched() bool { return p.Buys > 0 && p.Sells > 0 }

// Measurable reports whether the proceeds of a sale were actually observable.
//
// Uniswap V4 denominates native ETH as address(0), not WETH, so an ETH-paired
// V4 swap moves real ETH and emits NO Transfer log for it. Such a sale looks
// like it returned nothing. Scoring that as -100% would libel a profitable
// wallet as a total loss and discard it, so these are reported as unmeasurable
// instead. Fixing this properly needs internal-transaction traces.
func (p Position) Measurable() bool {
	return p.Sold.Sign() == 0 || p.Received.Sign() > 0
}

// Summary is the verdict for one wallet.
type Summary struct {
	Wallet    common.Address
	Trades    int
	Buys      int
	Sells     int
	Window    time.Duration
	Positions []Position

	// Matched-only figures. These are the defensible ones.
	MatchedSpent      *big.Int
	MatchedReceived   *big.Int
	MatchedRoundTrips int
	MatchedWins       int
	// Unmeasurable counts round trips whose proceeds left no log — almost
	// always native-ETH V4 pools. These are NOT losses; they are unknowns.
	Unmeasurable int
}

// MatchedNet is realised P&L across complete round trips only.
func (s Summary) MatchedNet() *big.Int {
	return new(big.Int).Sub(s.MatchedReceived, s.MatchedSpent)
}

// MatchedReturnPct is the return on capital deployed in complete round trips,
// or 0 when nothing was deployed.
func (s Summary) MatchedReturnPct() float64 {
	if s.MatchedSpent == nil || s.MatchedSpent.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Float).Quo(
		new(big.Float).SetInt(s.MatchedNet()),
		new(big.Float).SetInt(s.MatchedSpent)).Float64()
	return f * 100
}

// WinRate is the share of complete round trips that made money.
func (s Summary) WinRate() float64 {
	if s.MatchedRoundTrips == 0 {
		return 0
	}
	return 100 * float64(s.MatchedWins) / float64(s.MatchedRoundTrips)
}

// CopyViable reports whether the wallet clears the fee drag a copier pays.
// A wallet that merely breaks even is not worth copying.
func (s Summary) CopyViable() bool {
	return s.MatchedRoundTrips >= 3 && s.MatchedReturnPct() > FeeDragPct
}

type receipt struct {
	Status string `json:"status"`
	Logs   []struct {
		Address common.Address `json:"address"`
		Topics  []string       `json:"topics"`
		Data    string         `json:"data"`
	} `json:"logs"`
}

// FetchInternalETH returns native ETH credited to the wallet, keyed by
// transaction hash.
//
// This closes the blind spot that made V4 sales unmeasurable: V4 denominates
// native ETH as address(0) rather than WETH, so an ETH-paired swap moves real
// ETH and emits no Transfer log. That movement shows up only as an internal
// transaction, which is what this reads.
func FetchInternalETH(wallet common.Address, max int) (map[string]*big.Int, error) {
	out := map[string]*big.Int{}
	base := fmt.Sprintf("%s/addresses/%s/internal-transactions",
		ExplorerAPI, wallet.Hex())
	url := base
	seen := 0

	for seen < max && url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return out, err
		}
		var body struct {
			Items []struct {
				TxHash  string                `json:"transaction_hash"`
				Value   string                `json:"value"`
				Success *bool                 `json:"success"`
				From    struct{ Hash string } `json:"from"`
				To      struct{ Hash string } `json:"to"`
			} `json:"items"`
			Next map[string]any `json:"next_page_params"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return out, err
		}

		for _, it := range body.Items {
			seen++
			if it.Success != nil && !*it.Success {
				continue
			}
			v, ok := new(big.Int).SetString(it.Value, 10)
			if !ok || v.Sign() == 0 {
				continue
			}
			// Only ETH arriving at the wallet counts as proceeds.
			if common.HexToAddress(it.To.Hash) != wallet {
				continue
			}
			if out[it.TxHash] == nil {
				out[it.TxHash] = big.NewInt(0)
			}
			out[it.TxHash].Add(out[it.TxHash], v)
		}
		if len(body.Next) == 0 || len(body.Items) == 0 {
			break
		}
		url = fmt.Sprintf("%s?block_number=%v&index=%v&transaction_index=%v", base,
			body.Next["block_number"], body.Next["index"], body.Next["transaction_index"])
	}
	return out, nil
}

// receiptBatchSize bounds one JSON-RPC batch.
//
// Providers meter compute units per second rather than requests:
// eth_getTransactionReceipt costs ~15 CU, so fifty of them is ~750 CU against a
// budget nearer 330 CU/s. Twenty keeps a single batch inside roughly one
// second's allowance, which is what makes the pacing below workable.
const receiptBatchSize = 20

// batchPace is the minimum gap between batches, sized so sustained throughput
// stays under the compute-unit budget. A burst of nine large batches succeeds
// and then the next minute of work is rejected, so pacing beats retrying.
const batchPace = 1200 * time.Millisecond

// fetchReceipts retrieves receipts in batches, keyed by transaction hash.
// Individual failures are skipped rather than aborting the wallet: one missing
// receipt should cost one trade, not the whole measurement.
func fetchReceipts(ctx context.Context, client *rpc.Client, txs []Tx) (map[string]receipt, error) {
	out := make(map[string]receipt, len(txs))
	failed := 0

	for start := 0; start < len(txs); start += receiptBatchSize {
		end := start + receiptBatchSize
		if end > len(txs) {
			end = len(txs)
		}
		chunk := txs[start:end]

		batch := make([]rpc.BatchElem, len(chunk))
		results := make([]receipt, len(chunk))
		for i, t := range chunk {
			batch[i] = rpc.BatchElem{
				Method: "eth_getTransactionReceipt",
				Args:   []any{t.Hash},
				Result: &results[i],
			}
		}
		if err := batchWithRetry(ctx, client, batch); err != nil {
			// Swallowing this produced wallets scored as "0 trades" that were
			// really "0 receipts fetched" — a measurement failure wearing the
			// costume of a result. Surface it and stop, so a partial fetch can
			// never be mistaken for a complete history.
			return out, fmt.Errorf("receipt batch %d-%d of %d: %w",
				start, end, len(txs), err)
		}
		for i, e := range batch {
			if e.Error != nil {
				failed++
				continue
			}
			out[chunk[i].Hash] = results[i]
		}
	}
	// Per-element errors inside an otherwise-successful batch were previously
	// skipped one by one, so a wholly failed fetch returned an empty map and no
	// error — which downstream rendered as "0 trades" for a wallet with 425 of
	// them. A wallet we could not measure must never look like one that did
	// nothing.
	if len(out) == 0 && len(txs) > 0 {
		return out, fmt.Errorf("no receipts retrieved for %d transactions (%d rejected) "+
			"— the endpoint is refusing the batch", len(txs), failed)
	}
	if failed > len(txs)/2 {
		return out, fmt.Errorf("%d of %d receipts rejected — result would be partial",
			failed, len(txs))
	}
	return out, nil
}

// batchGate serialises receipt batches across every worker.
//
// Providers limit compute units per second, not requests. Nine sequential
// batches of fifty receipts complete in under a second; the same work split
// across three workers is rejected outright. Parallelism belongs in the
// explorer fetches and the decoding, not in hammering one rate-limited
// endpoint from several goroutines at once.
var (
	batchGate sync.Mutex
	lastBatch time.Time
)

// waitForSlot paces batches. The caller holds batchGate.
func waitForSlot(ctx context.Context) error {
	if gap := time.Since(lastBatch); gap < batchPace {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(batchPace - gap):
		}
	}
	lastBatch = time.Now()
	return nil
}

// batchWithRetry sends a batch, backing off on rate limits.
//
// Every provider throttles, including paid ones — Alchemy accepts nine
// sequential batches happily and rejects the same work sent from several
// workers at once. Treating a 429 as a fatal error discards a wallet we could
// have measured a moment later; treating it as an empty result would be worse
// still, since that reads as "this wallet did nothing".
func batchWithRetry(ctx context.Context, client *rpc.Client, batch []rpc.BatchElem) error {
	const maxAttempts = 6
	delay := 400 * time.Millisecond

	batchGate.Lock()
	defer batchGate.Unlock()

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := waitForSlot(ctx); err != nil {
			return err
		}
		err = client.BatchCallContext(ctx, batch)
		if err == nil {
			// A batch can also be throttled per element while the call itself
			// succeeds, which is the shape that previously went unnoticed.
			if !anyRateLimited(batch) {
				return nil
			}
			err = fmt.Errorf("429 Too Many Requests")
		} else if !isRateLimit(err) {
			return err
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return err
}

func anyRateLimited(batch []rpc.BatchElem) bool {
	for _, e := range batch {
		if e.Error != nil && isRateLimit(e.Error) {
			return true
		}
	}
	return false
}

func isRateLimit(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") || strings.Contains(s, "too many requests") ||
		strings.Contains(s, "rate limit") || strings.Contains(s, "capacity")
}

// Measure reconstructs trades for a wallet from its transactions.
//
// internalETH may be nil; when supplied it credits native ETH that left no
// Transfer log, which is required for V4 pools paired against native ETH.
func Measure(ctx context.Context, client *rpc.Client, wallet common.Address, txs []Tx, internalETH map[string]*big.Int) ([]Trade, error) {
	sort.Slice(txs, func(i, j int) bool { return txs[i].Block < txs[j].Block })

	// Receipts dominate the cost of scoring: a wallet with 425 trades needs 425
	// of them, and fetched one at a time against a 250ms-RTT node that is ~100
	// seconds for a single wallet. Batching turns it into a handful of round
	// trips.
	receipts, err := fetchReceipts(ctx, client, txs)
	if err != nil {
		return nil, err
	}

	var out []Trade
	for _, t := range txs {
		r, ok := receipts[t.Hash]
		if !ok {
			continue
		}
		// Status 0x0 covers both reverts and transactions voided by the ArbOS
		// compliance filter. Gas was burned, but no trade happened.
		if r.Status != "0x1" {
			continue
		}

		tr := Trade{Hash: t.Hash, Block: t.Block, When: t.When,
			ETHIn: new(big.Int).Set(t.Value), ETHOut: big.NewInt(0),
			TokensIn: big.NewInt(0), TokensOut: big.NewInt(0)}

		for _, lg := range r.Logs {
			if len(lg.Topics) != 3 || lg.Topics[0] != transferTopic {
				continue
			}
			from := common.HexToAddress(lg.Topics[1])
			to := common.HexToAddress(lg.Topics[2])
			amt := hexBig(lg.Data)
			if amt == nil {
				continue
			}

			if lg.Address == wethAddr {
				switch {
				case from == wallet && to == zeroAddr:
					// The wallet unwrapping its own WETH. Not a trade leg, and
					// counting it as spend would double-charge the position.
				case from == wallet:
					tr.ETHIn.Add(tr.ETHIn, amt)
				case to == wallet:
					// SwapRouter02 pays the seller in WETH directly.
					tr.ETHOut.Add(tr.ETHOut, amt)
				case to == zeroAddr:
					// A router unwrapping on the wallet's behalf, then
					// forwarding native ETH (which leaves no log).
					tr.ETHOut.Add(tr.ETHOut, amt)
				}
				continue
			}
			if to == wallet {
				tr.Token = lg.Address
				tr.TokensIn.Add(tr.TokensIn, amt)
			}
			if from == wallet {
				tr.Token = lg.Address
				tr.TokensOut.Add(tr.TokensOut, amt)
			}
		}
		// Native ETH refunded or paid out leaves no log; credit it here.
		if v := internalETH[t.Hash]; v != nil {
			tr.ETHOut.Add(tr.ETHOut, v)
		}

		// No token moved, so this is an approve, an unwrap, or a transfer —
		// not a trade.
		if tr.Token == zeroAddr || (tr.TokensIn.Sign() == 0 && tr.TokensOut.Sign() == 0) {
			continue
		}
		out = append(out, tr)
	}
	return out, nil
}

// Summarize aggregates trades into per-token positions and a verdict.
func Summarize(wallet common.Address, trades []Trade) Summary {
	s := Summary{Wallet: wallet, Trades: len(trades),
		MatchedSpent: big.NewInt(0), MatchedReceived: big.NewInt(0)}
	if len(trades) == 0 {
		return s
	}

	byToken := map[common.Address]*Position{}
	var order []common.Address
	for _, t := range trades {
		p := byToken[t.Token]
		if p == nil {
			p = &Position{Token: t.Token, Spent: big.NewInt(0), Received: big.NewInt(0),
				Bought: big.NewInt(0), Sold: big.NewInt(0)}
			byToken[t.Token] = p
			order = append(order, t.Token)
		}
		// Both legs are recorded regardless of direction: a swap can move ETH
		// in and out of the same transaction, and dropping either side would
		// understate cost or proceeds.
		p.Spent.Add(p.Spent, t.ETHIn)
		p.Received.Add(p.Received, t.ETHOut)
		if t.IsBuy() {
			s.Buys++
			p.Buys++
			p.Bought.Add(p.Bought, t.TokensIn)
		} else {
			s.Sells++
			p.Sells++
			p.Sold.Add(p.Sold, t.TokensOut)
		}
	}

	for _, tok := range order {
		p := *byToken[tok]
		s.Positions = append(s.Positions, p)
		if !p.Matched() {
			continue
		}
		if !p.Measurable() {
			s.Unmeasurable++
			continue
		}
		s.MatchedRoundTrips++
		s.MatchedSpent.Add(s.MatchedSpent, p.Spent)
		s.MatchedReceived.Add(s.MatchedReceived, p.Received)
		if p.Net().Sign() > 0 {
			s.MatchedWins++
		}
	}
	s.Window = trades[len(trades)-1].When.Sub(trades[0].When)
	return s
}

// Tx is the subset of a transaction this package needs.
type Tx struct {
	Hash  string
	Block uint64
	When  time.Time
	Value *big.Int
	To    string
	From  string
}

// FetchTxs pulls a wallet's outgoing transactions from the explorer.
func FetchTxs(wallet common.Address, max int) ([]Tx, error) {
	return fetchPaged(fmt.Sprintf("%s/addresses/%s/transactions?filter=from",
		ExplorerAPI, wallet.Hex()), max)
}

// FetchIncoming pulls transactions sent TO an address — used to enumerate every
// wallet that traded through a router.
func FetchIncoming(target common.Address, max int) ([]Tx, error) {
	return fetchPaged(fmt.Sprintf("%s/addresses/%s/transactions?filter=to",
		ExplorerAPI, target.Hex()), max)
}

func fetchPaged(url string, max int) ([]Tx, error) {
	var out []Tx
	base := url

	for len(out) < max && url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return out, err
		}
		var body struct {
			Items []struct {
				Hash        string                `json:"hash"`
				BlockNumber uint64                `json:"block_number"`
				Timestamp   string                `json:"timestamp"`
				Value       string                `json:"value"`
				From        struct{ Hash string } `json:"from"`
				To          struct{ Hash string } `json:"to"`
			} `json:"items"`
			Next map[string]any `json:"next_page_params"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return out, err
		}
		for _, it := range body.Items {
			v, ok := new(big.Int).SetString(it.Value, 10)
			if !ok {
				v = big.NewInt(0)
			}
			ts, _ := time.Parse(time.RFC3339, it.Timestamp)
			out = append(out, Tx{Hash: it.Hash, Block: it.BlockNumber, When: ts,
				Value: v, To: it.To.Hash, From: it.From.Hash})
			if len(out) >= max {
				break
			}
		}
		if len(body.Next) == 0 || len(body.Items) == 0 {
			break
		}
		url = fmt.Sprintf("%s&block_number=%v&index=%v", base,
			body.Next["block_number"], body.Next["index"])
	}
	return out, nil
}

func hexBig(s string) *big.Int {
	if len(s) < 3 {
		return nil
	}
	v, ok := new(big.Int).SetString(s[2:], 16)
	if !ok {
		return nil
	}
	return v
}

// ETH renders wei as a signed ETH string.
func ETH(v *big.Int) string {
	if v == nil {
		return "0.000000"
	}
	f := new(big.Float).Quo(new(big.Float).SetInt(v), big.NewFloat(1e18))
	s := f.Text('f', 6)
	if v.Sign() > 0 {
		return "+" + s
	}
	return s
}
