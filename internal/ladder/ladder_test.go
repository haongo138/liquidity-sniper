package ladder

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	wallet = common.HexToAddress("0x85b605b47a5323912615cb8Af834BB1c4716b794")
	token  = common.HexToAddress("0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64")
	other  = common.HexToAddress("0x5DD7184e28121837Ede59E7a3185C8697f90b172")
)

func eth(f float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil)
	return v
}

// The observed ladder: five 0.03 ETH clips of the same token in ~3 seconds.
// Exactly one must act, or the router's 1% fee is paid five times.
func TestObservedLadderCollapsesToOneSignal(t *testing.T) {
	tr := New(DefaultWindow)
	base := time.Now()
	offsets := []time.Duration{0, 500 * time.Millisecond, 700 * time.Millisecond,
		1800 * time.Millisecond, 3 * time.Second}

	acted := 0
	var last Verdict
	for i, off := range offsets {
		v := tr.Observe(wallet, token, "buy", eth(0.03), base.Add(off))
		if v.Act {
			acted++
			if i != 0 {
				t.Errorf("clip %d acted; only the opening clip should", i+1)
			}
		}
		last = v
	}
	if acted != 1 {
		t.Fatalf("acted on %d clips, want exactly 1", acted)
	}
	if last.Clip != 5 {
		t.Errorf("final clip index = %d, want 5", last.Clip)
	}
	// The aggregate is worth recording even though we do not mirror it.
	if want := eth(0.15); last.LadderTotalWei.Cmp(want) != 0 {
		t.Errorf("ladder total = %s, want %s", last.LadderTotalWei, want)
	}
}

// A sell must never be suppressed by a preceding buy. Swallowing an exit would
// leave a position with no way out.
func TestSellNotSuppressedByBuy(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()

	if v := tr.Observe(wallet, token, "buy", eth(0.03), now); !v.Act {
		t.Fatal("opening buy did not act")
	}
	if v := tr.Observe(wallet, token, "sell", eth(1000), now.Add(time.Second)); !v.Act {
		t.Fatal("sell was suppressed by a preceding buy — the exit would be lost")
	}
}

// Sells ladder too, and the same collapse applies within a direction.
func TestSellLadderCollapses(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()
	acted := 0
	for i := 0; i < 4; i++ {
		if tr.Observe(wallet, token, "sell", eth(100), now.Add(time.Duration(i)*time.Second)).Act {
			acted++
		}
	}
	if acted != 1 {
		t.Fatalf("acted on %d sell clips, want 1", acted)
	}
}

// A re-entry after the window is a new signal, not a continuation. The measured
// round trips were 97 and 142 seconds, well past the default window.
func TestReentryAfterWindowActsAgain(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()

	if v := tr.Observe(wallet, token, "buy", eth(0.03), now); !v.Act {
		t.Fatal("first entry did not act")
	}
	if v := tr.Observe(wallet, token, "buy", eth(0.03), now.Add(30*time.Second)); v.Act {
		t.Error("clip inside the window acted")
	}
	later := now.Add(DefaultWindow + time.Second)
	v := tr.Observe(wallet, token, "buy", eth(0.05), later)
	if !v.Act {
		t.Error("re-entry after the window was suppressed")
	}
	if v.Clip != 1 {
		t.Errorf("re-entry clip = %d, want 1 (a fresh ladder)", v.Clip)
	}
	if want := eth(0.05); v.LadderTotalWei.Cmp(want) != 0 {
		t.Errorf("re-entry total = %s, want %s (not carried over)", v.LadderTotalWei, want)
	}
}

// Different tokens and different wallets are independent signals.
func TestKeysAreIndependent(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()

	if v := tr.Observe(wallet, token, "buy", eth(0.03), now); !v.Act {
		t.Fatal("first token did not act")
	}
	if v := tr.Observe(wallet, other, "buy", eth(0.03), now); !v.Act {
		t.Error("a different token was suppressed")
	}
	stranger := common.HexToAddress("0x4d9644d05FE2123b4eAfa8d7fD31B0EA430726f3")
	if v := tr.Observe(stranger, token, "buy", eth(0.03), now); !v.Act {
		t.Error("a different wallet was suppressed")
	}
}

// A zero window disables consolidation, which is how the fee drag that would
// have been paid can be measured.
func TestZeroWindowDisables(t *testing.T) {
	tr := New(0)
	if tr.Enabled() {
		t.Error("zero window reported as enabled")
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if v := tr.Observe(wallet, token, "buy", eth(0.03), now); !v.Act {
			t.Fatalf("clip %d suppressed while disabled", i+1)
		}
	}
}

func TestNilAmountIsSafe(t *testing.T) {
	tr := New(DefaultWindow)
	v := tr.Observe(wallet, token, "buy", nil, time.Now())
	if !v.Act || v.LadderTotalWei == nil || v.LadderTotalWei.Sign() != 0 {
		t.Errorf("nil amount mishandled: act=%v total=%v", v.Act, v.LadderTotalWei)
	}
}

// The suppressed clip must explain itself; a signal that vanishes without a
// reason is indistinguishable from a bug.
func TestVerdictReasonIsInformative(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()
	tr.Observe(wallet, token, "buy", eth(0.03), now)
	v := tr.Observe(wallet, token, "buy", eth(0.03), now.Add(1200*time.Millisecond))

	got := v.Reason()
	for _, want := range []string{"clip 2", "suppressed", "1.2s"} {
		if !contains(got, want) {
			t.Errorf("reason %q missing %q", got, want)
		}
	}
	// The filter check is already named "ladder"; repeating it renders as
	// "ladder: ladder: clip 2 …".
	if contains(got, "ladder") {
		t.Errorf("reason %q duplicates the check name", got)
	}
}

// The tracker sees every watched swap for the process lifetime, so entries must
// not accumulate without bound.
func TestPruneBoundsMemory(t *testing.T) {
	tr := New(time.Second)
	base := time.Now()

	for i := 0; i < 500; i++ {
		tok := common.BigToAddress(big.NewInt(int64(i)))
		tr.Observe(wallet, tok, "buy", eth(0.01), base)
	}
	// Past both the window and the prune interval.
	tr.Observe(wallet, token, "buy", eth(0.01), base.Add(pruneEvery+2*time.Second))

	tr.mu.Lock()
	n := len(tr.seen)
	tr.mu.Unlock()
	if n > 10 {
		t.Errorf("retained %d entries after prune, want the expired ones dropped", n)
	}
}

func TestConcurrentObserve(t *testing.T) {
	tr := New(DefaultWindow)
	now := time.Now()
	var wg sync.WaitGroup
	acted := make(chan bool, 200)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				acted <- tr.Observe(wallet, token, "buy", eth(0.03), now).Act
			}
		}()
	}
	wg.Wait()
	close(acted)

	// Exactly one goroutine may open the ladder, however they interleave.
	n := 0
	for a := range acted {
		if a {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d clips acted under concurrency, want exactly 1", n)
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
