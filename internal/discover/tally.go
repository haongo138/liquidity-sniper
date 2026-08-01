// Package discover answers "which contracts and selectors actually carry the
// flow" from live feed traffic, rather than from a hardcoded address list that
// silently goes stale.
//
// A wrong router address means zero detections and no error, so this exists to
// keep that guess out of the codebase.
package discover

import (
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Key identifies a call target and the function invoked on it.
type Key struct {
	To       common.Address
	Selector [4]byte
}

// Count pairs a Key with how often it was seen.
type Count struct {
	Key
	N       int
	Senders int
}

// Tally accumulates destination/selector frequencies. Safe for concurrent use.
type Tally struct {
	mu     sync.Mutex
	counts map[Key]int
	// senders tracks distinct callers per key — a router used by many wallets
	// looks very different from a bot hammering its own contract.
	senders map[Key]map[common.Address]struct{}
	total   int
}

// New builds an empty Tally.
func New() *Tally {
	return &Tally{
		counts:  map[Key]int{},
		senders: map[Key]map[common.Address]struct{}{},
	}
}

// Observe records one transaction. Transactions without a destination (contract
// creations) or without a 4-byte selector are counted but not keyed.
func (t *Tally) Observe(tx *types.Transaction, from common.Address) {
	to := tx.To()
	if to == nil {
		return
	}
	data := tx.Data()
	if len(data) < 4 {
		return
	}
	k := Key{To: *to}
	copy(k.Selector[:], data[:4])

	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[k]++
	t.total++
	if t.senders[k] == nil {
		t.senders[k] = map[common.Address]struct{}{}
	}
	t.senders[k][from] = struct{}{}
}

// Top returns the n most frequent keys, highest first.
func (t *Tally) Top(n int) []Count {
	t.mu.Lock()
	out := make([]Count, 0, len(t.counts))
	for k, c := range t.counts {
		out = append(out, Count{Key: k, N: c, Senders: len(t.senders[k])})
	}
	t.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Senders > out[j].Senders
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Total returns how many keyed calls were observed.
func (t *Tally) Total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}
