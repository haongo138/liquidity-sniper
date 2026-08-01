package exec

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/ethereum/go-ethereum/common"
)

var token = common.HexToAddress("0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64")

// The single most important safety property: our slippage floor is never zero.
// The copied wallets send amountOutMinimum=0 and rely on speed; we land behind
// them by design, so an unprotected order hands a free option to anyone in
// between.
func TestMinOutIsNeverZero(t *testing.T) {
	cases := []struct {
		name        string
		slippageBps int64
		expectedOut *big.Int
	}{
		{"normal", 500, big.NewInt(1_000_000)},
		{"zero slippage config", 0, big.NewInt(1_000_000)},
		{"negative slippage config", -100, big.NewInt(1_000_000)},
		{"absurd slippage config", 99999, big.NewInt(1_000_000)},
		{"nil expected", 500, nil},
		{"zero expected", 500, big.NewInt(0)},
		{"tiny expected", 500, big.NewInt(1)},
	}
	for _, tc := range cases {
		e := &Executor{cfg: Config{SlippageBps: tc.slippageBps}}
		got := e.minOut(tc.expectedOut)
		if got == nil || got.Sign() <= 0 {
			t.Errorf("%s: minOut = %v, must always be positive", tc.name, got)
		}
	}
}

func TestMinOutAppliesSlippage(t *testing.T) {
	e := &Executor{cfg: Config{SlippageBps: 500}} // 5%
	got := e.minOut(big.NewInt(1000))
	if got.Int64() != 950 {
		t.Errorf("minOut = %s, want 950 (1000 less 5%%)", got)
	}
}

// A bad config must fall back to a protective default, never to no protection.
func TestMinOutFallsBackToProtection(t *testing.T) {
	e := &Executor{cfg: Config{SlippageBps: 0}}
	got := e.minOut(big.NewInt(10000))
	if got.Int64() != 9500 {
		t.Errorf("minOut = %s, want 9500 (the 5%% default)", got)
	}
}

// The calldata must match what the decoder reads back, or we would be sending
// something other than what we believe.
func TestEncodeExactInputSingleRoundTrips(t *testing.T) {
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")
	data := encodeExactInputSingle(chain.WETH, token, 10000, recipient,
		big.NewInt(3e14), big.NewInt(12345))

	if !strings.HasPrefix(data, "0x"+selExactInputSingle) {
		t.Fatalf("wrong selector: %s", data[:12])
	}
	body := data[10:]
	if len(body) != 7*64 {
		t.Fatalf("body = %d chars, want %d (7 static words)", len(body), 7*64)
	}
	word := func(i int) string { return body[i*64 : (i+1)*64] }
	mustAddr := func(i int, want common.Address, name string) {
		if got := common.HexToAddress(word(i)); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	mustAddr(0, chain.WETH, "tokenIn")
	mustAddr(1, token, "tokenOut")
	mustAddr(3, recipient, "recipient")

	mustInt := func(i int, want int64, name string) {
		v, _ := new(big.Int).SetString(word(i), 16)
		if v == nil || v.Int64() != want {
			t.Errorf("%s = %v, want %d", name, v, want)
		}
	}
	mustInt(2, 10000, "fee")
	mustInt(4, 3e14, "amountIn")
	mustInt(5, 12345, "amountOutMinimum")
	mustInt(6, 0, "sqrtPriceLimitX96")
}

// V4 depth is measured but V4 encoding is not implemented. Routing a V4 token
// through SwapRouter02 would revert at best and mis-fill at worst, so it must
// refuse rather than try.
func TestV4IsRefused(t *testing.T) {
	e := New(nil, nil, Config{})
	pool := &chain.Pool{Venue: "uniswap-v4", FeeTier: 10000}
	if _, err := e.swap(context.TODO(), pool, chain.WETH, token, big.NewInt(1), nil); err == nil {
		t.Fatal("a v4 pool was accepted for execution")
	}
}

func TestNoPoolIsRefused(t *testing.T) {
	e := New(nil, nil, Config{})
	if _, err := e.swap(context.TODO(), nil, chain.WETH, token, big.NewInt(1), nil); err == nil {
		t.Fatal("a nil pool was accepted")
	}
}

func TestZeroAmountIsRefused(t *testing.T) {
	e := New(nil, nil, Config{})
	pool := &chain.Pool{Venue: "uniswap-v3", FeeTier: 3000}
	for _, amt := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		if _, err := e.swap(context.TODO(), pool, chain.WETH, token, amt, nil); err == nil {
			t.Errorf("amount %v was accepted", amt)
		}
	}
}

// The per-trade ceiling exists to bound a bug, so it must actually bind.
func TestMaxTradeCeilingBinds(t *testing.T) {
	e := New(nil, nil, Config{MaxTradeWei: big.NewInt(1e15)})
	pool := &chain.Pool{Venue: "uniswap-v3", FeeTier: 3000}
	if _, err := e.swap(context.TODO(), pool, chain.WETH, token, big.NewInt(2e15), nil); err == nil {
		t.Fatal("a trade above max_trade_eth was accepted")
	}
}

// The kill switch must stop trading, and stay stopped.
func TestDailyLossKillSwitch(t *testing.T) {
	e := New(nil, nil, Config{DailyLossLimitWei: big.NewInt(1000)})
	if halted, _ := e.Halted(); halted {
		t.Fatal("halted before any loss")
	}
	e.RecordPnL(big.NewInt(-400))
	if halted, _ := e.Halted(); halted {
		t.Fatal("halted below the limit")
	}
	e.RecordPnL(big.NewInt(-700)) // cumulative 1100 > 1000
	halted, why := e.Halted()
	if !halted {
		t.Fatal("did not halt after breaching the daily limit")
	}
	if why == "" {
		t.Error("halt reason is empty")
	}
	// A later profit must not silently re-arm: resuming after a loss streak
	// should be a human decision.
	e.RecordPnL(big.NewInt(100_000))
	if halted, _ := e.Halted(); !halted {
		t.Error("a profit re-armed execution after the kill switch tripped")
	}
	// And no trade may pass while halted.
	pool := &chain.Pool{Venue: "uniswap-v3", FeeTier: 3000}
	if _, err := e.swap(context.TODO(), pool, chain.WETH, token, big.NewInt(1), nil); err == nil {
		t.Error("a trade executed while halted")
	}
}

func TestProfitDoesNotTripKillSwitch(t *testing.T) {
	e := New(nil, nil, Config{DailyLossLimitWei: big.NewInt(1000)})
	for i := 0; i < 10; i++ {
		e.RecordPnL(big.NewInt(5000))
	}
	if halted, _ := e.Halted(); halted {
		t.Error("profits tripped the loss limit")
	}
}
