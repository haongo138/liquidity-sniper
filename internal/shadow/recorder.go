// Package shadow records the trades the daemon would have made.
//
// Output is JSONL rather than a database: it is append-only, greppable, needs no
// driver, and the dashboard can tail it. Every record carries the full check
// list, so a decision can be re-argued weeks later without rerunning anything.
package shadow

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/antono/hoodsniper/internal/filter"
	"github.com/ethereum/go-ethereum/common"
)

// Record is one observation of a watched wallet's swap.
type Record struct {
	At       time.Time `json:"at"`
	SeqNum   uint64    `json:"seq"`
	TxHash   string    `json:"tx_hash"`
	KOL      string    `json:"kol"`
	Approved bool      `json:"approved"`
	Reason   string    `json:"reason"`

	// DetectLatencyMS is feed-frame to decision. It is the budget actually
	// spent racing other feed consumers, and the number to watch if entries
	// start landing late.
	DetectLatencyMS float64 `json:"detect_latency_ms"`

	Direction string `json:"direction"`
	Venue     string `json:"venue"`
	Token     string `json:"token"`

	// KOLAmountInWei is what the watched wallet spent.
	KOLAmountInWei string `json:"kol_amount_in_wei"`
	// WouldTradeWei is what we would have spent copying it.
	WouldTradeWei string `json:"would_trade_wei"`

	Pool          string `json:"pool,omitempty"`
	PoolFeeTier   uint32 `json:"pool_fee_tier,omitempty"`
	PoolLiquidity string `json:"pool_liquidity_wei,omitempty"`

	Checks []filter.Check `json:"checks"`
}

// Recorder appends records to a JSONL file.
type Recorder struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	seen struct {
		approved int
		rejected int
	}
}

// NewRecorder opens path for appending.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Recorder{f: f, enc: json.NewEncoder(f)}, nil
}

// Write appends one record and returns it, so the caller can log it too.
func (r *Recorder) Write(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Approved {
		r.seen.approved++
	} else {
		r.seen.rejected++
	}
	return r.enc.Encode(rec)
}

// Totals reports how many decisions went each way.
func (r *Recorder) Totals() (approved, rejected int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen.approved, r.seen.rejected
}

// Close flushes and closes the file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// Build assembles a Record from the pieces the daemon has on hand.
//
// It deliberately does not simulate a fill price: an honest entry price needs a
// quoter call against the post-KOL pool state, which belongs with real
// execution. Recording a made-up fill would make the shadow log look more
// authoritative than it is.
func Build(
	seq uint64, txHash common.Hash, kol common.Address,
	intent decode.SwapIntent, state chain.TokenState, dec filter.Decision,
	wouldTrade *big.Int, detectLatency time.Duration, now time.Time,
) Record {
	rec := Record{
		At:              now,
		SeqNum:          seq,
		TxHash:          txHash.Hex(),
		KOL:             kol.Hex(),
		Approved:        dec.Approved,
		Reason:          dec.Summary(),
		DetectLatencyMS: float64(detectLatency.Microseconds()) / 1000,
		Direction:       string(intent.Direction),
		Venue:           string(intent.Venue),
		Token:           intent.Token().Hex(),
		KOLAmountInWei:  bigStr(intent.AmountIn),
		WouldTradeWei:   bigStr(wouldTrade),
		Checks:          dec.Checks,
	}
	if state.Pool != nil {
		rec.Pool = state.Pool.Address.Hex()
		rec.PoolFeeTier = state.Pool.FeeTier
		rec.PoolLiquidity = bigStr(state.Pool.WETHLiquidity)
	}
	return rec
}

// Line renders a record for terminal output.
func (rec Record) Line() string {
	mark := "REJECT"
	if rec.Approved {
		mark = "APPROVE"
	}
	return fmt.Sprintf("%-7s %-4s %s  seq %d  %.1fms  %s",
		mark, rec.Direction, rec.Token, rec.SeqNum, rec.DetectLatencyMS, rec.Reason)
}

func bigStr(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}
