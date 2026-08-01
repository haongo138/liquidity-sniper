package decode

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Golden calldata captured from mainnet tx
// 0x7dcc021d4b2f4d3232d757d6d3704040254ad30bd3ff6cfa42395ef3869f6904,
// sent by 0xe28601...216d to bot router 0x65050A9b. Single-hop sell of
// 0xe595a5a4... for WETH: the receipt shows a Transfer of exactly 3274547534468479395430934
// of that token from the sender to the V4 PoolManager, which is what word 2 must decode to.
const goldenSingleHop = "4d819a2a" +
	"00000000000000000000000000000000000000000000000000000000000000a0" + // w0 offset
	"0000000000000000000000000000000000000000000000000000000000000000" + // w1
	"00000000000000000000000000000000000000000002b56993d996afe6ee0216" + // w2 amountIn
	"00000000000000000000000000000000000000000000000000ccb67bde51bda2" + // w3 amountOutMin
	"0000000000000000000000000000000000000000000000000000000000000000" + // w4
	"0000000000000000000000000000000000000000000000000000000000000001" + // w5 hops
	"0000000000000000000000000000000000000000000000000000000000000020" + // w6
	"0000000000000000000000000000000000000000000000000000000000000002" + // w7
	"000000000000000000000000e595a5a411c9c236939130791ef5f9e3242209f2" + // w8 token
	"0000000000000000000000000000000000000000000000000000000000000000" + // w9
	"0000000000000000000000000000000000000000000000000000000000000000" + // w10
	"0000000000000000000000000000000000000000000000000000000000000bb8" + // w11 fee 3000
	"000000000000000000000000000000000000000000000000000000000000003c" + // w12 tick 60
	"000000000000000000000000d7634d1b30c230265a036cbd8b957069eee0e2c4" + // w13 hooks
	"0000000000000000000000000000000000000000000000000000000000000140" + // w14
	"0000000000000000000000008366a39cc670b4001a1121b8f6a443a643e40951" + // w15 PoolManager
	"0000000000000000000000000000000000000000000000000000000000000000" + // w16
	"0000000000000000000000000000000000000000000000000000000000000000" //   w17

var botRouter = common.HexToAddress("0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc")

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("decoding golden calldata: %v", err)
	}
	return b
}

func TestBotRouterSingleHopSell(t *testing.T) {
	tx := txTo(botRouter, mustHex(t, goldenSingleHop), big.NewInt(0))

	got, err := BotRouterSwap(tx)
	if err != nil {
		t.Fatalf("BotRouterSwap: %v", err)
	}
	if got.Direction != DirectionSell {
		t.Errorf("direction = %s, want sell (no ETH value)", got.Direction)
	}
	// Must match the Transfer amount in the on-chain receipt exactly.
	if want := "3274547534468479395430934"; got.AmountIn.String() != want {
		t.Errorf("amountIn = %s, want %s", got.AmountIn, want)
	}
	// 0x00ccb67bde51bda2. Sanity: below the 89543960180427938 wei actually
	// received in the receipt, as a slippage floor must be.
	if want := "57621538376105378"; got.AmountOutMin.String() != want {
		t.Errorf("amountOutMin = %s, want %s", got.AmountOutMin, want)
	}

	// The real token must be among the candidates, and infrastructure must not.
	token := common.HexToAddress("0xe595a5a411c9c236939130791ef5f9e3242209f2")
	var sawToken bool
	for _, c := range got.Candidates {
		if c == token {
			sawToken = true
		}
		if infrastructure[c] {
			t.Errorf("infrastructure address %s leaked into candidates", c)
		}
		if c == wethAddress {
			t.Error("WETH leaked into candidates")
		}
	}
	if !sawToken {
		t.Errorf("traded token %s missing from candidates %v", token, got.Candidates)
	}
}

