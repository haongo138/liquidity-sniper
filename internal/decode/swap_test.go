package decode

import (
	"math/big"
	"strings"
	"testing"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// The decoder is hand-rolled for the hot path, so these tests build calldata
// with go-ethereum's real ABI encoder. That cross-validates the offset maths
// against an independent implementation rather than against my own assumptions.

const routerABI = `[
 {"name":"exactInputSingle","type":"function","inputs":[{"name":"params","type":"tuple","components":[
   {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"fee","type":"uint24"},
   {"name":"recipient","type":"address"},{"name":"amountIn","type":"uint256"},
   {"name":"amountOutMinimum","type":"uint256"},{"name":"sqrtPriceLimitX96","type":"uint160"}]}]},
 {"name":"exactInput","type":"function","inputs":[{"name":"params","type":"tuple","components":[
   {"name":"path","type":"bytes"},{"name":"recipient","type":"address"},
   {"name":"amountIn","type":"uint256"},{"name":"amountOutMinimum","type":"uint256"}]}]},
 {"name":"swapExactTokensForTokens","type":"function","inputs":[
   {"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},
   {"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}]},
 {"name":"swapExactETHForTokens","type":"function","inputs":[
   {"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},
   {"name":"to","type":"address"},{"name":"deadline","type":"uint256"}]},
 {"name":"multicall","type":"function","inputs":[{"name":"data","type":"bytes[]"}]}
]`

var (
	tokenA    = common.HexToAddress("0x496d19Ed7942858790469B963B1763a5fda93EA0")
	recipient = common.HexToAddress("0x81BA5528569Fd4490483615c1fe93c6F50cC8D1F")
)

type exactInputSingleParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Fee               *big.Int
	Recipient         common.Address
	AmountIn          *big.Int
	AmountOutMinimum  *big.Int
	SqrtPriceLimitX96 *big.Int
}

type exactInputParams struct {
	Path             []byte
	Recipient        common.Address
	AmountIn         *big.Int
	AmountOutMinimum *big.Int
}

func mustABI(t *testing.T) abi.ABI {
	t.Helper()
	a, err := abi.JSON(strings.NewReader(routerABI))
	if err != nil {
		t.Fatalf("parsing abi: %v", err)
	}
	return a
}

func pack(t *testing.T, name string, args ...any) []byte {
	t.Helper()
	data, err := mustABI(t).Pack(name, args...)
	if err != nil {
		t.Fatalf("packing %s: %v", name, err)
	}
	return data
}

// txTo builds a transaction to a router carrying data and value.
func txTo(to common.Address, data []byte, value *big.Int) *types.Transaction {
	return types.NewTx(&types.DynamicFeeTx{
		ChainID: big.NewInt(chain.MainnetChainID), Nonce: 1, To: &to,
		Value: value, Gas: 300000, GasFeeCap: big.NewInt(1e9), GasTipCap: big.NewInt(1),
		Data: data,
	})
}

