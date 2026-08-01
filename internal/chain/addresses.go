// Package chain holds verified on-chain constants for Robinhood Chain.
//
// Every address here was resolved empirically on 2026-07-31 and verified by
// calling the contract, never copied from a blog post. The provenance comment
// on each one records the call that proved it. A wrong router address produces
// zero detections and no error, which is the worst possible failure mode, so
// re-verify with `feedtap --discover` if detections ever go quiet.
package chain

import "github.com/ethereum/go-ethereum/common"

// MainnetChainID is Robinhood Chain mainnet (0x1237).
const MainnetChainID = 4663

// Core tokens and infrastructure.
var (
	// WETH: symbol() == "WETH", decimals() == 18.
	WETH = common.HexToAddress("0x0bd7d308f8e1639fab988df18a8011f41eacad73")

	// Permit2, canonical address, confirmed deployed.
	Permit2 = common.HexToAddress("0x000000000022D473030F116dDEE9F6B43aC78BA3")
)

// Uniswap V2. The long tail of permissionless tokens lives here: the factory
// reported allPairsLength() == 26,856. V2 is the venue where "LP burnt or
// locked" is a meaningful check, because V2 LP positions are fungible ERC-20s.
var (
	// V2Router: factory() and WETH() both resolve.
	V2Router = common.HexToAddress("0x89e5DB8B5aA49aA85AC63f691524311AEB649eba")

	// V2Factory: reached via V2Router.factory(); allPairsLength() == 26856.
	V2Factory = common.HexToAddress("0x8bceaa40b9acdfaedf85adf4ff01f5ad6517937f")
)

// Uniswap V3. Highest call volume on the chain by a wide margin.
//
// Note for filter design: V3 has no fungible LP token, so "LP burnt or locked"
// does not apply. Liquidity is held as NonfungiblePositionManager NFTs.
var (
	// V3Router is SwapRouter02: WETH9() and positionManager() resolve, and
	// factoryV2() reverts. Carried ~3,400 of 13,400 keyed calls in a 75s sample.
	V3Router = common.HexToAddress("0xCaf681a66D020601342297493863E78C959E5cb2")

	// V3Factory: reached via V3Router.factory();
	// feeAmountTickSpacing(3000) == 60, which is the canonical V3 mapping.
	V3Factory = common.HexToAddress("0x1f7d7550b1b028f7571e69a784071f0205fd2efa")

	// V3PositionManager: reached via V3Router.positionManager().
	V3PositionManager = common.HexToAddress("0x73991a25c818bf1f1128deaab1492d45638de0d3")
)

// Uniswap V4 / UniversalRouter.
var (
	// UniversalRouter: poolManager() resolves. Lower volume than SwapRouter02
	// (~100 vs ~3,400 calls in the same sample) but it is the path the Uniswap
	// front-end uses, so KOL flow from the web app arrives here.
	UniversalRouter = common.HexToAddress("0x8876789976dEcBfCbBbe364623C63652db8C0904")

	// V4PoolManager: reached via UniversalRouter.poolManager().
	V4PoolManager = common.HexToAddress("0x8366a39cc670b4001a1121b8f6a443a643e40951")
)

// BurnAddresses are the sinks that count as "LP burnt". Sending LP tokens here
// is irreversible, which is the property the filter actually cares about.
var BurnAddresses = []common.Address{
	common.HexToAddress("0x0000000000000000000000000000000000000000"),
	common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
}

// KnownRouters maps each router we can decode to its venue.
var KnownRouters = map[common.Address]string{
	V2Router:        "uniswap-v2",
	V3Router:        "uniswap-v3",
	UniversalRouter: "universal-router",
}

// IsKnownRouter reports whether addr is a router this tool can decode.
func IsKnownRouter(addr common.Address) bool {
	_, ok := KnownRouters[addr]
	return ok
}
