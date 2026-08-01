package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

// Uniswap V4 support.
//
// V4 is a SINGLETON: every pool lives inside one PoolManager contract, so there
// is no per-pool address and `WETH.balanceOf(pool)` — the measure used for V2
// and V3 — is meaningless. Ignoring that made the liquidity filter gate on
// whichever dormant V3 dust pool happened to exist, rejecting actively-traded
// tokens. One token was rejected fifteen times at a fixed 0.22 ETH while the
// PoolManager held 1,434 of it against 1,074 WETH.
//
// Two facts make this tractable, and both were measured rather than assumed:
//
//  1. V4 denominates native ETH as address(0), NOT WETH. Pool discovery that
//     looks for WETH pairs finds nothing, which is why V4 pools were invisible.
//  2. The PoolManager emits Initialize with the poolId as its first indexed
//     topic. Reconstructing the poolId by hashing a guessed PoolKey failed
//     (every storage read returned zero); reading it from the event cannot.
//
// Depth then comes from the pool's stored liquidity and price, which was
// verified against a Swap event's own liquidity field before being trusted.

const (
	// v4PoolsSlot is the storage slot of PoolManager._pools. Confirmed by
	// scanning slots against a poolId taken from a live Swap event until the
	// stored liquidity matched that event's liquidity exactly.
	v4PoolsSlot = 6
	// v4LiquidityOffset is Pool.State.liquidity within the pool's struct.
	v4LiquidityOffset = 3
	// selExtsload is PoolManager.extsload(bytes32).
	selExtsload = "0x1e2eaeaf"
)

// v4InitializeTopic is
// Initialize(bytes32,address,address,uint24,int24,address,uint160,int24).
var v4InitializeTopic = common.HexToHash(
	"0xdd466e674ea557f56295e2d0218a125ea4b4f0f6f3307b95f85e6110838d6438")

// NativeETH is how V4 denominates ether. It sorts before every real token, so
// it is always currency0 in an ETH-paired pool.
var NativeETH = common.Address{}

// two96 is the Q96 fixed-point scale V4 prices use.
var two96 = new(big.Int).Lsh(big.NewInt(1), 96)

// V4Pool identifies one initialized pool.
type V4Pool struct {
	ID          common.Hash
	Currency0   common.Address
	Currency1   common.Address
	Fee         uint32
	TickSpacing int32
	Hooks       common.Address
}

// PairedAsset returns the ETH-side currency and whether it is currency0.
func (p V4Pool) PairedAsset() (common.Address, bool) {
	if p.Currency0 == NativeETH || p.Currency0 == WETH {
		return p.Currency0, true
	}
	return p.Currency1, false
}

