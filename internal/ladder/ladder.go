// Package ladder collapses a laddered entry into a single trading signal.
//
// Watched wallets slice a position into equal clips. One observed entry was
// five identical 0.03 ETH buys of the same token inside ~3 seconds, totalling
// 0.15 ETH. Mirroring each clip would pay the bot router's 1% fee five times —
// roughly 5% on entry alone, before the pool fee and before the exit, which is
// more than the edge being chased.
//
// The fix cannot be "wait for the ladder to finish and then size up". Detection
// latency is ~250ms warm and the ladder took ~3 seconds, so waiting would trade
// away the entire timing advantage the strategy depends on. Instead the FIRST
// clip acts and the rest are suppressed: our position size is our own risk
// parameter, not a mirror of theirs.
//
// Suppressed clips are reported, never silently dropped. A signal that vanishes
// without explanation is indistinguishable from a bug, and this project has
// already been bitten three times by failures that looked like results.
package ladder

import (
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// DefaultWindow is how long clips of the same ladder are expected to arrive
// within. Observed ladders completed in ~3 seconds, while genuine re-entries
// came after a full round trip — 97 and 142 seconds in the two measured cases.
// A minute sits comfortably between the two.
const DefaultWindow = 60 * time.Second

// pruneEvery bounds how often expired entries are swept. The tracker lives for
// the process lifetime and sees every watched swap, so it must not grow without
// limit on a multi-day run.
const pruneEvery = 5 * time.Minute

type key struct {
	wallet    common.Address
	token     common.Address
	direction string
}

type entry struct {
	first    time.Time
	last     time.Time
	clips    int
	totalWei *big.Int
}

// Verdict describes what to do with one observed swap.
type Verdict struct {
	// Act is true only for the clip that opens a ladder.
	Act bool
	// Clip is 1 for the first, 2 for the second, and so on.
	Clip int
	// LadderTotalWei is the running sum across the ladder so far, including
	// this clip. It is what the wallet has actually committed, which is worth
	// recording even when we do not mirror it.
	LadderTotalWei *big.Int
	// SinceFirst is how long after the opening clip this one arrived.
	SinceFirst time.Duration
}

// Reason renders the verdict for a decision record. It carries no "ladder:"
// prefix — the filter check is already named that, and adding one produced
// "ladder: ladder: clip 2 …" in the shadow log.
func (v Verdict) Reason() string {
	if v.Act {
		return "opening clip"
	}
	return "clip " + itoa(v.Clip) + " suppressed, already acted " +
		v.SinceFirst.Round(time.Millisecond).String() + " ago"
}

// Tracker collapses ladders. Safe for concurrent use: the daemon evaluates each
// matched swap in its own goroutine.
type Tracker struct {
	mu        sync.Mutex
	window    time.Duration
	seen      map[key]*entry
	lastPrune time.Time
}

// New builds a Tracker. A window of zero disables consolidation, which makes
// every clip act — useful for measuring what the fee drag would have been.
func New(window time.Duration) *Tracker {
	return &Tracker{window: window, seen: map[key]*entry{}}
}

// Enabled reports whether consolidation is active.
func (t *Tracker) Enabled() bool { return t != nil && t.window > 0 }

// Observe records one swap and reports whether to act on it.
//
// Direction is part of the key so that a sell is never suppressed by a
// preceding buy: exiting is a separate decision from entering, and swallowing
// an exit signal would leave a position with no way out.
func (t *Tracker) Observe(
	wallet, token common.Address, direction string, amount *big.Int, now time.Time,
) Verdict {
	amt := big.NewInt(0)
	if amount != nil {
		amt = new(big.Int).Set(amount)
	}
	if !t.Enabled() {
		return Verdict{Act: true, Clip: 1, LadderTotalWei: amt}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)

	k := key{wallet: wallet, token: token, direction: direction}
	e, ok := t.seen[k]
	// An expired ladder is a genuinely new signal, not a continuation.
	if !ok || now.Sub(e.first) > t.window {
		t.seen[k] = &entry{first: now, last: now, clips: 1, totalWei: amt}
		return Verdict{Act: true, Clip: 1, LadderTotalWei: new(big.Int).Set(amt)}
	}

	e.clips++
	e.last = now
	e.totalWei.Add(e.totalWei, amt)
	return Verdict{
		Act:            false,
		Clip:           e.clips,
		LadderTotalWei: new(big.Int).Set(e.totalWei),
		SinceFirst:     now.Sub(e.first),
	}
}

// pruneLocked drops entries whose window has closed. The caller holds the lock.
func (t *Tracker) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < pruneEvery {
		return
	}
	t.lastPrune = now
	for k, e := range t.seen {
		if now.Sub(e.last) > t.window {
			delete(t.seen, k)
		}
	}
}

// Stats reports consolidation effectiveness: how many ladders were opened and
// how many clips were suppressed. The ratio is the fee multiplication avoided.
func (t *Tracker) Stats() (ladders, suppressed int) {
	if !t.Enabled() {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.seen {
		ladders++
		suppressed += e.clips - 1
	}
	return ladders, suppressed
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
