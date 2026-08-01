package chain

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// A naive implementation re-reads everything on every detection, which costs
// three sequential round trips — measured at ~780ms against a 250ms-RTT node,
// more than the whole latency edge the strategy depends on.
//
// The split below fixes that by separating what changes from what does not:
//
//   - Profile (pool address, venue, decimals, supply, owner, LP totals) is
//     effectively static, so it is cached and costs nothing on a repeat token.
//     Watched wallets trade the same handful of tokens over and over, so the
//     hit rate is high in practice.
//   - Liquidity moves with every trade and is never cached. It costs exactly
//     one call, always fresh.
//
// Net: a repeat token costs one round trip instead of three, and the
// min-liquidity check is still evaluated against current state.

// profileTTL bounds staleness of the cached half. A pool can be created or
// migrated, so the cache expires rather than living forever.
const profileTTL = 10 * time.Minute

// Profile is the slow-moving half of a token's state.
type Profile struct {
	Token    common.Address
	Decimals uint8
	Supply   *big.Int
	Owner    common.Address

	// Pool is nil when the token has no WETH pool on any venue.
	Pool *Pool

	// LP accounting, V2 only. Nil on V3.
	LPTotalSupply *big.Int
	LPBurned      *big.Int

	fetchedAt time.Time
}

// Cache memoises token profiles.
type Cache struct {
	mu      sync.RWMutex
	entries map[common.Address]Profile
	hits    int
	misses  int
}

// NewCache builds an empty cache.
func NewCache() *Cache {
	return &Cache{entries: map[common.Address]Profile{}}
}

// Stats reports cache effectiveness, which is the number to watch if detection
// latency regresses.
func (c *Cache) Stats() (hits, misses int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses
}

func (c *Cache) get(token common.Address, now time.Time) (Profile, bool) {
	c.mu.RLock()
	p, ok := c.entries[token]
	c.mu.RUnlock()
	if ok && now.Sub(p.fetchedAt) < profileTTL {
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return p, true
	}
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()
	return Profile{}, false
}

func (c *Cache) put(p Profile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[p.Token] = p
}

// FetchState returns a token snapshot, using the cached profile when possible
// and always reading liquidity fresh.
func (c *Client) FetchState(ctx context.Context, cache *Cache, token common.Address) (TokenState, error) {
	now := time.Now()

	profile, cached := cache.get(token, now)
	if !cached {
		p, err := c.fetchProfile(ctx, token)
		if err != nil {
			return TokenState{Token: token}, err
		}
		p.fetchedAt = now
		cache.put(p)
		profile = p
	}

	state := TokenState{
		Token:         profile.Token,
		Decimals:      profile.Decimals,
		Supply:        profile.Supply,
		Owner:         profile.Owner,
		LPTotalSupply: profile.LPTotalSupply,
		LPBurned:      profile.LPBurned,
	}
	if profile.Pool == nil {
		return state, nil
	}

	// Always fresh: one call, and the only figure that moves trade to trade.
	liq, err := c.wethBalance(ctx, profile.Pool.Address)
	if err != nil {
		return state, err
	}
	state.Pool = &Pool{
		Address:       profile.Pool.Address,
		Venue:         profile.Pool.Venue,
		FeeTier:       profile.Pool.FeeTier,
		WETHLiquidity: liq,
	}
	return state, nil
}

// fetchProfile does the expensive discovery, run once per token per TTL.
func (c *Client) fetchProfile(ctx context.Context, token common.Address) (Profile, error) {
	p := Profile{Token: token, Decimals: 18}

	pool, err := c.findDeepestPool(ctx, token)
	if err != nil {
		return p, err
	}
	p.Pool = pool

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
		return p, err
	}

	if d := resultBig(calls[0]); d != nil && d.IsUint64() && d.Uint64() <= 255 {
		p.Decimals = uint8(d.Uint64())
	}
	p.Supply = resultBig(calls[1])
	if o := resultBig(calls[2]); o != nil {
		p.Owner = common.BigToAddress(o)
	}
	if len(calls) == 6 {
		p.LPTotalSupply = resultBig(calls[3])
		burned := big.NewInt(0)
		for _, i := range []int{4, 5} {
			if v := resultBig(calls[i]); v != nil {
				burned.Add(burned, v)
			}
		}
		p.LPBurned = burned
	}
	return p, nil
}

// wethBalance reads the WETH held by a pool in one call.
func (c *Client) wethBalance(ctx context.Context, pool common.Address) (*big.Int, error) {
	var out string
	err := c.rpc.CallContext(ctx, &out, "eth_call",
		map[string]any{"to": WETH.Hex(), "data": selBalanceOf + padAddr(pool)}, "latest")
	if err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(trimHex(out), 16)
	if !ok {
		return big.NewInt(0), nil
	}
	return v, nil
}

func trimHex(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}
