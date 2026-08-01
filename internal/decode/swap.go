// Package decode turns router calldata into a SwapIntent.
//
// Selector coverage is driven by observed traffic, not by the full router ABI:
// a 75s mainnet sample showed exactInputSingle (0x04e45aaf) and multicall
// (0xac9650d8) on SwapRouter02 carrying ~25x the volume of every other swap
// path combined. Unrecognised calls return ErrNotASwap rather than a guess.
package decode

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Venue identifies which AMM the swap routes through.
type Venue string

const (
	VenueV2        Venue = "uniswap-v2"
	VenueV3        Venue = "uniswap-v3"
	VenueUniversal Venue = "universal-router"
)

// Direction reports whether the KOL is entering or exiting a position.
type Direction string

const (
	// DirectionBuy means WETH (or native ETH) went in and a token came out.
	DirectionBuy Direction = "buy"
	// DirectionSell means a token went in and WETH came out.
	DirectionSell Direction = "sell"
	// DirectionSwap is a token-to-token swap touching no WETH leg.
	DirectionSwap Direction = "swap"
)

// ErrNotASwap means the calldata is not a swap this package understands.
var ErrNotASwap = errors.New("decode: not a recognised swap")

// SwapIntent is an immutable description of what a transaction is trying to do.
// Decoders return a new value; nothing mutates an intent after construction.
type SwapIntent struct {
	Venue     Venue
	Router    common.Address
	Direction Direction

	TokenIn  common.Address
	TokenOut common.Address

	// AmountIn is the exact input for exact-in swaps, or the maximum input for
	// exact-out swaps. For native-ETH swaps it comes from tx.Value().
	AmountIn *big.Int
	// AmountOutMin is the caller's slippage floor. Zero means unprotected.
	AmountOutMin *big.Int
	// ExactOut is true when the caller pinned the output rather than the input.
	ExactOut bool

	Recipient common.Address
	// FeeTier is the V3 pool fee in hundredths of a bip (3000 == 0.30%).
	// Zero for V2.
	FeeTier uint32
	// Path is the full hop sequence, at least two entries.
	Path []common.Address
}

// Token returns the non-WETH side of the swap — the asset actually being
// speculated on. For token-to-token swaps it returns TokenOut.
func (s SwapIntent) Token() common.Address {
	switch s.Direction {
	case DirectionBuy:
		return s.TokenOut
	case DirectionSell:
		return s.TokenIn
	default:
		return s.TokenOut
	}
}

// Selectors observed carrying flow on Robinhood Chain.
var (
	// Uniswap V2 router.
	selV2SwapExactTokensForTokens    = [4]byte{0x38, 0xed, 0x17, 0x39}
	selV2SwapExactETHForTokens       = [4]byte{0x7f, 0xf3, 0x6a, 0xb5}
	selV2SwapExactTokensForETH       = [4]byte{0x18, 0xcb, 0xaf, 0xe5}
	selV2SwapExactETHForTokensFee    = [4]byte{0xb6, 0xf9, 0xde, 0x95}
	selV2SwapExactTokensForTokensFee = [4]byte{0x5c, 0x11, 0xd7, 0x95}
	selV2SwapExactTokensForETHFee    = [4]byte{0x79, 0x1a, 0xc9, 0x47}
	selV2SwapTokensForExactTokens    = [4]byte{0x88, 0x03, 0xdb, 0xee}
	selV2SwapETHForExactTokens       = [4]byte{0xfb, 0x3b, 0xdb, 0x41}
	selV2SwapTokensForExactETH       = [4]byte{0x4a, 0x25, 0xd9, 0x4a}

	// Uniswap V3 SwapRouter02.
	selV3ExactInputSingle  = [4]byte{0x04, 0xe4, 0x5a, 0xaf}
	selV3ExactInput        = [4]byte{0xb8, 0x58, 0x18, 0x3f}
	selV3ExactOutputSingle = [4]byte{0x50, 0x23, 0xb4, 0xdf}
	selV3ExactOutput       = [4]byte{0x09, 0xb8, 0x13, 0x46}

	// Multicall wrappers. The Uniswap front-end wraps nearly every swap.
	selMulticall             = [4]byte{0xac, 0x96, 0x50, 0xd8} // multicall(bytes[])
	selMulticallDeadline     = [4]byte{0x5a, 0xe4, 0x01, 0xdc} // multicall(uint256,bytes[])
	selMulticallPrevBlockNum = [4]byte{0x1f, 0x00, 0x64, 0xd9} // multicall(bytes32,bytes[])
)

// maxMulticallDepth guards against a self-referential multicall.
const maxMulticallDepth = 4

// Swap decodes tx into a SwapIntent. It returns ErrNotASwap when the target is
// not a known router or the selector is not a swap we understand.
func Swap(tx *types.Transaction) (SwapIntent, error) {
	to := tx.To()
	if to == nil {
		return SwapIntent{}, ErrNotASwap
	}
	venue, ok := routerVenue(*to)
	if !ok {
		return SwapIntent{}, ErrNotASwap
	}
	return decodeCall(*to, venue, tx.Data(), tx.Value(), 0)
}

