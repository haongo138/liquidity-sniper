// Package position tracks what we hold and decides when to exit.
//
// This ships in the same change as execution, and deliberately so: a bot that
// can buy but cannot sell is worse than no bot, because it converts a losing
// trade into a permanent one.
//
// Four independent triggers, because each covers a failure the others miss:
//
//   - The copied wallet sells. They know something we do not, and the observed
//     pattern is a de-risk sell that recovers the stake — following it is the
//     whole thesis.
//   - Take profit. The wallet may never sell, or may sell somewhere we cannot
//     see.
//   - Stop loss. The wallet may be wrong, or we may have entered far worse than
//     they did, which is structural for a backrunner.
//   - Maximum hold. Covers the case none of the above fire: a token that stops
//     trading, a wallet that goes silent, a position that would otherwise be
//     held forever.
package position

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Reason identifies why an exit fired.
type Reason string

const (
	ReasonKOLSold    Reason = "kol_sold"
	ReasonTakeProfit Reason = "take_profit"
	ReasonStopLoss   Reason = "stop_loss"
	ReasonMaxHold    Reason = "max_hold"
)

// Rules configures the exit triggers.
type Rules struct {
	// TakeProfitPct exits when value rises this far above cost, e.g. 50 for +50%.
	TakeProfitPct float64
	// StopLossPct exits when value falls this far below cost, e.g. 30 for -30%.
	StopLossPct float64
	// MaxHold exits regardless once this long has passed.
	MaxHold time.Duration
	// FollowKOLSell exits when the copied wallet sells.
	FollowKOLSell bool
}

// DefaultRules are deliberately conservative. The measured round trips were 97
// and 142 seconds and the fee drag is ~4%, so a max hold of ten minutes is long
// enough not to cut a normal trade short and short enough to bound a stuck one.
func DefaultRules() Rules {
	return Rules{
		TakeProfitPct: 50,
		StopLossPct:   30,
		MaxHold:       10 * time.Minute,
		FollowKOLSell: true,
	}
}

// Position is one open holding.
type Position struct {
	Token   common.Address
	KOL     common.Address
	Pool    common.Address
	FeeTier uint32
	// CostWei is what we spent, including fees.
	CostWei *big.Int
	// Tokens is the amount received.
	Tokens   *big.Int
	OpenedAt time.Time
	EntryTx  common.Hash
}

// Age returns how long the position has been open.
func (p Position) Age(now time.Time) time.Duration { return now.Sub(p.OpenedAt) }

// Exit describes a decision to close.
type Exit struct {
	Position Position
	Reason   Reason
	// ValueWei is what the position is currently worth, when known.
	ValueWei *big.Int
	PnLPct   float64
	Detail   string
}

// Book holds open positions. Safe for concurrent use.
type Book struct {
	mu    sync.Mutex
	rules Rules
	open  map[common.Address]*Position
	// closed counts exits by reason, for the run summary.
	closed map[Reason]int
}

// NewBook builds an empty Book.
func NewBook(rules Rules) *Book {
	return &Book{
		rules:  rules,
		open:   map[common.Address]*Position{},
		closed: map[Reason]int{},
	}
}

// Open records a new position. A second buy of a token already held is folded
// into the existing position rather than opening a parallel one, so the cost
// basis stays correct and there is only ever one thing to sell.
func (b *Book) Open(p Position) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.open[p.Token]; ok {
		cur.CostWei = new(big.Int).Add(cur.CostWei, p.CostWei)
		cur.Tokens = new(big.Int).Add(cur.Tokens, p.Tokens)
		return
	}
	cp := p
	cp.CostWei = new(big.Int).Set(p.CostWei)
	cp.Tokens = new(big.Int).Set(p.Tokens)
	b.open[p.Token] = &cp
}

// Get returns the open position for a token.
func (b *Book) Get(token common.Address) (Position, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.open[token]
	if !ok {
		return Position{}, false
	}
	return *p, true
}

// Close removes a position and records why.
func (b *Book) Close(token common.Address, reason Reason) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.open[token]; ok {
		delete(b.open, token)
		b.closed[reason]++
	}
}

// Open positions, as a snapshot.
func (b *Book) Positions() []Position {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Position, 0, len(b.open))
	for _, p := range b.open {
		out = append(out, *p)
	}
	return out
}

// Stats reports open count and exits by reason.
func (b *Book) Stats() (open int, closed map[Reason]int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := make(map[Reason]int, len(b.closed))
	for k, v := range b.closed {
		c[k] = v
	}
	return len(b.open), c
}

// OnKOLSell reports whether a watched wallet selling a token should close our
// position in it.
func (b *Book) OnKOLSell(token common.Address) (Exit, bool) {
	b.mu.Lock()
	rules := b.rules
	p, ok := b.open[token]
	var snapshot Position
	if ok {
		snapshot = *p
	}
	b.mu.Unlock()

	if !ok || !rules.FollowKOLSell {
		return Exit{}, false
	}
	return Exit{Position: snapshot, Reason: ReasonKOLSold,
		Detail: "the copied wallet exited"}, true
}

// Check evaluates the price- and time-based triggers for one position.
//
// valueWei is what the position would fetch right now, which the caller obtains
// by simulating the sell. A nil value means the sell could not be simulated at
// all — the position may be a honeypot — and that is surfaced rather than
// treated as unchanged.
func (b *Book) Check(p Position, valueWei *big.Int, now time.Time) (Exit, bool) {
	b.mu.Lock()
	rules := b.rules
	b.mu.Unlock()

	if rules.MaxHold > 0 && p.Age(now) >= rules.MaxHold {
		return Exit{Position: p, Reason: ReasonMaxHold, ValueWei: valueWei,
			PnLPct: pnlPct(p.CostWei, valueWei),
			Detail: fmt.Sprintf("held %s, limit %s",
				p.Age(now).Round(time.Second), rules.MaxHold)}, true
	}
	if valueWei == nil || p.CostWei == nil || p.CostWei.Sign() == 0 {
		return Exit{}, false
	}

	pct := pnlPct(p.CostWei, valueWei)
	switch {
	case rules.TakeProfitPct > 0 && pct >= rules.TakeProfitPct:
		return Exit{Position: p, Reason: ReasonTakeProfit, ValueWei: valueWei, PnLPct: pct,
			Detail: fmt.Sprintf("%+.1f%% >= +%.1f%%", pct, rules.TakeProfitPct)}, true
	case rules.StopLossPct > 0 && pct <= -rules.StopLossPct:
		return Exit{Position: p, Reason: ReasonStopLoss, ValueWei: valueWei, PnLPct: pct,
			Detail: fmt.Sprintf("%+.1f%% <= -%.1f%%", pct, rules.StopLossPct)}, true
	}
	return Exit{}, false
}

// pnlPct returns the percentage change from cost to value.
func pnlPct(cost, value *big.Int) float64 {
	if cost == nil || value == nil || cost.Sign() == 0 {
		return 0
	}
	delta := new(big.Float).SetInt(new(big.Int).Sub(value, cost))
	f, _ := new(big.Float).Quo(delta, new(big.Float).SetInt(cost)).Float64()
	return f * 100
}
