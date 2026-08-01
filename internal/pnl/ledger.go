package pnl

import (
	"bufio"
	"encoding/json"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// The explorer serves a rolling ~50-transaction window per address, which caps
// any wallet at a handful of complete round trips. That is far too few to tell
// skill from luck — re-measuring a four-wallet shortlist flipped one from +67.7%
// to -19.4%, and another's entire edge came from a single trade.
//
// The ledger removes that ceiling. The sequencer feed already carries every
// transaction on the chain for free, so recording them append-only builds
// history that grows without bound and survives restarts. Scoring then reads the
// ledger instead of the explorer.
//
// Only cheap data goes in: what the calldata already told us. Receipts are
// fetched later, on demand, for the bounded set of wallets actually being
// scored — fetching one per observed swap would be thousands of calls a minute.

// Entry is one observed router interaction.
type Entry struct {
	At     time.Time `json:"at"`
	Seq    uint64    `json:"seq"`
	Hash   string    `json:"hash"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Value  string    `json:"value"`
	Token  string    `json:"token,omitempty"`
	Dir    string    `json:"dir,omitempty"`
	Amount string    `json:"amount_in,omitempty"`
}

// Ledger is an append-only JSONL store of observed swaps.
type Ledger struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
	n   int
}

// OpenLedger opens path for appending, creating it if needed.
func OpenLedger(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Ledger{f: f, enc: json.NewEncoder(f)}, nil
}

// Append records one entry.
func (l *Ledger) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.enc.Encode(e)
}

// Count returns how many entries this process has written.
func (l *Ledger) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// Close flushes and closes the file.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// LoadLedger reads a ledger and groups transactions by sending wallet.
//
// Duplicate hashes are collapsed: a restart replays part of the feed backlog,
// and counting a swap twice would double its weight in the P&L.
func LoadLedger(path string) (map[common.Address][]Tx, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[common.Address][]Tx{}
	seen := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a torn final line after a kill is not fatal
		}
		if e.Hash == "" || seen[e.Hash] {
			continue
		}
		seen[e.Hash] = true

		v, ok := new(big.Int).SetString(e.Value, 10)
		if !ok {
			v = big.NewInt(0)
		}
		w := common.HexToAddress(e.From)
		out[w] = append(out[w], Tx{
			Hash: e.Hash, Block: e.Seq, When: e.At, Value: v,
			To: e.To, From: e.From,
		})
	}
	return out, sc.Err()
}
