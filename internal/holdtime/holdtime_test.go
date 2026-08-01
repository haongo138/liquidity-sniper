package holdtime

import (
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	slow  = common.HexToAddress("0x4d9644d05FE2123b4eAfa8d7fD31B0EA430726f3")
	quick = common.HexToAddress("0x85b605b47a5323912615cb8Af834BB1c4716b794")
	token = common.HexToAddress("0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64")
	other = common.HexToAddress("0x5DD7184e28121837Ede59E7a3185C8697f90b172")
)

// feed records n round trips of the given hold for a wallet.
func feed(t *Tracker, w common.Address, hold time.Duration, n int) {
	base := time.Now()
	for i := 0; i < n; i++ {
		tok := common.BigToAddress(common.Big1)
		if i%2 == 0 {
			tok = token
		} else {
			tok = other
		}
		at := base.Add(time.Duration(i) * time.Hour)
		t.ObserveEntry(w, tok, at)
		t.ObserveExit(w, tok, at.Add(hold))
	}
}

func TestRoundTripMeasured(t *testing.T) {
	tr := New()
	now := time.Now()
	tr.ObserveEntry(quick, token, now)
	hold, ok := tr.ObserveExit(quick, token, now.Add(97*time.Second))
	if !ok {
		t.Fatal("exit did not match the entry")
	}
	if hold != 97*time.Second {
		t.Errorf("hold = %s, want 97s", hold)
	}
	// The position is closed; a second sell has nothing to match.
	if _, ok := tr.ObserveExit(quick, token, now.Add(200*time.Second)); ok {
		t.Error("a second exit matched an already-closed position")
	}
}

// A laddered entry opens the position at its first clip. Measuring from the
// last clip would understate the hold.
func TestLadderedEntryUsesFirstClip(t *testing.T) {
	tr := New()
	now := time.Now()
	tr.ObserveEntry(quick, token, now)
	tr.ObserveEntry(quick, token, now.Add(1*time.Second))
	tr.ObserveEntry(quick, token, now.Add(3*time.Second))

	hold, ok := tr.ObserveExit(quick, token, now.Add(60*time.Second))
	if !ok {
		t.Fatal("exit did not match")
	}
	if hold != 60*time.Second {
		t.Errorf("hold = %s, want 60s measured from the first clip", hold)
	}
}

func TestExitWithoutEntryIsIgnored(t *testing.T) {
	tr := New()
	if _, ok := tr.ObserveExit(quick, token, time.Now()); ok {
		t.Error("an exit with no matching entry was counted")
	}
	if _, rt, _ := tr.Stats(); rt != 0 {
		t.Errorf("round trips = %d, want 0", rt)
	}
}

// Below the sample floor the gate must report inapplicable, not pass. A gate
// that never ran must never look like one that approved.
func TestInsufficientSamplesIsNotApplicable(t *testing.T) {
	tr := New()
	tr.ObserveLatency(800 * time.Millisecond)
	feed(tr, quick, time.Second, 2)

	v := tr.Check(quick, DefaultMinRatio, DefaultMinSamples)
	if v.Applicable {
		t.Error("gate claimed to apply on 2 samples")
	}
	if !v.Pass {
		t.Error("gate rejected without enough evidence")
	}
	if v.Samples != 2 {
		t.Errorf("samples = %d, want 2", v.Samples)
	}
}

// The measured case: a wallet whose median hold is 13s against a p90 latency of
// 2.7s is 4.8x — below the 5x floor, so it must be rejected.
func TestFastWalletRejected(t *testing.T) {
	tr := New()
	tr.ObserveLatency(2748 * time.Millisecond)
	feed(tr, quick, 13*time.Second, 6)

	v := tr.Check(quick, DefaultMinRatio, DefaultMinSamples)
	if !v.Applicable {
		t.Fatal("gate did not apply with 6 samples")
	}
	if v.Pass {
		t.Errorf("13s hold vs 2.7s latency (%.1fx) passed a 5x floor", v.Ratio)
	}
	if v.MedianHold != 13*time.Second {
		t.Errorf("median = %s, want 13s", v.MedianHold)
	}
}

// The other measured wallet: 33s median against the same latency is 12x, which
// is comfortably followable.
func TestSlowWalletPasses(t *testing.T) {
	tr := New()
	tr.ObserveLatency(2748 * time.Millisecond)
	feed(tr, slow, 33*time.Second, 6)

	v := tr.Check(slow, DefaultMinRatio, DefaultMinSamples)
	if !v.Applicable {
		t.Fatal("gate did not apply")
	}
	if !v.Pass {
		t.Errorf("33s hold vs 2.7s latency (%.1fx) failed a 5x floor", v.Ratio)
	}
}

