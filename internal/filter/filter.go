// Package filter decides whether a detected swap is worth copying.
//
// Filters are pure functions over an immutable snapshot: each returns a new
// Decision and never mutates its input. Every verdict carries a reason, so a
// shadow-mode log is auditable after the fact rather than a bare yes/no.
//
// Tiering matters because the hot path is a race. Tier 0 is in-memory and free;
// Tier 1 costs the batched state read; Tier 2 is deliberately excluded from the
// entry decision and belongs to post-entry exit logic.
package filter

import (
	"fmt"
	"math/big"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/ethereum/go-ethereum/common"
)

// Verdict is the outcome of one filter.
type Verdict string

const (
	// Pass means the filter was evaluated and satisfied.
	Pass Verdict = "pass"
	// Reject means the filter was evaluated and failed.
	Reject Verdict = "reject"
	// NotApplicable means the check cannot be performed on this venue. It is
	// reported distinctly rather than silently passing — a V3 token has no LP
	// burn to check, and calling that a pass would be a lie.
	NotApplicable Verdict = "n/a"
)

// Check is one filter's result.
type Check struct {
	Name    string
	Verdict Verdict
	Reason  string
}

// Decision is the immutable outcome of the whole chain.
type Decision struct {
	Approved bool
	Checks   []Check
}

// Rejected returns the checks that failed.
func (d Decision) Rejected() []Check {
	var out []Check
	for _, c := range d.Checks {
		if c.Verdict == Reject {
			out = append(out, c)
		}
	}
	return out
}

// Summary renders a one-line reason, suitable for a log column.
func (d Decision) Summary() string {
	rej := d.Rejected()
	if len(rej) == 0 {
		return "approved"
	}
	return fmt.Sprintf("%s: %s", rej[0].Name, rej[0].Reason)
}

// Config holds the filter thresholds. Zero values disable a filter, so an empty
// config approves everything — always set these explicitly.
type Config struct {
	// MinLiquidityWei gates on the WETH side of the pool.
	MinLiquidityWei *big.Int
	// MaxLiquidityWei skips pools too deep for a copy trade to move.
	MaxLiquidityWei *big.Int
	// RequireLPSecured demands V2 LP tokens be burnt or locked.
	RequireLPSecured bool
	// MinLPBurnedPct is the share of LP supply that must be burnt.
	MinLPBurnedPct float64
	// RequireRenounced demands owner() be the zero address.
	RequireRenounced bool
	// MinTradeWei ignores dust trades from the watched wallet.
	MinTradeWei *big.Int
	// AllowSells copies exits as well as entries.
	AllowSells bool
	// Blocklist and Allowlist are checked at tier 0.
	Blocklist map[common.Address]bool
	Allowlist map[common.Address]bool
}

// Tier0 runs the free, in-memory checks. It rejects before any network call,
// so a blocklisted token costs nothing.
func Tier0(cfg Config, intent decode.SwapIntent) Decision {
	var checks []Check
	token := intent.Token()

	switch {
	case cfg.Blocklist[token]:
		checks = append(checks, Check{"blocklist", Reject, "token is blocklisted"})
	case len(cfg.Allowlist) > 0 && !cfg.Allowlist[token]:
		checks = append(checks, Check{"allowlist", Reject, "token not in allowlist"})
	default:
		checks = append(checks, Check{"listing", Pass, "not blocked"})
	}

	switch intent.Direction {
	case decode.DirectionBuy:
		checks = append(checks, Check{"direction", Pass, "buy"})
	case decode.DirectionSell:
		if cfg.AllowSells {
			checks = append(checks, Check{"direction", Pass, "sell (copying exits)"})
		} else {
			checks = append(checks, Check{"direction", Reject, "sell, and allow_sells is off"})
		}
	default:
		checks = append(checks, Check{"direction", Reject, "token-to-token swap, no WETH leg"})
	}

	// The threshold is denominated in ETH, so it can only be applied when the
	// input side actually is ETH. On a sell, AmountIn is a token amount with
	// the token's own decimals — comparing that against an ETH floor is
	// meaningless and would gate on a number that means nothing.
	if cfg.MinTradeWei != nil && cfg.MinTradeWei.Sign() > 0 {
		switch {
		case intent.Direction != decode.DirectionBuy:
			checks = append(checks, Check{"min_trade", NotApplicable,
				"input is denominated in tokens, not ETH"})
		case intent.AmountIn == nil || intent.AmountIn.Cmp(cfg.MinTradeWei) < 0:
			checks = append(checks, Check{"min_trade", Reject,
				fmt.Sprintf("input %s ETH below floor %s ETH",
					weiStr(intent.AmountIn), weiStr(cfg.MinTradeWei))})
		default:
			checks = append(checks, Check{"min_trade", Pass, weiStr(intent.AmountIn) + " ETH"})
		}
	}

	return finish(checks)
}

