// Package holdtime decides whether a wallet trades slowly enough to be copied.
//
// Copying a wallet you are structurally too slow to follow is a guaranteed
// loss, not a probabilistic one: if they are out in a second and detection
// alone costs two, the position is entered after the move and exited after the
// reversal, every time.
//
// The comparison needs both halves and neither is a constant:
//
//   - Their hold time. Measured per wallet from observed buy→sell pairs. The two
//     wallets sampled had medians of 13s and 33s, with a 1s minimum and a 144s
//     maximum, so a single global threshold would be wrong for both.
//   - Our latency. Measured, not assumed. The shadow run showed p50 826ms and
//     p90 2748ms, dominated by round-trip distance rather than computation, so
//     it changes with deployment and must be read at decision time.
//
// The gate is deliberately per-trade rather than per-wallet. One sampled wallet
// had only 2 of 11 trades under 3 seconds; excluding the wallet outright would
// have discarded nine good signals to avoid two bad ones.
package holdtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// DefaultMinRatio is how many times our latency their hold must exceed.
	//
	// We enter late by roughly our latency and exit late by the same, so a hold
	// of N times our latency captures about (N-2)/N of the move. At 5x that is
	// 60%, which leaves room for the ~4% round-trip fee drag. Below 3x the
	// arithmetic stops working at all.
	DefaultMinRatio = 5.0

	// DefaultMinSamples is how many completed round trips are needed before the
	// gate may reject. One or two hold times cannot establish a median — the
	// same sample-size trap that made wallet ranking noise.
	DefaultMinSamples = 3

	// maxHolds bounds the per-wallet history. Recent behaviour is what matters;
	// a wallet that used to be slow and now scalps should fail quickly.
	maxHolds = 50

	// maxLatencies bounds our own latency history.
	maxLatencies = 500

	// openPositionTTL discards entries that never produced a sell, so a wallet
	// that buys and holds forever does not leak memory.
	openPositionTTL = time.Hour
)

type posKey struct {
	wallet common.Address
	token  common.Address
}

// Tracker measures both sides of the comparison.
type Tracker struct {
	mu sync.Mutex

	// open maps an unmatched buy to when it happened.
	open map[posKey]time.Time
	// holds is each wallet's recent completed hold durations.
	holds map[common.Address][]time.Duration
	// latencies is our own recent detect-to-decision times.
	latencies []time.Duration

	lastPrune time.Time
}

// New builds an empty Tracker.
func New() *Tracker {
	return &Tracker{
		open:  map[posKey]time.Time{},
		holds: map[common.Address][]time.Duration{},
	}
}

// ObserveLatency records how long one detection took end to end.
//
// Only decisions that did the full tier-1 state read should be recorded. A
// tier-0 or ladder rejection returns in under a millisecond without touching
// the network, and mixing those in dragged the measured p90 from ~250ms down to
// 1ms — which made every wallet look followable by a factor of thirty thousand.
func (t *Tracker) ObserveLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latencies = append(t.latencies, d)
	if len(t.latencies) > maxLatencies {
		t.latencies = t.latencies[len(t.latencies)-maxLatencies:]
	}
}

// ObserveEntry records a watched wallet buying a token.
func (t *Tracker) ObserveEntry(wallet, token common.Address, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(at)
	k := posKey{wallet, token}
	// Keep the earliest unmatched buy: a laddered entry opens the position at
	// its first clip, and measuring from the last clip would understate the hold.
	if _, exists := t.open[k]; !exists {
		t.open[k] = at
	}
}

// ObserveExit records a watched wallet selling a token, completing a round trip
// if a matching buy was seen.
func (t *Tracker) ObserveExit(wallet, token common.Address, at time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := posKey{wallet, token}
	entry, ok := t.open[k]
	if !ok || at.Before(entry) {
		return 0, false
	}
	delete(t.open, k)

	hold := at.Sub(entry)
	h := append(t.holds[wallet], hold)
	if len(h) > maxHolds {
		h = h[len(h)-maxHolds:]
	}
	t.holds[wallet] = h
	return hold, true
}

// Verdict is the outcome of the gate for one wallet.
type Verdict struct {
	// Applicable is false when too few round trips have been seen to judge.
	Applicable bool
	// Pass is true when the wallet trades slowly enough to follow.
	Pass bool

	MedianHold time.Duration
	OurLatency time.Duration
	Ratio      float64
	Samples    int
}