// A native-ETH input means a buy, and tx.Value is the real amount regardless of
// what word 2 holds.
func TestBotRouterValueMeansBuy(t *testing.T) {
	tx := txTo(botRouter, mustHex(t, goldenSingleHop), big.NewInt(4e15))
	got, err := BotRouterSwap(tx)
	if err != nil {
		t.Fatalf("BotRouterSwap: %v", err)
	}
	if got.Direction != DirectionBuy {
		t.Errorf("direction = %s, want buy", got.Direction)
	}
	if got.AmountIn.Int64() != 4e15 {
		t.Errorf("amountIn = %s, want tx value 4000000000000000", got.AmountIn)
	}
}

// Resolve orients the intent around whichever token the caller identified.
func TestBotSwapResolveOrientation(t *testing.T) {
	token := common.HexToAddress("0xe595a5a411c9c236939130791ef5f9e3242209f2")

	sell := BotSwap{Direction: DirectionSell, AmountIn: big.NewInt(5), AmountOutMin: big.NewInt(1)}
	if got := sell.Resolve(token); got.TokenIn != token || got.TokenOut != wethAddress {
		t.Errorf("sell: in=%s out=%s, want token->WETH", got.TokenIn, got.TokenOut)
	} else if got.Token() != token {
		t.Errorf("sell: Token() = %s, want %s", got.Token(), token)
	}

	buy := BotSwap{Direction: DirectionBuy, AmountIn: big.NewInt(5), AmountOutMin: big.NewInt(1)}
	if got := buy.Resolve(token); got.TokenIn != wethAddress || got.TokenOut != token {
		t.Errorf("buy: in=%s out=%s, want WETH->token", got.TokenIn, got.TokenOut)
	}
}

// Small integers (fee tiers, tick spacings, array lengths) must never be
// mistaken for addresses.
func TestCandidatesExcludeSmallIntegers(t *testing.T) {
	got, err := BotRouterSwap(txTo(botRouter, mustHex(t, goldenSingleHop), big.NewInt(0)))
	if err != nil {
		t.Fatalf("BotRouterSwap: %v", err)
	}
	for _, c := range got.Candidates {
		if c.Big().BitLen() < 33 {
			t.Errorf("small integer %s leaked into candidates", c.Hex())
		}
	}
}

func TestBotRouterRejectsWrongSelectorAndTarget(t *testing.T) {
	// Right target, wrong selector.
	wrongSel := append([]byte{0xde, 0xad, 0xbe, 0xef}, make([]byte, 6*32)...)
	if _, err := BotRouterSwap(txTo(botRouter, wrongSel, big.NewInt(0))); err != ErrNotASwap {
		t.Errorf("err = %v, want ErrNotASwap for a foreign selector", err)
	}
	// Right selector, wrong target.
	stranger := common.HexToAddress("0x1111111111111111111111111111111111111111")
	if _, err := BotRouterSwap(txTo(stranger, mustHex(t, goldenSingleHop), big.NewInt(0))); err != ErrNotASwap {
		t.Errorf("err = %v, want ErrNotASwap for a foreign router", err)
	}
}

// Truncated bodies must error, never panic — this is attacker-controlled data.
func TestBotRouterMalformedDoesNotPanic(t *testing.T) {
	for i, data := range [][]byte{
		{0x4d, 0x81, 0x9a, 0x2a},
		append([]byte{0x4d, 0x81, 0x9a, 0x2a}, make([]byte, 31)...),
		append([]byte{0x4d, 0x81, 0x9a, 0x2a}, make([]byte, 5*32)...),
		append([]byte{0x4d, 0x81, 0x9a, 0x2a}, make([]byte, 6*32)...), // all-zero: no candidates
	} {
		if _, err := BotRouterSwap(txTo(botRouter, data, big.NewInt(0))); err == nil {
			t.Errorf("case %d: expected an error, got nil", i)
		}
	}
}

// Swap() must not claim bot-router calls — they need the resolving path.
func TestSwapDoesNotHandleBotRouter(t *testing.T) {
	if _, err := Swap(txTo(botRouter, mustHex(t, goldenSingleHop), big.NewInt(0))); err != ErrNotASwap {
		t.Errorf("err = %v, want ErrNotASwap", err)
	}
	if !IsBotRouter(botRouter) {
		t.Error("IsBotRouter did not recognise the bot router")
	}
}