// Tier1 runs the checks that need the batched state read.
func Tier1(cfg Config, state chain.TokenState) Decision {
	var checks []Check

	if state.Pool == nil {
		return finish([]Check{{"pool", Reject, "no WETH pool found on any venue"}})
	}
	checks = append(checks, Check{"pool", Pass,
		fmt.Sprintf("%s fee=%d %s", state.Pool.Venue, state.Pool.FeeTier, state.Pool.Address.Hex())})

	liq := state.Pool.WETHLiquidity
	if cfg.MinLiquidityWei != nil && cfg.MinLiquidityWei.Sign() > 0 {
		if liq == nil || liq.Cmp(cfg.MinLiquidityWei) < 0 {
			checks = append(checks, Check{"min_liquidity", Reject,
				fmt.Sprintf("%s ETH below floor %s ETH", weiStr(liq), weiStr(cfg.MinLiquidityWei))})
		} else {
			checks = append(checks, Check{"min_liquidity", Pass, weiStr(liq) + " ETH"})
		}
	}
	if cfg.MaxLiquidityWei != nil && cfg.MaxLiquidityWei.Sign() > 0 && liq != nil {
		if liq.Cmp(cfg.MaxLiquidityWei) > 0 {
			checks = append(checks, Check{"max_liquidity", Reject,
				fmt.Sprintf("%s ETH above ceiling %s ETH", weiStr(liq), weiStr(cfg.MaxLiquidityWei))})
		} else {
			checks = append(checks, Check{"max_liquidity", Pass, weiStr(liq) + " ETH"})
		}
	}

	if cfg.RequireLPSecured {
		checks = append(checks, lpCheck(cfg, state))
	}

	if cfg.RequireRenounced {
		if state.Owner == (common.Address{}) {
			checks = append(checks, Check{"renounced", Pass, "ownership renounced"})
		} else {
			checks = append(checks, Check{"renounced", Reject, "owner is " + state.Owner.Hex()})
		}
	}

	return finish(checks)
}

// lpCheck evaluates LP burn. On V3 there is no fungible LP token, so this
// reports NotApplicable rather than passing a check it never ran.
func lpCheck(cfg Config, state chain.TokenState) Check {
	pct, ok := state.LPSecuredPct()
	if !ok {
		return Check{"lp_secured", NotApplicable,
			fmt.Sprintf("%s has no fungible LP token; liquidity is held as position NFTs", state.Pool.Venue)}
	}
	if pct < cfg.MinLPBurnedPct {
		return Check{"lp_secured", Reject,
			fmt.Sprintf("only %.1f%% of LP burnt, need %.1f%%", pct, cfg.MinLPBurnedPct)}
	}
	return Check{"lp_secured", Pass, fmt.Sprintf("%.1f%% of LP burnt", pct)}
}

// finish approves only when nothing rejected. NotApplicable does not block:
// the check could not run, which is not the same as failing.
func finish(checks []Check) Decision {
	for _, c := range checks {
		if c.Verdict == Reject {
			return Decision{Approved: false, Checks: checks}
		}
	}
	return Decision{Approved: true, Checks: checks}
}

// Merge combines two decisions, preserving check order.
func Merge(a, b Decision) Decision {
	checks := make([]Check, 0, len(a.Checks)+len(b.Checks))
	checks = append(checks, a.Checks...)
	checks = append(checks, b.Checks...)
	return finish(checks)
}

// weiStr renders wei as ETH with four decimals.
func weiStr(v *big.Int) string {
	if v == nil {
		return "0"
	}
	f := new(big.Float).Quo(new(big.Float).SetInt(v), big.NewFloat(1e18))
	return f.Text('f', 4)
}
