package position

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	tok = common.HexToAddress("0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64")
	kol = common.HexToAddress("0x85b605b47a5323912615cb8Af834BB1c4716b794")
)

func wei(f float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil)
	return v
}

func open(b *Book, cost float64, at time.Time) Position {
	p := Position{Token: tok, KOL: kol, CostWei: wei(cost),
		Tokens: big.NewInt(1_000_000), OpenedAt: at}
	b.Open(p)
	got, _ := b.Get(tok)
	return got
}

func TestTakeProfitFires(t *testing.T) {
	b := NewBook(Rules{TakeProfitPct: 50})
	now := time.Now()
	b.Open(Position{Token: tok, CostWei: big.NewInt(100),
		Tokens: big.NewInt(10), OpenedAt: now})
	p, _ := b.Get(tok)

	if _, ok := b.Check(p, big.NewInt(140), now); ok {
		t.Error("+40% triggered a +50% take profit")
	}
	ex, ok := b.Check(p, big.NewInt(150), now)
	if !ok {
		t.Fatal("+50% did not trigger take profit")
	}
	if ex.Reason != ReasonTakeProfit {
		t.Errorf("reason = %s, want %s", ex.Reason, ReasonTakeProfit)
	}
	if ex.PnLPct < 49 || ex.PnLPct > 51 {
		t.Errorf("pnl = %.1f%%, want ~50%%", ex.PnLPct)
	}
}

func TestStopLossFires(t *testing.T) {
	b := NewBook(Rules{StopLossPct: 30})
	now := time.Now()
	// Exact integers, not floats: 0.07 is inexact in binary and lands at
	// -29.999...%, which is a test artefact rather than a trigger bug.
	b.Open(Position{Token: tok, CostWei: big.NewInt(1000),
		Tokens: big.NewInt(100), OpenedAt: now})
	p, _ := b.Get(tok)

	if _, ok := b.Check(p, big.NewInt(750), now); ok {
		t.Error("-25% triggered a -30% stop loss")
	}
	ex, ok := b.Check(p, big.NewInt(700), now)
	if !ok {
		t.Fatal("-30% did not trigger stop loss")
	}
	if ex.Reason != ReasonStopLoss {
		t.Errorf("reason = %s, want %s", ex.Reason, ReasonStopLoss)
	}
	if ex.PnLPct > -29.9 || ex.PnLPct < -30.1 {
		t.Errorf("pnl = %.2f%%, want -30%%", ex.PnLPct)
	}
}

// Max hold must fire even when the position cannot be valued — that is exactly
// the case where the other triggers cannot help, and the one where a position
// would otherwise be held forever.
func TestMaxHoldFiresWithoutAValuation(t *testing.T) {
	b := NewBook(Rules{MaxHold: 10 * time.Minute, TakeProfitPct: 50, StopLossPct: 30})
	now := time.Now()
	p := open(b, 0.1, now.Add(-11*time.Minute))

	ex, ok := b.Check(p, nil, now)
	if !ok {
		t.Fatal("max hold did not fire on an unquotable position")
	}
	if ex.Reason != ReasonMaxHold {
		t.Errorf("reason = %s, want %s", ex.Reason, ReasonMaxHold)
	}
}

// Without a valuation and within the hold window there is nothing to decide, so
// no trigger may fire on a guess.
func TestNoValuationNoPriceTrigger(t *testing.T) {
	b := NewBook(DefaultRules())
	now := time.Now()
	p := open(b, 0.1, now)
	if _, ok := b.Check(p, nil, now); ok {
		t.Error("a trigger fired with no valuation")
	}
}

func TestKOLSellMirrored(t *testing.T) {
	b := NewBook(Rules{FollowKOLSell: true})
	open(b, 0.1, time.Now())

	ex, ok := b.OnKOLSell(tok)
	if !ok {
		t.Fatal("the copied wallet's sell was not mirrored")
	}
	if ex.Reason != ReasonKOLSold {
		t.Errorf("reason = %s, want %s", ex.Reason, ReasonKOLSold)
	}
	// A token we do not hold must not produce an exit.
	if _, ok := b.OnKOLSell(common.HexToAddress("0xdead")); ok {
		t.Error("an unheld token produced an exit")
	}
}

