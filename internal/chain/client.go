package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// Function selectors used for state reads.
var (
	selBalanceOf   = "0x70a08231" // balanceOf(address)
	selTotalSupply = "0x18160ddd" // totalSupply()
	selDecimals    = "0x313ce567" // decimals()
	selGetPairV2   = "0xe6a43905" // getPair(address,address)
	selGetPoolV3   = "0x1698ee82" // getPool(address,address,uint24)
	selOwner       = "0x8da5cb5b" // owner()
)

// V3 fee tiers, cheapest first. The 1% tier carries most of the speculative
// long tail on this chain — the sampled tokens had no V2 pair at all.
var V3FeeTiers = []uint32{500, 3000, 10000}

// Client reads chain state. It batches reads into a single JSON-RPC batch so
// the hot path costs one round trip rather than one per check.
type Client struct {
	rpc *rpc.Client
}

// Dial connects to an RPC endpoint.
func Dial(ctx context.Context, url string) (*Client, error) {
	c, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Client{rpc: c}, nil
}

// Close releases the connection.
func (c *Client) Close() { c.rpc.Close() }

// Pool describes where a token's liquidity lives.
type Pool struct {
	// Address is the pool contract for V2 and V3. V4 has no per-pool contract,
	// so this holds the low 20 bytes of the poolId purely for display; use
	// V4PoolID for anything real.
	Address common.Address
	// Venue is "uniswap-v2", "uniswap-v3" or "uniswap-v4".
	Venue string
	// FeeTier is the fee in hundredths of a bip; zero for V2.
	FeeTier uint32
	// WETHLiquidity is the ETH-side depth of the pool, in wei. This is the
	// number the min-liquidity filter gates on: what could actually be sold
	// into, regardless of what the token side claims to be worth.
	//
	// For V2 and V3 it is the pool's WETH balance. For V4 — a singleton with no
	// per-pool balance — it is the virtual reserve derived from the pool's
	// liquidity and current price. See internal/chain/v4.go.
	WETHLiquidity *big.Int
	// V4PoolID identifies the pool inside the singleton PoolManager. Zero for
	// V2 and V3.
	V4PoolID common.Hash
	// V4Currency0 and V4Currency1 are the pool's ordered currencies. They are
	// carried because the depth formula depends on which side holds ETH, and
	// re-reading depth from a cached profile must not have to rediscover them.
	V4Currency0 common.Address
	V4Currency1 common.Address
}

// TokenState is an immutable snapshot of everything the filters need.
type TokenState struct {
	Token    common.Address
	Decimals uint8
	Supply   *big.Int

	// Pool is the deepest pool found, or nil when the token has no WETH pair.
	Pool *Pool

	// LPTotalSupply and LPBurned are only meaningful for V2, where LP tokens
	// are fungible ERC-20s. Both are nil on V3, where liquidity is held as
	// position NFTs and "LP burnt" has no equivalent.
	LPTotalSupply *big.Int
	LPBurned      *big.Int

	// Owner is the contract owner, or the zero address when ownership is
	// renounced or the token exposes no owner().
	Owner common.Address
}

// LPSecuredPct returns the share of V2 LP supply sent to a burn address, and
// whether the figure applies at all. It reports false for V3 rather than
// inventing a passing score for a check that cannot be performed.
func (t TokenState) LPSecuredPct() (float64, bool) {
	if t.LPTotalSupply == nil || t.LPBurned == nil || t.LPTotalSupply.Sign() == 0 {
		return 0, false
	}
	burned := new(big.Float).SetInt(t.LPBurned)
	total := new(big.Float).SetInt(t.LPTotalSupply)
	pct, _ := new(big.Float).Quo(burned, total).Float64()
	return pct * 100, true
}

// FetchTokenState gathers everything the filter chain needs.
//
// It costs two batched round trips: one to locate the pool across candidate
// venues, one to read the state of whichever pool won. Reads are against the
// node's "latest", which is necessarily behind the feed block we are reacting
// to — that is the correct pre-trade view, not a staleness bug.
func (c *Client) FetchTokenState(ctx context.Context, token common.Address) (TokenState, error) {
	state := TokenState{Token: token}

	pool, err := c.findDeepestPool(ctx, token)
	if err != nil {
		return state, err
	}
	state.Pool = pool

	// Token metadata plus, for V2, the LP burn accounting.
	calls := []rpc.BatchElem{
		callElem(token, selDecimals),
		callElem(token, selTotalSupply),
		callElem(token, selOwner),
	}
	if pool != nil && pool.Venue == "uniswap-v2" {
		calls = append(calls,
			callElem(pool.Address, selTotalSupply),
			callElem(pool.Address, selBalanceOf+padAddr(BurnAddresses[0])),
			callElem(pool.Address, selBalanceOf+padAddr(BurnAddresses[1])),
		)
	}
	if err := c.rpc.BatchCallContext(ctx, calls); err != nil {
		return state, err
	}

	if d := resultBig(calls[0]); d != nil && d.IsUint64() && d.Uint64() <= 255 {
		state.Decimals = uint8(d.Uint64())
	} else {
		state.Decimals = 18 // ponytail: overwhelmingly the real answer
	}
	state.Supply = resultBig(calls[1])
	if o := resultBig(calls[2]); o != nil {
		state.Owner = common.BigToAddress(o)
	}
	if len(calls) == 6 {
		state.LPTotalSupply = resultBig(calls[3])
		burned := big.NewInt(0)
		for _, i := range []int{4, 5} {
			if v := resultBig(calls[i]); v != nil {
				burned.Add(burned, v)
			}
		}
		state.LPBurned = burned
	}
	return state, nil
}