func routerVenue(to common.Address) (Venue, bool) {
	switch to {
	case chain.V2Router:
		return VenueV2, true
	case chain.V3Router:
		return VenueV3, true
	case chain.UniversalRouter:
		return VenueUniversal, true
	default:
		return "", false
	}
}

// decodeCall dispatches on the selector, unwrapping multicall as needed.
func decodeCall(router common.Address, venue Venue, data []byte, value *big.Int, depth int) (SwapIntent, error) {
	if len(data) < 4 {
		return SwapIntent{}, ErrNotASwap
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	args := data[4:]

	switch sel {
	case selMulticall, selMulticallDeadline, selMulticallPrevBlockNum:
		return decodeMulticall(router, venue, sel, args, value, depth)

	case selV3ExactInputSingle:
		return decodeExactInputSingle(router, args, value)
	case selV3ExactOutputSingle:
		return decodeExactOutputSingle(router, args, value)
	case selV3ExactInput:
		return decodeExactInputPath(router, args, value, false)
	case selV3ExactOutput:
		return decodeExactInputPath(router, args, value, true)

	case selV2SwapExactTokensForTokens, selV2SwapExactTokensForETH,
		selV2SwapExactTokensForTokensFee, selV2SwapExactTokensForETHFee:
		// (amountIn, amountOutMin, path, to, deadline)
		return decodeV2(router, args, nil, false)
	case selV2SwapExactETHForTokens, selV2SwapExactETHForTokensFee:
		// (amountOutMin, path, to, deadline) — input is msg.value
		return decodeV2ETHIn(router, args, value, false)
	case selV2SwapTokensForExactTokens, selV2SwapTokensForExactETH:
		// (amountOut, amountInMax, path, to, deadline)
		return decodeV2(router, args, nil, true)
	case selV2SwapETHForExactTokens:
		// (amountOut, path, to, deadline) — input capped by msg.value
		return decodeV2ETHIn(router, args, value, true)

	default:
		return SwapIntent{}, ErrNotASwap
	}
}

// decodeMulticall walks the wrapped calls and returns the first real swap.
// A multicall typically bundles approve/permit/refundETH around one swap.
func decodeMulticall(router common.Address, venue Venue, sel [4]byte, args []byte, value *big.Int, depth int) (SwapIntent, error) {
	if depth >= maxMulticallDepth {
		return SwapIntent{}, ErrNotASwap
	}
	// multicall(bytes[]) puts the array offset first; the deadline and
	// previousBlockhash variants prepend one static word before it.
	offsetWord := 0
	if sel == selMulticallDeadline || sel == selMulticallPrevBlockNum {
		offsetWord = 1
	}
	calls, err := readBytesArray(args, offsetWord)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	for _, call := range calls {
		intent, err := decodeCall(router, venue, call, value, depth+1)
		if err == nil {
			return intent, nil
		}
	}
	return SwapIntent{}, ErrNotASwap
}

// decodeExactInputSingle handles
// exactInputSingle((address,address,uint24,address,uint256,uint256,uint160)).
// Every member is static, so the tuple is encoded inline with no head offset.
func decodeExactInputSingle(router common.Address, args []byte, value *big.Int) (SwapIntent, error) {
	if len(args) < 7*32 {
		return SwapIntent{}, ErrNotASwap
	}
	tokenIn := wordAddress(args, 0)
	tokenOut := wordAddress(args, 1)
	fee := wordBig(args, 2)
	recipient := wordAddress(args, 3)
	amountIn := wordBig(args, 4)
	amountOutMin := wordBig(args, 5)

	// A native-ETH swap sends value and lets the router wrap it.
	if amountIn.Sign() == 0 && value != nil && value.Sign() > 0 {
		amountIn = new(big.Int).Set(value)
	}
	return newIntent(VenueV3, router, tokenIn, tokenOut, amountIn, amountOutMin,
		recipient, uint32(fee.Uint64()), false,
		[]common.Address{tokenIn, tokenOut}), nil
}

// decodeExactOutputSingle handles the exact-output twin, where word 4 is
// amountOut and word 5 is amountInMaximum.
func decodeExactOutputSingle(router common.Address, args []byte, value *big.Int) (SwapIntent, error) {
	if len(args) < 7*32 {
		return SwapIntent{}, ErrNotASwap
	}
	tokenIn := wordAddress(args, 0)
	tokenOut := wordAddress(args, 1)
	fee := wordBig(args, 2)
	recipient := wordAddress(args, 3)
	amountInMax := wordBig(args, 5)

	if amountInMax.Sign() == 0 && value != nil && value.Sign() > 0 {
		amountInMax = new(big.Int).Set(value)
	}
	return newIntent(VenueV3, router, tokenIn, tokenOut, amountInMax, big.NewInt(0),
		recipient, uint32(fee.Uint64()), true,
		[]common.Address{tokenIn, tokenOut}), nil
}

// decodeExactInputPath handles exactInput((bytes,address,uint256,uint256)) and
// its exact-output twin. The tuple is dynamic, so the head holds an offset.
func decodeExactInputPath(router common.Address, args []byte, value *big.Int, exactOut bool) (SwapIntent, error) {
	if len(args) < 32 {
		return SwapIntent{}, ErrNotASwap
	}
	base, err := readOffset(args, 0)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	body := args[base:]
	if len(body) < 4*32 {
		return SwapIntent{}, ErrNotASwap
	}
	pathOff, err := readOffset(body, 0)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	recipient := wordAddress(body, 1)
	amount := wordBig(body, 2)

	raw, err := readBytes(body, pathOff)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	path, fee, err := parseV3Path(raw)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	// An exact-output path is encoded reversed: it runs tokenOut -> tokenIn.
	if exactOut {
		path = reverseAddrs(path)
	}
	if amount.Sign() == 0 && value != nil && value.Sign() > 0 {
		amount = new(big.Int).Set(value)
	}
	return newIntent(VenueV3, router, path[0], path[len(path)-1], amount,
		big.NewInt(0), recipient, fee, exactOut, path), nil
}

// decodeV2 handles the token-input V2 forms:
// (amountIn|amountOut, amountOutMin|amountInMax, address[] path, to, deadline).
func decodeV2(router common.Address, args []byte, value *big.Int, exactOut bool) (SwapIntent, error) {
	if len(args) < 5*32 {
		return SwapIntent{}, ErrNotASwap
	}
	first := wordBig(args, 0)
	second := wordBig(args, 1)
	pathOff, err := readOffset(args, 2)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	recipient := wordAddress(args, 3)
	path, err := readAddressArray(args, pathOff)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}

	amountIn, outMin := first, second
	if exactOut {
		// swapTokensForExact*: first is amountOut, second is amountInMax.
		amountIn, outMin = second, first
	}
	return newIntent(VenueV2, router, path[0], path[len(path)-1], amountIn,
		outMin, recipient, 0, exactOut, path), nil
}