func TestKOLSellIgnoredWhenDisabled(t *testing.T) {
	b := NewBook(Rules{FollowKOLSell: false})
	open(b, 0.1, time.Now())
	if _, ok := b.OnKOLSell(tok); ok {
		t.Error("mirrored a sell while follow_kol_sell is off")
	}
}

// A second buy folds into the existing position. Opening a parallel one would
// split the cost basis and leave two things to sell.
func TestSecondBuyFoldsIntoPosition(t *testing.T) {
	b := NewBook(DefaultRules())
	now := time.Now()
	b.Open(Position{Token: tok, CostWei: wei(0.1), Tokens: big.NewInt(100), OpenedAt: now})
	b.Open(Position{Token: tok, CostWei: wei(0.05), Tokens: big.NewInt(50), OpenedAt: now})

	p, ok := b.Get(tok)
	if !ok {
		t.Fatal("position missing")
	}
	if want := wei(0.15); p.CostWei.Cmp(want) != 0 {
		t.Errorf("cost = %s, want %s", p.CostWei, want)
	}
	if p.Tokens.Int64() != 150 {
		t.Errorf("tokens = %s, want 150", p.Tokens)
	}
	if n, _ := b.Stats(); n != 1 {
		t.Errorf("open positions = %d, want 1", n)
	}
}

// Open must copy its input: a caller mutating the struct afterwards must not
// silently rewrite our cost basis.
func TestOpenCopiesAmounts(t *testing.T) {
	b := NewBook(DefaultRules())
	cost := wei(0.1)
	b.Open(Position{Token: tok, CostWei: cost, Tokens: big.NewInt(100), OpenedAt: time.Now()})
	cost.SetInt64(1)

	p, _ := b.Get(tok)
	if p.CostWei.Cmp(wei(0.1)) != 0 {
		t.Errorf("cost basis was mutated by the caller: %s", p.CostWei)
	}
}

func TestCloseRecordsReason(t *testing.T) {
	b := NewBook(DefaultRules())
	open(b, 0.1, time.Now())
	b.Close(tok, ReasonTakeProfit)

	n, closed := b.Stats()
	if n != 0 {
		t.Errorf("open = %d, want 0", n)
	}
	if closed[ReasonTakeProfit] != 1 {
		t.Errorf("closed[take_profit] = %d, want 1", closed[ReasonTakeProfit])
	}
	// Closing twice must not double-count.
	b.Close(tok, ReasonTakeProfit)
	if _, closed := b.Stats(); closed[ReasonTakeProfit] != 1 {
		t.Errorf("closing an absent position counted again: %d", closed[ReasonTakeProfit])
	}
}

// Every trigger disabled means nothing ever closes. The config layer refuses
// this, but the book must not invent an exit either.
func TestAllTriggersDisabledNeverExits(t *testing.T) {
	b := NewBook(Rules{})
	now := time.Now()
	p := open(b, 0.1, now.Add(-24*time.Hour))
	if _, ok := b.Check(p, wei(10), now); ok {
		t.Error("an exit fired with every trigger disabled")
	}
	if _, ok := b.OnKOLSell(tok); ok {
		t.Error("a KOL sell was mirrored with the trigger disabled")
	}
}

func TestConcurrentBookUse(t *testing.T) {
	b := NewBook(DefaultRules())
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tk := common.BigToAddress(big.NewInt(int64(n)))
			for j := 0; j < 50; j++ {
				b.Open(Position{Token: tk, CostWei: wei(0.01),
					Tokens: big.NewInt(10), OpenedAt: now})
				_, _ = b.Get(tk)
				_ = b.Positions()
				b.Close(tk, ReasonMaxHold)
			}
		}(i)
	}
	wg.Wait()
	if n, _ := b.Stats(); n != 0 {
		t.Errorf("open = %d after balanced open/close, want 0", n)
	}
}
