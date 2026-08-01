package chain

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestV4StateSlotIsDeterministic(t *testing.T) {
	id := common.HexToHash("0xc4eb92788729d4b97d3f07b41aafd230cc57158cce61ff85ad1d56f1ff2646c2")
	a, b := v4StateSlot(id), v4StateSlot(id)
	if a.Cmp(b) != 0 {
		t.Fatal("slot derivation is not deterministic")
	}
	if a.Sign() == 0 {
		t.Fatal("slot derivation produced zero")
	}
	if other := v4StateSlot(common.HexToHash("0x01")); other.Cmp(a) == 0 {
		t.Fatal("different poolIds produced the same slot")
	}
}

// tickSpacing is an int24, so a negative tick must not decode as a huge
// positive number.
func TestToSignedHandlesTwosComplement(t *testing.T) {
	if got := toSigned(big.NewInt(60)); got.Int64() != 60 {
		t.Errorf("positive: got %s, want 60", got)
	}
	neg, _ := new(big.Int).SetString(
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffc4", 16)
	if got := toSigned(neg); got.Int64() != -60 {
		t.Errorf("negative: got %s, want -60", got)
	}
}

func TestParseInitializeData(t *testing.T) {
	// fee=10000, tickSpacing=200, hooks=0x00…00
	data := "0x" +
		"0000000000000000000000000000000000000000000000000000000000002710" +
		"00000000000000000000000000000000000000000000000000000000000000c8" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000002"
	fee, tick, hooks, ok := parseInitializeData(data)
	if !ok {
		t.Fatal("parse failed")
	}
	if fee != 10000 {
		t.Errorf("fee = %d, want 10000", fee)
	}
	if tick != 200 {
		t.Errorf("tickSpacing = %d, want 200", tick)
	}
	if hooks != (common.Address{}) {
		t.Errorf("hooks = %s, want zero", hooks)
	}
	if _, _, _, ok := parseInitializeData("0x1234"); ok {
		t.Error("truncated data was accepted")
	}
}

// PairedAsset decides which side of the price formula to use, so getting it
// wrong inverts the depth figure.
func TestPairedAsset(t *testing.T) {
	tok := common.HexToAddress("0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9")

	native := V4Pool{Currency0: NativeETH, Currency1: tok}
	if a, first := native.PairedAsset(); a != NativeETH || !first {
		t.Errorf("native ETH: got %s first=%v, want zero address first=true", a, first)
	}
	weth := V4Pool{Currency0: tok, Currency1: WETH}
	if a, first := weth.PairedAsset(); a != WETH || first {
		t.Errorf("WETH second: got %s first=%v, want WETH first=false", a, first)
	}
}

// TestV4Live hits mainnet. It is the regression test for the bug this file
// exists to fix: token 0xaF3D76f1… was rejected fifteen times at a fixed
// 0.2200 ETH read from a dormant V3 dust pool, while its real liquidity sat in
// the V4 singleton.
//
//	V4_LIVE=1 go test ./internal/chain -run TestV4Live -v
func TestV4Live(t *testing.T) {
	if os.Getenv("V4_LIVE") == "" {
		t.Skip("set V4_LIVE=1 to run against mainnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := Dial(ctx, "https://rpc.mainnet.chain.robinhood.com")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	token := common.HexToAddress("0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9")

	pools, err := c.findV4Pools(ctx, token)
	if err != nil {
		t.Fatalf("findV4Pools: %v", err)
	}
	t.Logf("V4 pools found: %d", len(pools))
	for _, p := range pools {
		depth, err := c.v4Depth(ctx, p)
		if err != nil {
			t.Logf("  %s depth error: %v", p.ID.Hex()[:18], err)
			continue
		}
		eth := new(big.Float).Quo(new(big.Float).SetInt(depth), big.NewFloat(1e18))
		t.Logf("  pool %s fee=%d tick=%d hooks=%s depth=%s ETH",
			p.ID.Hex()[:18], p.Fee, p.TickSpacing, p.Hooks.Hex()[:10], eth.Text('f', 4))
	}
	if len(pools) == 0 {
		t.Fatal("no V4 pools found — discovery is still blind")
	}

	state, err := c.FetchState(ctx, NewCache(), token)
	if err != nil {
		t.Fatalf("FetchState: %v", err)
	}
	if state.Pool == nil {
		t.Fatal("FetchState found no pool at all")
	}
	eth := new(big.Float).Quo(
		new(big.Float).SetInt(state.Pool.WETHLiquidity), big.NewFloat(1e18))
	t.Logf("chosen pool: venue=%s fee=%d depth=%s ETH",
		state.Pool.Venue, state.Pool.FeeTier, eth.Text('f', 4))

	// The whole point: the deepest pool must no longer be the 0.22 ETH V3 dust.
	dust := big.NewInt(3e17) // 0.3 ETH
	if state.Pool.WETHLiquidity.Cmp(dust) < 0 {
		t.Errorf("still selecting a dust pool (%s ETH) — V4 depth is not winning",
			eth.Text('f', 4))
	}
	if state.Pool.Venue != "uniswap-v4" {
		t.Errorf("venue = %s, want uniswap-v4", state.Pool.Venue)
	}
}

// A token whose only liquidity is in V4 has no V2 pair and no V3 pool. Pool
// discovery used to return early in that case, which made precisely the tokens
// this code exists for invisible.
//
//	V4_LIVE=1 go test ./internal/chain -run TestV4OnlyToken -v
func TestV4OnlyToken(t *testing.T) {
	if os.Getenv("V4_LIVE") == "" {
		t.Skip("set V4_LIVE=1 to run against mainnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := Dial(ctx, "https://rpc.mainnet.chain.robinhood.com")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Verified to trade actively in V4 against native ETH, with no V2 pair and
	// no V3 pool at any fee tier. Under the old code these returned no pool at
	// all and were rejected without ever being measured.
	tokens := []common.Address{
		common.HexToAddress("0x36ce7d85b588518a7e01a0678f61f8747e8657ea"),
		common.HexToAddress("0x98e88f6b901f2939aacde7745d027518d7f880cc"),
		common.HexToAddress("0x3cc1251041c66ebc0d9aa1f422de43b0c06b8e8a"),
	}

	found := 0
	for _, token := range tokens {
		state, err := c.FetchState(ctx, NewCache(), token)
		if err != nil {
			t.Logf("%s: FetchState error: %v", token.Hex()[:12], err)
			continue
		}
		if state.Pool == nil {
			t.Errorf("%s: no pool found — discovery returns early again", token.Hex()[:12])
			continue
		}
		eth := new(big.Float).Quo(
			new(big.Float).SetInt(state.Pool.WETHLiquidity), big.NewFloat(1e18))
		t.Logf("%s: venue=%s fee=%d depth=%s ETH", token.Hex()[:12],
			state.Pool.Venue, state.Pool.FeeTier, eth.Text('f', 6))
		if state.Pool.Venue != "uniswap-v4" {
			t.Errorf("%s: venue = %s, want uniswap-v4", token.Hex()[:12], state.Pool.Venue)
		}
		if state.Pool.WETHLiquidity.Sign() > 0 {
			found++
		}
	}
	if found == 0 {
		t.Fatal("no V4-only token reported any depth")
	}
}
