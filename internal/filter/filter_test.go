package filter

import (
	"math/big"
	"testing"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/decode"
	"github.com/ethereum/go-ethereum/common"
)

var token = common.HexToAddress("0x496d19Ed7942858790469B963B1763a5fda93EA0")

func buy(amountInWei int64) decode.SwapIntent {
	return decode.SwapIntent{
		Direction: decode.DirectionBuy, TokenIn: chain.WETH, TokenOut: token,
		AmountIn: big.NewInt(amountInWei), AmountOutMin: big.NewInt(0),
	}
}

func sell(amountInTokens *big.Int) decode.SwapIntent {
	return decode.SwapIntent{
		Direction: decode.DirectionSell, TokenIn: token, TokenOut: chain.WETH,
		AmountIn: amountInTokens, AmountOutMin: big.NewInt(0),
	}
}

func find(d Decision, name string) (Check, bool) {
	for _, c := range d.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestBlocklistRejects(t *testing.T) {
	cfg := Config{Blocklist: map[common.Address]bool{token: true}}
	if d := Tier0(cfg, buy(1e15)); d.Approved {
		t.Fatal("blocklisted token was approved")
	}
}

func TestAllowlistExcludesEverythingElse(t *testing.T) {
	other := common.HexToAddress("0x1111111111111111111111111111111111111111")
	cfg := Config{Allowlist: map[common.Address]bool{other: true}}
	if d := Tier0(cfg, buy(1e15)); d.Approved {
		t.Fatal("token outside the allowlist was approved")
	}
}

func TestSellsRejectedUnlessEnabled(t *testing.T) {
	if d := Tier0(Config{}, sell(big.NewInt(1e18))); d.Approved {
		t.Fatal("sell approved while allow_sells is off")
	}
	if d := Tier0(Config{AllowSells: true}, sell(big.NewInt(1e18))); !d.Approved {
		t.Fatalf("sell rejected while allow_sells is on: %s", d.Summary())
	}
}

func TestMinTradeGatesBuys(t *testing.T) {
	cfg := Config{MinTradeWei: big.NewInt(1e15)}
	if d := Tier0(cfg, buy(1e14)); d.Approved {
		t.Fatal("dust buy was approved")
	}
	if d := Tier0(cfg, buy(2e15)); !d.Approved {
		t.Fatalf("sized buy was rejected: %s", d.Summary())
	}
}

// A sell's AmountIn is a token amount, so an ETH-denominated floor cannot be
// applied to it. It must report n/a rather than compare unrelated units.
func TestMinTradeNotAppliedToSells(t *testing.T) {
	cfg := Config{AllowSells: true, MinTradeWei: big.NewInt(1e15)}
	// A huge token amount that would trivially clear an ETH floor by accident,
	// and a tiny one that would trivially fail it.
	for _, amt := range []*big.Int{
		new(big.Int).SetUint64(2656406438815130185),
		big.NewInt(1),
	} {
		d := Tier0(cfg, sell(amt))
		c, ok := find(d, "min_trade")
		if !ok {
			t.Fatal("min_trade check missing")
		}
		if c.Verdict != NotApplicable {
			t.Errorf("verdict = %s, want n/a for a sell (amount %s)", c.Verdict, amt)
		}
		if !d.Approved {
			t.Errorf("sell rejected by an inapplicable check: %s", d.Summary())
		}
	}
}

func TestMinLiquidityGates(t *testing.T) {
	cfg := Config{MinLiquidityWei: big.NewInt(1e18)}
	state := chain.TokenState{Token: token, Pool: &chain.Pool{
		Address: common.HexToAddress("0xa381b5329e1adeda99b9caae340c9b1efd942f36"),
		Venue:   "uniswap-v3", FeeTier: 10000, WETHLiquidity: big.NewInt(4e17),
	}}
	if d := Tier1(cfg, state); d.Approved {
		t.Fatal("thin pool was approved")
	}
	state.Pool.WETHLiquidity = big.NewInt(4e18)
	if d := Tier1(cfg, state); !d.Approved {
		t.Fatalf("deep pool was rejected: %s", d.Summary())
	}
}

func TestNoPoolRejects(t *testing.T) {
	if d := Tier1(Config{}, chain.TokenState{Token: token}); d.Approved {
		t.Fatal("token with no pool was approved")
	}
}

// V3 has no fungible LP token. The check must report n/a, never a false pass.
func TestLPSecuredIsNotApplicableOnV3(t *testing.T) {
	cfg := Config{RequireLPSecured: true, MinLPBurnedPct: 90}
	state := chain.TokenState{Token: token, Pool: &chain.Pool{
		Venue: "uniswap-v3", FeeTier: 10000, WETHLiquidity: big.NewInt(4e18),
	}}
	d := Tier1(cfg, state)
	c, ok := find(d, "lp_secured")
	if !ok {
		t.Fatal("lp_secured check missing")
	}
	if c.Verdict != NotApplicable {
		t.Errorf("verdict = %s, want n/a on v3", c.Verdict)
	}
	if !d.Approved {
		t.Errorf("v3 token blocked by an inapplicable check: %s", d.Summary())
	}
}

func TestLPSecuredEvaluatedOnV2(t *testing.T) {
	cfg := Config{RequireLPSecured: true, MinLPBurnedPct: 90}
	state := chain.TokenState{
		Token:         token,
		Pool:          &chain.Pool{Venue: "uniswap-v2", WETHLiquidity: big.NewInt(4e18)},
		LPTotalSupply: big.NewInt(1000),
		LPBurned:      big.NewInt(500), // 50%, below the 90% floor
	}
	if d := Tier1(cfg, state); d.Approved {
		t.Fatal("half-burnt LP was approved against a 90% floor")
	}
	state.LPBurned = big.NewInt(950)
	if d := Tier1(cfg, state); !d.Approved {
		t.Fatalf("95%% burnt LP was rejected: %s", d.Summary())
	}
}

func TestRenouncedOwnership(t *testing.T) {
	cfg := Config{RequireRenounced: true}
	pool := &chain.Pool{Venue: "uniswap-v3", WETHLiquidity: big.NewInt(4e18)}
	owned := chain.TokenState{Token: token, Pool: pool,
		Owner: common.HexToAddress("0x2222222222222222222222222222222222222222")}
	if d := Tier1(cfg, owned); d.Approved {
		t.Fatal("token with a live owner was approved")
	}
	if d := Tier1(cfg, chain.TokenState{Token: token, Pool: pool}); !d.Approved {
		t.Fatalf("renounced token was rejected: %s", d.Summary())
	}
}

// Merge must preserve a rejection from either side.
func TestMergeKeepsRejections(t *testing.T) {
	pass := Decision{Approved: true, Checks: []Check{{"a", Pass, ""}}}
	fail := Decision{Approved: false, Checks: []Check{{"b", Reject, "nope"}}}
	if Merge(pass, fail).Approved {
		t.Fatal("merge approved despite a rejection")
	}
	if !Merge(pass, pass).Approved {
		t.Fatal("merge rejected two passes")
	}
}