// Reason renders the verdict for a decision record. It carries no check-name
// prefix; the filter check supplies that.
//
// The two inapplicable causes are reported separately: saying "too few round
// trips" when the real problem is that we have not measured our own latency
// sends the reader looking in the wrong place.
func (v Verdict) Reason() string {
	if !v.Applicable {
		if v.OurLatency <= 0 && v.Samples > 0 {
			return "no completed decisions yet — our own latency is unmeasured"
		}
		return fmt.Sprintf("only %d completed round trips — too few to judge", v.Samples)
	}
	if v.Pass {
		return fmt.Sprintf("median hold %s is %.1fx our p90 latency %s",
			v.MedianHold.Round(time.Millisecond), v.Ratio,
			v.OurLatency.Round(time.Millisecond))
	}
	return fmt.Sprintf("median hold %s is only %.1fx our p90 latency %s — too fast to follow",
		v.MedianHold.Round(time.Millisecond), v.Ratio,
		v.OurLatency.Round(time.Millisecond))
}

// Check reports whether a wallet can be followed.
//
// minRatio of zero disables the gate. minSamples of zero uses the default.
func (t *Tracker) Check(wallet common.Address, minRatio float64, minSamples int) Verdict {
	if minRatio <= 0 {
		return Verdict{Applicable: false, Pass: true}
	}
	if minSamples <= 0 {
		minSamples = DefaultMinSamples
	}

	t.mu.Lock()
	holds := append([]time.Duration(nil), t.holds[wallet]...)
	lat := append([]time.Duration(nil), t.latencies...)
	t.mu.Unlock()

	v := Verdict{Samples: len(holds)}
	if len(holds) < minSamples {
		// Not enough evidence to reject. Reported as inapplicable rather than
		// as a pass, so a gate that never ran is never mistaken for one that
		// approved.
		v.Pass = true
		return v
	}

	v.Applicable = true
	v.MedianHold = median(holds)
	v.OurLatency = percentile(lat, 0.90)

	// With no latency samples yet the gate cannot compare anything, so it lets
	// the trade through rather than inventing a number.
	if v.OurLatency <= 0 {
		v.Applicable = false
		v.Pass = true
		return v
	}

	v.Ratio = float64(v.MedianHold) / float64(v.OurLatency)
	v.Pass = v.Ratio >= minRatio
	return v
}

// Stats reports how much evidence the tracker holds.
func (t *Tracker) Stats() (wallets, roundTrips, latencySamples int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range t.holds {
		wallets++
		roundTrips += len(h)
	}
	return wallets, roundTrips, len(t.latencies)
}

// pruneLocked discards buys that never produced a sell. The caller holds the lock.
func (t *Tracker) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < openPositionTTL/4 {
		return
	}
	t.lastPrune = now
	for k, at := range t.open {
		if now.Sub(at) > openPositionTTL {
			delete(t.open, k)
		}
	}
}

func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func percentile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(q * float64(len(s)))
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

// SeedFromLedger primes the tracker from a cmd/collect ledger.
//
// Without this the gate is inert for hours: hold times can only be learned from
// completed round trips, and a wallet turning over every 13-144 seconds still
// needs a long run before three of them are observed live. The collector has
// already recorded months of this — replaying it makes the gate useful on the
// first decision instead of the thousandth.
//
// Entries are replayed in file order, which is chronological because the ledger
// is append-only. Out-of-order records would silently fail to match and are
// skipped by ObserveExit rather than producing a negative hold.
func (t *Tracker) SeedFromLedger(path string) (roundTrips int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	seen := map[string]struct{}{}

	for sc.Scan() {
		var e struct {
			At    time.Time `json:"at"`
			Hash  string    `json:"hash"`
			From  string    `json:"from"`
			Token string    `json:"token"`
			Dir   string    `json:"dir"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		// Only decoded swaps carry a token and direction.
		if e.Token == "" || e.Dir == "" || e.Hash == "" {
			continue
		}
		// A restart replays part of the feed backlog; counting a swap twice
		// would distort the distribution it is meant to measure.
		if _, dup := seen[e.Hash]; dup {
			continue
		}
		seen[e.Hash] = struct{}{}

		wallet := common.HexToAddress(e.From)
		token := common.HexToAddress(e.Token)
		switch e.Dir {
		case "buy":
			t.ObserveEntry(wallet, token, e.At)
		case "sell":
			if _, ok := t.ObserveExit(wallet, token, e.At); ok {
				roundTrips++
			}
		}
	}
	return roundTrips, sc.Err()
}