// findDeepestPool locates the WETH pool with the most WETH in it, checking the
// V2 pair and every V3 fee tier in one batch each.
func (c *Client) findDeepestPool(ctx context.Context, token common.Address) (*Pool, error) {
	lookups := []rpc.BatchElem{
		callElem(V2Factory, selGetPairV2+padAddr(token)+padAddr(WETH)),
	}
	for _, fee := range V3FeeTiers {
		lookups = append(lookups, callElem(V3Factory,
			selGetPoolV3+padAddr(token)+padAddr(WETH)+padUint(fee)))
	}
	if err := c.rpc.BatchCallContext(ctx, lookups); err != nil {
		return nil, err
	}

	type candidate struct {
		addr  common.Address
		venue string
		fee   uint32
	}
	var cands []candidate
	if a := resultAddr(lookups[0]); a != (common.Address{}) {
		cands = append(cands, candidate{a, "uniswap-v2", 0})
	}
	for i, fee := range V3FeeTiers {
		if a := resultAddr(lookups[i+1]); a != (common.Address{}) {
			cands = append(cands, candidate{a, "uniswap-v3", fee})
		}
	}
	// No early return when cands is empty: a token whose only liquidity is in
	// V4 has no V2 pair and no V3 pool, and returning here would make exactly
	// the case this code exists for invisible again.
	var best *Pool
	if len(cands) > 0 {
		// The WETH balance of the pool contract is the honest depth measure for
		// both venues: it is what is actually there to sell into.
		balCalls := make([]rpc.BatchElem, len(cands))
		for i, cd := range cands {
			balCalls[i] = callElem(WETH, selBalanceOf+padAddr(cd.addr))
		}
		if err := c.rpc.BatchCallContext(ctx, balCalls); err != nil {
			return nil, err
		}
		for i, cd := range cands {
			bal := resultBig(balCalls[i])
			if bal == nil {
				continue
			}
			if best == nil || bal.Cmp(best.WETHLiquidity) > 0 {
				best = &Pool{Address: cd.addr, Venue: cd.venue, FeeTier: cd.fee, WETHLiquidity: bal}
			}
		}
	}

	// V4 is a singleton, so it cannot be discovered by asking a factory for a
	// pool address. Skipping it made the filter measure a dormant V3 dust pool
	// while the real liquidity sat in the PoolManager — see internal/chain/v4.go.
	if v4 := c.deepestV4Pool(ctx, token); v4 != nil {
		if best == nil || v4.WETHLiquidity.Cmp(best.WETHLiquidity) > 0 {
			best = v4
		}
	}
	return best, nil
}

// deepestV4Pool returns the deepest V4 pool for a token, or nil.
//
// Errors are swallowed deliberately: V4 is additive here, so a failed lookup
// should leave the V2/V3 answer standing rather than fail the whole read. It
// cannot mask a problem, because a V4-only token still ends up with no pool at
// all and is rejected on that basis.
func (c *Client) deepestV4Pool(ctx context.Context, token common.Address) *Pool {
	pools, err := c.findV4Pools(ctx, token)
	if err != nil || len(pools) == 0 {
		return nil
	}
	var best *Pool
	for _, p := range pools {
		depth, err := c.v4Depth(ctx, p)
		if err != nil || depth == nil {
			continue
		}
		// A zero-depth pool is still returned. Concentrated liquidity can leave
		// a pool with no active liquidity at the current price, and that must be
		// rejected as thin — not reported as "no pool found on any venue", which
		// would wrongly suggest the token could not be seen at all.
		if best == nil || depth.Cmp(best.WETHLiquidity) > 0 {
			best = &Pool{
				Address:       common.BytesToAddress(p.ID.Bytes()[12:]),
				Venue:         "uniswap-v4",
				FeeTier:       p.Fee,
				WETHLiquidity: depth,
				V4PoolID:      p.ID,
				V4Currency0:   p.Currency0,
				V4Currency1:   p.Currency1,
			}
		}
	}
	return best
}

// callElem builds one eth_call batch element against "latest".
func callElem(to common.Address, data string) rpc.BatchElem {
	out := new(string)
	return rpc.BatchElem{
		Method: "eth_call",
		Args: []any{
			map[string]any{"to": to.Hex(), "data": data},
			"latest",
		},
		Result: out,
	}
}

// resultBig decodes a batch result as an integer, or nil if the call reverted.
// A reverted read is normal — plenty of tokens omit owner() or decimals().
func resultBig(e rpc.BatchElem) *big.Int {
	if e.Error != nil {
		return nil
	}
	s, ok := e.Result.(*string)
	if !ok || s == nil || *s == "" || *s == "0x" {
		return nil
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(*s, "0x"), 16)
	if !ok {
		return nil
	}
	return v
}

func resultAddr(e rpc.BatchElem) common.Address {
	v := resultBig(e)
	if v == nil {
		return common.Address{}
	}
	return common.BigToAddress(v)
}

func padAddr(a common.Address) string {
	return fmt.Sprintf("%064x", a.Big())
}

func padUint(v uint32) string {
	return fmt.Sprintf("%064x", v)
}