// findV4Pools locates every initialized pool pairing token against native ETH
// or WETH.
//
// Filtering on the indexed currency keeps the result set to a handful even
// across the whole chain — an unfiltered query trips the node's 10,000-log
// limit, a filtered one does not.
func (c *Client) findV4Pools(ctx context.Context, token common.Address) ([]V4Pool, error) {
	tokenTopic := common.BytesToHash(token.Bytes())

	queries := []map[string]any{
		// token as currency0 (its counterparty must then be a real token, but
		// WETH can still sit on the other side).
		{"address": V4PoolManager.Hex(), "fromBlock": "0x0", "toBlock": "latest",
			"topics": []any{v4InitializeTopic.Hex(), nil, tokenTopic.Hex()}},
		// token as currency1 — the usual case, since ETH sorts first.
		{"address": V4PoolManager.Hex(), "fromBlock": "0x0", "toBlock": "latest",
			"topics": []any{v4InitializeTopic.Hex(), nil, nil, tokenTopic.Hex()}},
	}

	var out []V4Pool
	seen := map[common.Hash]bool{}
	for _, q := range queries {
		var logs []struct {
			Topics []common.Hash `json:"topics"`
			Data   string        `json:"data"`
		}
		if err := c.rpc.CallContext(ctx, &logs, "eth_getLogs", q); err != nil {
			// A failed query for one side should not discard the other.
			continue
		}
		for _, lg := range logs {
			if len(lg.Topics) < 4 {
				continue
			}
			p := V4Pool{
				ID:        lg.Topics[1],
				Currency0: common.BytesToAddress(lg.Topics[2].Bytes()),
				Currency1: common.BytesToAddress(lg.Topics[3].Bytes()),
			}
			if seen[p.ID] {
				continue
			}
			// Only ETH-denominated pools are useful: the filter gates on what
			// could be sold back into ETH.
			if p.Currency0 != NativeETH && p.Currency0 != WETH &&
				p.Currency1 != NativeETH && p.Currency1 != WETH {
				continue
			}
			fee, tick, hooks, ok := parseInitializeData(lg.Data)
			if !ok {
				continue
			}
			p.Fee, p.TickSpacing, p.Hooks = fee, tick, hooks
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// parseInitializeData reads fee, tickSpacing and hooks from the event body.
func parseInitializeData(data string) (uint32, int32, common.Address, bool) {
	b := data
	if len(b) >= 2 && b[:2] == "0x" {
		b = b[2:]
	}
	if len(b) < 3*64 {
		return 0, 0, common.Address{}, false
	}
	word := func(i int) *big.Int {
		v, ok := new(big.Int).SetString(b[i*64:(i+1)*64], 16)
		if !ok {
			return nil
		}
		return v
	}
	fee, tickRaw := word(0), word(1)
	if fee == nil || tickRaw == nil || !fee.IsUint64() {
		return 0, 0, common.Address{}, false
	}
	return uint32(fee.Uint64()), int32(toSigned(tickRaw).Int64()),
		common.HexToAddress(b[2*64 : 3*64]), true
}

// toSigned reinterprets a 256-bit word as two's-complement, which int24 fields
// such as tickSpacing and tick require.
func toSigned(v *big.Int) *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 255)
	if v.Cmp(limit) < 0 {
		return v
	}
	return new(big.Int).Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
}

// v4Depth returns the ETH-side depth of a pool, in wei.
//
// V4 holds concentrated liquidity, so there is no single "reserve". This
// converts the pool's liquidity L and current price into the virtual reserve at
// that price, which is the honest comparable to a V2/V3 pool's WETH balance:
// what is available to trade against right now. It overstates depth for a range
// that ends near the current tick and understates it for a wide one, so treat
// it as an order of magnitude rather than an exact figure.
func (c *Client) v4Depth(ctx context.Context, p V4Pool) (*big.Int, error) {
	base := v4StateSlot(p.ID)

	calls := []rpc.BatchElem{
		extsloadElem(base),
		extsloadElem(new(big.Int).Add(base, big.NewInt(v4LiquidityOffset))),
	}
	if err := c.rpc.BatchCallContext(ctx, calls); err != nil {
		return nil, err
	}
	slot0, liquidity := resultBig(calls[0]), resultBig(calls[1])
	if slot0 == nil || liquidity == nil || liquidity.Sign() == 0 {
		return big.NewInt(0), nil
	}

	// Slot0 packs sqrtPriceX96 in its low 160 bits.
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPriceX96 := new(big.Int).And(slot0, mask)
	if sqrtPriceX96.Sign() == 0 {
		return big.NewInt(0), nil
	}

	// amount0 = L * 2^96 / sqrtP   ·   amount1 = L * sqrtP / 2^96
	if _, ethIsCurrency0 := p.PairedAsset(); ethIsCurrency0 {
		return new(big.Int).Div(new(big.Int).Mul(liquidity, two96), sqrtPriceX96), nil
	}
	return new(big.Int).Div(new(big.Int).Mul(liquidity, sqrtPriceX96), two96), nil
}

// v4StateSlot is keccak256(poolId . poolsSlot), the base of the pool's struct.
func v4StateSlot(id common.Hash) *big.Int {
	var buf []byte
	buf = append(buf, id.Bytes()...)
	buf = append(buf, common.LeftPadBytes(big.NewInt(v4PoolsSlot).Bytes(), 32)...)
	return new(big.Int).SetBytes(crypto.Keccak256(buf))
}

func extsloadElem(slot *big.Int) rpc.BatchElem {
	out := new(string)
	return rpc.BatchElem{
		Method: "eth_call",
		Args: []any{
			map[string]any{"to": V4PoolManager.Hex(),
				"data": fmt.Sprintf("%s%064x", selExtsload, slot)},
			"latest",
		},
		Result: out,
	}
}