// decodeV2ETHIn handles the native-ETH input forms, where the input amount is
// msg.value rather than a parameter.
func decodeV2ETHIn(router common.Address, args []byte, value *big.Int, exactOut bool) (SwapIntent, error) {
	if len(args) < 4*32 {
		return SwapIntent{}, ErrNotASwap
	}
	amountOut := wordBig(args, 0)
	pathOff, err := readOffset(args, 1)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}
	recipient := wordAddress(args, 2)
	path, err := readAddressArray(args, pathOff)
	if err != nil {
		return SwapIntent{}, ErrNotASwap
	}

	amountIn := big.NewInt(0)
	if value != nil {
		amountIn = new(big.Int).Set(value)
	}
	outMin := amountOut
	if exactOut {
		outMin = big.NewInt(0)
	}
	return newIntent(VenueV2, router, path[0], path[len(path)-1], amountIn,
		outMin, recipient, 0, exactOut, path), nil
}

// newIntent builds an intent and classifies its direction against WETH.
func newIntent(venue Venue, router, tokenIn, tokenOut common.Address,
	amountIn, amountOutMin *big.Int, recipient common.Address,
	fee uint32, exactOut bool, path []common.Address) SwapIntent {

	dir := DirectionSwap
	switch {
	case tokenIn == chain.WETH && tokenOut != chain.WETH:
		dir = DirectionBuy
	case tokenOut == chain.WETH && tokenIn != chain.WETH:
		dir = DirectionSell
	}
	if amountIn == nil {
		amountIn = big.NewInt(0)
	}
	if amountOutMin == nil {
		amountOutMin = big.NewInt(0)
	}
	return SwapIntent{
		Venue:        venue,
		Router:       router,
		Direction:    dir,
		TokenIn:      tokenIn,
		TokenOut:     tokenOut,
		AmountIn:     amountIn,
		AmountOutMin: amountOutMin,
		ExactOut:     exactOut,
		Recipient:    recipient,
		FeeTier:      fee,
		Path:         path,
	}
}

// parseV3Path unpacks the packed encoding token(20) fee(3) token(20) [fee token...]
// and returns the hop sequence plus the first fee tier.
func parseV3Path(raw []byte) ([]common.Address, uint32, error) {
	const addrLen, feeLen = 20, 3
	if len(raw) < addrLen+feeLen+addrLen || (len(raw)-addrLen)%(addrLen+feeLen) != 0 {
		return nil, 0, fmt.Errorf("decode: bad v3 path length %d", len(raw))
	}
	var out []common.Address
	out = append(out, common.BytesToAddress(raw[:addrLen]))
	firstFee := uint32(raw[addrLen])<<16 | uint32(raw[addrLen+1])<<8 | uint32(raw[addrLen+2])

	for i := addrLen; i+feeLen+addrLen <= len(raw); i += feeLen + addrLen {
		out = append(out, common.BytesToAddress(raw[i+feeLen:i+feeLen+addrLen]))
	}
	return out, firstFee, nil
}

func reverseAddrs(in []common.Address) []common.Address {
	out := make([]common.Address, len(in))
	for i, a := range in {
		out[len(in)-1-i] = a
	}
	return out
}
