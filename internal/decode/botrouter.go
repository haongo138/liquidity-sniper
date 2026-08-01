package decode

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Third-party bot routers carry a large share of retail flow on this chain and
// are not Uniswap contracts. They are unverified, upgradeable proxies with no
// published ABI, so this decoder is deliberately structured as a HEURISTIC and
// is honest about that:
//
//   - Only the two fields observed stable across every sampled layout are read
//     positionally: word 2 (amountIn) and word 3 (amountOutMinimum). Sampling on
//     2026-07-31 found both an 18-word single-hop form and a 42-word three-hop
//     form; every other field moves between them, so no other index is trusted.
//   - The traded token is NOT read from a fixed offset. Candidate addresses are
//     extracted in calldata order and resolved against the pool registry by the
//     caller, which already has a cache. Whatever has a real WETH pool wins.
//
// Because the proxy owner can change the layout at any time, callers must watch
// the decode rate. A silent drop to zero is the expected failure mode, which is
// why Stats() exists and why the daemon alarms on it.

// BotRouters are non-Uniswap routers handled by the heuristic path.
//
// 0x65050a9b: EIP-1967 proxy (impl 0x73a160aa), exposes WETH(), routes into the
// V4 PoolManager. Seen used by 151 distinct wallets in a 75s sample.
var BotRouters = map[common.Address]bool{
	common.HexToAddress("0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc"): true,
}

// selBotSwap is the swap entrypoint observed on those routers.
var selBotSwap = [4]byte{0x4d, 0x81, 0x9a, 0x2a}

// infrastructure addresses are never the traded token.
var infrastructure = map[common.Address]bool{}

func init() {
	for _, a := range []string{
		"0x8366a39cc670b4001a1121b8f6a443a643e40951", // V4 PoolManager
		"0x0bd7d308f8e1639fab988df18a8011f41eacad73", // WETH
		"0x000000000022D473030F116dDEE9F6B43aC78BA3", // Permit2
		"0x65050A9b7E5075A2bA5cED7b1b64EE66262c40Dc", // the router itself
	} {
		infrastructure[common.HexToAddress(a)] = true
	}
}

// BotSwap is a partially-decoded swap awaiting token resolution.
type BotSwap struct {
	Router common.Address
	// Direction is inferred from tx.Value: native ETH in means a buy.
	Direction Direction
	// AmountIn is word 2 for a token-input swap, or tx.Value for an ETH-input
	// swap. Trust it less than a Uniswap decode — see the package note.
	AmountIn     *big.Int
	AmountOutMin *big.Int
	// Candidates are address-shaped words in calldata order, minus known
	// infrastructure. The caller resolves these against the pool registry.
	Candidates []common.Address
	// WETHSeen reports whether WETH appeared in the calldata, which
	// distinguishes the multi-hop form from the single-hop one.
	WETHSeen bool
}

// IsBotRouter reports whether addr is handled by the heuristic path.
func IsBotRouter(addr common.Address) bool { return BotRouters[addr] }

// BotRouterSwap decodes a bot-router call as far as is safely possible without
// chain access. The caller must resolve Candidates to finish the job.
func BotRouterSwap(tx *types.Transaction) (BotSwap, error) {
	to := tx.To()
	if to == nil || !BotRouters[*to] {
		return BotSwap{}, ErrNotASwap
	}
	data := tx.Data()
	if len(data) < 4 {
		return BotSwap{}, ErrNotASwap
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	if sel != selBotSwap {
		return BotSwap{}, ErrNotASwap
	}

	args := data[4:]
	// Both observed layouts share a head of at least six words.
	if len(args) < 6*wordSize {
		return BotSwap{}, ErrNotASwap
	}

	out := BotSwap{
		Router:       *to,
		AmountIn:     wordBig(args, 2),
		AmountOutMin: wordBig(args, 3),
		Direction:    DirectionSell,
	}
	// A native-ETH input means the wallet is buying, and the value is the real
	// input amount regardless of what word 2 says.
	if v := tx.Value(); v != nil && v.Sign() > 0 {
		out.Direction = DirectionBuy
		out.AmountIn = new(big.Int).Set(v)
	}

	out.Candidates, out.WETHSeen = addressCandidates(args)
	if len(out.Candidates) == 0 {
		return BotSwap{}, ErrNotASwap
	}
	return out, nil
}

// addressCandidates extracts address-shaped words in calldata order. A word
// qualifies when its top 12 bytes are zero and the low 20 bytes are large
// enough not to be a small integer — real integers like fee tiers, tick
// spacings and array lengths all fall below the cutoff.
func addressCandidates(args []byte) ([]common.Address, bool) {
	const minAddrValue = 1 << 32

	var out []common.Address
	seen := map[common.Address]bool{}
	wethSeen := false

	for i := 0; i*wordSize < len(args); i++ {
		w, ok := word(args, i)
		if !ok {
			break
		}
		if !isZero(w[:12]) {
			continue
		}
		v := new(big.Int).SetBytes(w[12:])
		if v.BitLen() < 33 || v.Cmp(big.NewInt(minAddrValue)) < 0 {
			continue
		}
		addr := common.BytesToAddress(w[12:])
		if addr == wethAddress {
			wethSeen = true
			continue
		}
		if infrastructure[addr] || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out, wethSeen
}

// wethAddress mirrors chain.WETH without importing it, so decode stays free of
// a dependency cycle.
var wethAddress = common.HexToAddress("0x0bd7d308f8e1639fab988df18a8011f41eacad73")

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// Resolve builds a SwapIntent once the caller has identified the real token.
func (b BotSwap) Resolve(token common.Address) SwapIntent {
	in, outTok := wethAddress, token
	if b.Direction == DirectionSell {
		in, outTok = token, wethAddress
	}
	return SwapIntent{
		Venue:        VenueUniversal,
		Router:       b.Router,
		Direction:    b.Direction,
		TokenIn:      in,
		TokenOut:     outTok,
		AmountIn:     b.AmountIn,
		AmountOutMin: b.AmountOutMin,
		Path:         []common.Address{in, outTok},
	}
}