// The gate compares against OUR latency, so the same wallet is followable from
// a co-located node and not from far away.
func TestVerdictFollowsOurLatency(t *testing.T) {
	far := New()
	far.ObserveLatency(2748 * time.Millisecond)
	feed(far, quick, 13*time.Second, 6)
	if far.Check(quick, DefaultMinRatio, DefaultMinSamples).Pass {
		t.Error("13s hold passed at 2.7s latency")
	}

	near := New()
	near.ObserveLatency(60 * time.Millisecond) // co-located
	feed(near, quick, 13*time.Second, 6)
	v := near.Check(quick, DefaultMinRatio, DefaultMinSamples)
	if !v.Pass {
		t.Errorf("13s hold failed at 60ms latency (%.1fx) — co-location should help", v.Ratio)
	}
}

// Without latency samples there is nothing to compare against, so the gate must
// stand down rather than invent a number.
func TestNoLatencySamplesStandsDown(t *testing.T) {
	tr := New()
	feed(tr, quick, time.Second, 6)
	v := tr.Check(quick, DefaultMinRatio, DefaultMinSamples)
	if v.Applicable {
		t.Error("gate applied with no latency samples")
	}
	if !v.Pass {
		t.Error("gate rejected with nothing to compare against")
	}
}

func TestZeroRatioDisables(t *testing.T) {
	tr := New()
	tr.ObserveLatency(5 * time.Second)
	feed(tr, quick, time.Millisecond, 10) // hopeless, but the gate is off
	v := tr.Check(quick, 0, DefaultMinSamples)
	if v.Applicable || !v.Pass {
		t.Errorf("disabled gate did not pass: applicable=%v pass=%v", v.Applicable, v.Pass)
	}
}

// Per-wallet, not global: one wallet being unfollowable must not gate another.
func TestWalletsAreIndependent(t *testing.T) {
	tr := New()
	tr.ObserveLatency(2748 * time.Millisecond)
	feed(tr, quick, 2*time.Second, 6)
	feed(tr, slow, 60*time.Second, 6)

	if tr.Check(quick, DefaultMinRatio, DefaultMinSamples).Pass {
		t.Error("fast wallet passed")
	}
	if !tr.Check(slow, DefaultMinRatio, DefaultMinSamples).Pass {
		t.Error("slow wallet was gated by the fast one")
	}
}

func TestReasonIsInformative(t *testing.T) {
	tr := New()
	tr.ObserveLatency(2748 * time.Millisecond)
	feed(tr, quick, 13*time.Second, 6)

	got := tr.Check(quick, DefaultMinRatio, DefaultMinSamples).Reason()
	for _, want := range []string{"median hold", "13s", "too fast to follow"} {
		if !contains(got, want) {
			t.Errorf("reason %q missing %q", got, want)
		}
	}
}

// The tracker runs for the process lifetime, so unmatched buys must not leak.
func TestOpenPositionsArePruned(t *testing.T) {
	tr := New()
	base := time.Now()
	for i := 0; i < 200; i++ {
		tr.ObserveEntry(quick, common.BigToAddress(common.Big1), base)
		tr.ObserveEntry(common.BigToAddress(common.Big2), token, base)
	}
	// Past both the TTL and the prune interval.
	tr.ObserveEntry(slow, other, base.Add(openPositionTTL+time.Minute))

	tr.mu.Lock()
	n := len(tr.open)
	tr.mu.Unlock()
	if n > 5 {
		t.Errorf("retained %d open positions after prune", n)
	}
}

func TestConcurrentUse(t *testing.T) {
	tr := New()
	var wg sync.WaitGroup
	base := time.Now()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w := common.BigToAddress(common.Big1)
			for j := 0; j < 100; j++ {
				at := base.Add(time.Duration(n*100+j) * time.Second)
				tr.ObserveEntry(w, token, at)
				tr.ObserveExit(w, token, at.Add(20*time.Second))
				tr.ObserveLatency(time.Duration(j) * time.Millisecond)
				_ = tr.Check(w, DefaultMinRatio, DefaultMinSamples)
			}
		}(i)
	}
	wg.Wait()

	if _, rt, ls := tr.Stats(); rt == 0 || ls == 0 {
		t.Errorf("nothing recorded under concurrency: roundTrips=%d latencies=%d", rt, ls)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