func TestExactInputSingleBuy(t *testing.T) {
	data := pack(t, "exactInputSingle", exactInputSingleParams{
		TokenIn: chain.WETH, TokenOut: tokenA, Fee: big.NewInt(10000),
		Recipient: recipient, AmountIn: big.NewInt(3e14),
		AmountOutMinimum: big.NewInt(12345), SqrtPriceLimitX96: big.NewInt(0),
	})

	got, err := Swap(txTo(chain.V3Router, data, big.NewInt(0)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Direction != DirectionBuy {
		t.Errorf("direction = %s, want buy", got.Direction)
	}
	if got.Venue != VenueV3 {
		t.Errorf("venue = %s, want %s", got.Venue, VenueV3)
	}
	if got.Token() != tokenA {
		t.Errorf("token = %s, want %s", got.Token(), tokenA)
	}
	if got.AmountIn.Int64() != 3e14 {
		t.Errorf("amountIn = %s, want 300000000000000", got.AmountIn)
	}
	if got.FeeTier != 10000 {
		t.Errorf("feeTier = %d, want 10000", got.FeeTier)
	}
}

// A native-ETH swap leaves amountIn zero and carries the value on the tx.
func TestExactInputSingleUsesTxValueWhenAmountInZero(t *testing.T) {
	data := pack(t, "exactInputSingle", exactInputSingleParams{
		TokenIn: chain.WETH, TokenOut: tokenA, Fee: big.NewInt(3000),
		Recipient: recipient, AmountIn: big.NewInt(0),
		AmountOutMinimum: big.NewInt(1), SqrtPriceLimitX96: big.NewInt(0),
	})
	got, err := Swap(txTo(chain.V3Router, data, big.NewInt(5e15)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.AmountIn.Int64() != 5e15 {
		t.Errorf("amountIn = %s, want tx value 5000000000000000", got.AmountIn)
	}
}

func TestExactInputSingleSell(t *testing.T) {
	data := pack(t, "exactInputSingle", exactInputSingleParams{
		TokenIn: tokenA, TokenOut: chain.WETH, Fee: big.NewInt(10000),
		Recipient: recipient, AmountIn: big.NewInt(999),
		AmountOutMinimum: big.NewInt(1), SqrtPriceLimitX96: big.NewInt(0),
	})
	got, err := Swap(txTo(chain.V3Router, data, big.NewInt(0)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Direction != DirectionSell {
		t.Errorf("direction = %s, want sell", got.Direction)
	}
	if got.Token() != tokenA {
		t.Errorf("token = %s, want %s", got.Token(), tokenA)
	}
}

// exactInput carries a packed multi-hop path: token(20) fee(3) token(20)...
func TestExactInputPackedPath(t *testing.T) {
	mid := common.HexToAddress("0xa019709F16971b8fC4b04537FB6D7c59FeC012fb")
	var path []byte
	path = append(path, chain.WETH.Bytes()...)
	path = append(path, 0x00, 0x0b, 0xb8) // 3000
	path = append(path, mid.Bytes()...)
	path = append(path, 0x00, 0x27, 0x10) // 10000
	path = append(path, tokenA.Bytes()...)

	data := pack(t, "exactInput", exactInputParams{
		Path: path, Recipient: recipient,
		AmountIn: big.NewInt(7e14), AmountOutMinimum: big.NewInt(42),
	})
	got, err := Swap(txTo(chain.V3Router, data, big.NewInt(0)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if len(got.Path) != 3 {
		t.Fatalf("path len = %d, want 3", len(got.Path))
	}
	if got.Path[0] != chain.WETH || got.Path[2] != tokenA {
		t.Errorf("path = %v, want WETH..tokenA", got.Path)
	}
	if got.FeeTier != 3000 {
		t.Errorf("feeTier = %d, want first hop 3000", got.FeeTier)
	}
	if got.Direction != DirectionBuy {
		t.Errorf("direction = %s, want buy", got.Direction)
	}
}

func TestV2SwapExactTokensForTokensSell(t *testing.T) {
	data := pack(t, "swapExactTokensForTokens",
		big.NewInt(1e18), big.NewInt(5), []common.Address{tokenA, chain.WETH},
		recipient, big.NewInt(1800000000))

	got, err := Swap(txTo(chain.V2Router, data, big.NewInt(0)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Venue != VenueV2 {
		t.Errorf("venue = %s, want %s", got.Venue, VenueV2)
	}
	if got.Direction != DirectionSell {
		t.Errorf("direction = %s, want sell", got.Direction)
	}
	if got.AmountIn.String() != "1000000000000000000" {
		t.Errorf("amountIn = %s, want 1e18", got.AmountIn)
	}
	if got.AmountOutMin.Int64() != 5 {
		t.Errorf("amountOutMin = %s, want 5", got.AmountOutMin)
	}
	if got.FeeTier != 0 {
		t.Errorf("feeTier = %d, want 0 for v2", got.FeeTier)
	}
}

func TestV2ExactETHForTokensUsesValue(t *testing.T) {
	data := pack(t, "swapExactETHForTokens",
		big.NewInt(500), []common.Address{chain.WETH, tokenA}, recipient, big.NewInt(99))

	got, err := Swap(txTo(chain.V2Router, data, big.NewInt(2e16)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Venue != VenueV2 {
		t.Errorf("venue = %s, want %s", got.Venue, VenueV2)
	}
	if got.Direction != DirectionBuy {
		t.Errorf("direction = %s, want buy", got.Direction)
	}
	if got.AmountIn.Int64() != 2e16 {
		t.Errorf("amountIn = %s, want tx value", got.AmountIn)
	}
	if got.AmountOutMin.Int64() != 500 {
		t.Errorf("amountOutMin = %s, want 500", got.AmountOutMin)
	}
}

// The Uniswap front-end wraps swaps in multicall; the swap must still surface.
func TestMulticallUnwrapsSwap(t *testing.T) {
	inner := pack(t, "exactInputSingle", exactInputSingleParams{
		TokenIn: chain.WETH, TokenOut: tokenA, Fee: big.NewInt(10000),
		Recipient: recipient, AmountIn: big.NewInt(1e15),
		AmountOutMinimum: big.NewInt(7), SqrtPriceLimitX96: big.NewInt(0),
	})
	// A realistic bundle: an unrelated call, then the swap.
	junk := []byte{0x12, 0x34, 0x56, 0x78}
	data := pack(t, "multicall", [][]byte{junk, inner})

	got, err := Swap(txTo(chain.V3Router, data, big.NewInt(0)))
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Token() != tokenA {
		t.Errorf("token = %s, want %s", got.Token(), tokenA)
	}
	if got.AmountIn.Int64() != 1e15 {
		t.Errorf("amountIn = %s, want 1000000000000000", got.AmountIn)
	}
}

func TestUnknownRouterRejected(t *testing.T) {
	data := pack(t, "exactInputSingle", exactInputSingleParams{
		TokenIn: chain.WETH, TokenOut: tokenA, Fee: big.NewInt(3000),
		Recipient: recipient, AmountIn: big.NewInt(1),
		AmountOutMinimum: big.NewInt(1), SqrtPriceLimitX96: big.NewInt(0),
	})
	stranger := common.HexToAddress("0x1111111111111111111111111111111111111111")
	if _, err := Swap(txTo(stranger, data, big.NewInt(0))); err != ErrNotASwap {
		t.Fatalf("err = %v, want ErrNotASwap", err)
	}
}

// Calldata is attacker-controlled. Truncated and nonsense bodies must return an
// error, never panic.
func TestMalformedCalldataDoesNotPanic(t *testing.T) {
	cases := [][]byte{
		{},
		{0x04},
		{0x04, 0xe4, 0x5a, 0xaf}, // selector, no args
		append([]byte{0x04, 0xe4, 0x5a, 0xaf}, make([]byte, 31)...), // partial word
		append([]byte{0xb8, 0x58, 0x18, 0x3f}, make([]byte, 32)...), // bogus offset
		append([]byte{0xac, 0x96, 0x50, 0xd8}, make([]byte, 64)...), // empty multicall
		append([]byte{0x38, 0xed, 0x17, 0x39}, make([]byte, 96)...), // short v2 args
	}
	for i, data := range cases {
		if _, err := Swap(txTo(chain.V3Router, data, big.NewInt(0))); err == nil {
			t.Errorf("case %d: expected an error, got nil", i)
		}
	}
}

// A path offset pointing past the buffer must be rejected, not read out of bounds.
func TestHostilePathOffsetRejected(t *testing.T) {
	data := append([]byte{0xb8, 0x58, 0x18, 0x3f}, make([]byte, 32)...)
	// Offset = 0xffffffff, far past the end.
	for i := 28; i < 32; i++ {
		data[4+i] = 0xff
	}
	if _, err := Swap(txTo(chain.V3Router, data, big.NewInt(0))); err == nil {
		t.Fatal("expected an error for an out-of-range offset")
	}
}
