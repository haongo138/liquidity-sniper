// Package exec builds, simulates, signs and submits swaps.
//
// Every trade is simulated with eth_call before it is signed. A simulation that
// reverts is not sent, which costs one round trip and buys three things: it
// catches honeypots (a sell that cannot execute), it catches slippage floors
// that would fail anyway, and it means a broken encoding wastes no gas.
//
// Three deliberate refusals, each of which would otherwise be a silent way to
// lose money:
//
//   - We set our OWN slippage floor. Watched wallets routinely send
//     amountOutMinimum=0, betting on speed. We land behind them by design, so
//     copying that hands a free option to anyone in between.
//   - We do not trade V4-only tokens. Their depth is now measured correctly,
//     but encoding a V4 swap goes through UniversalRouter's command buffer and
//     is not implemented. Refusing is honest; mis-encoding is not.
//   - We never broadcast in dry-run mode, which is the default the first time
//     execution is armed.
package exec

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/antono/hoodsniper/internal/chain"
	"github.com/antono/hoodsniper/internal/wallet"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// Selectors we encode. Only SwapRouter02's exactInputSingle is used for trading;
// the ERC-20 pair is for the approval a sell needs.
const (
	selExactInputSingle = "04e45aaf" // exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))
	selApprove          = "095ea7b3" // approve(address,uint256)
	selAllowance        = "dd62ed3e" // allowance(address,address)
	selBalanceOf        = "70a08231" // balanceOf(address)
)

// gasLimitPad multiplies the estimate. Swaps through unfamiliar tokens vary
// with transfer hooks and fee-on-transfer logic, and an out-of-gas revert costs
// the full limit anyway.
const gasLimitPad = 130 // percent

// Config controls execution.
type Config struct {
	// DryRun builds, signs and simulates but never broadcasts.
	DryRun bool
	// SlippageBps is our own floor, in basis points. Never taken from the
	// wallet being copied.
	SlippageBps int64
	// MaxTradeWei is a hard per-trade ceiling.
	MaxTradeWei *big.Int
	// DailyLossLimitWei trips the kill switch.
	DailyLossLimitWei *big.Int
	// Deadline is how long a submitted swap stays valid.
	Deadline time.Duration
}

// Result describes what happened.
type Result struct {
	Simulated bool
	Sent      bool
	Hash      common.Hash
	AmountIn  *big.Int
	MinOut    *big.Int
	GasUsed   uint64
	// Voided reports an ArbOS-61 compliance filter kill: included in a block
	// with status 0x0, no logs, and the gas fully burned.
	Voided bool
	Reason string
}

// Executor submits swaps.
type Executor struct {
	rpc    *rpc.Client
	signer *wallet.Signer
	cfg    Config

	mu       sync.Mutex
	nonce    uint64
	nonceSet bool
	// realisedLossWei accumulates within the day for the kill switch.
	realisedLossWei *big.Int
	halted          bool
	haltReason      string
}

// New builds an Executor.
func New(rpcClient *rpc.Client, signer *wallet.Signer, cfg Config) *Executor {
	if cfg.Deadline == 0 {
		cfg.Deadline = 60 * time.Second
	}
	return &Executor{
		rpc: rpcClient, signer: signer, cfg: cfg,
		realisedLossWei: big.NewInt(0),
	}
}

// Halted reports whether the kill switch has tripped.
func (e *Executor) Halted() (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.halted, e.haltReason
}

// RecordPnL accumulates realised profit or loss and trips the kill switch when
// the daily limit is breached. Halting is one-way for the process lifetime:
// re-arming after a loss streak should be a human decision.
func (e *Executor) RecordPnL(deltaWei *big.Int) {
	if deltaWei == nil || e.cfg.DailyLossLimitWei == nil ||
		e.cfg.DailyLossLimitWei.Sign() == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if deltaWei.Sign() < 0 {
		e.realisedLossWei.Add(e.realisedLossWei, new(big.Int).Neg(deltaWei))
	}
	if e.realisedLossWei.Cmp(e.cfg.DailyLossLimitWei) >= 0 && !e.halted {
		e.halted = true
		e.haltReason = fmt.Sprintf("daily loss limit reached: %s wei lost, limit %s",
			e.realisedLossWei, e.cfg.DailyLossLimitWei)
	}
}

// Buy swaps WETH for a token.
func (e *Executor) Buy(ctx context.Context, pool *chain.Pool, token common.Address,
	amountInWei *big.Int, expectedOut *big.Int) (Result, error) {
	return e.swap(ctx, pool, chain.WETH, token, amountInWei, expectedOut)
}

// Sell swaps a token back to WETH. It ensures the router is approved first.
func (e *Executor) Sell(ctx context.Context, pool *chain.Pool, token common.Address,
	amountTokens *big.Int, expectedOut *big.Int) (Result, error) {
	if err := e.ensureApproval(ctx, token); err != nil {
		return Result{Reason: "approval failed"}, err
	}
	return e.swap(ctx, pool, token, chain.WETH, amountTokens, expectedOut)
}

// swap is the shared path for both directions.
func (e *Executor) swap(ctx context.Context, pool *chain.Pool,
	tokenIn, tokenOut common.Address, amountIn, expectedOut *big.Int) (Result, error) {

	if halted, why := e.Halted(); halted {
		return Result{Reason: "halted: " + why}, fmt.Errorf("execution halted: %s", why)
	}
	if pool == nil {
		return Result{Reason: "no pool"}, fmt.Errorf("no pool for %s", tokenOut)
	}
	// V4 depth is measured but V4 encoding is not implemented. Routing a V4-only
	// token through SwapRouter02 would revert at best and mis-fill at worst.
	if pool.Venue == "uniswap-v4" {
		return Result{Reason: "v4 execution not implemented"},
			fmt.Errorf("token trades on uniswap-v4; execution supports v2/v3 only")
	}
	if amountIn == nil || amountIn.Sign() <= 0 {
		return Result{Reason: "zero amount"}, fmt.Errorf("amount must be positive")
	}
	if e.cfg.MaxTradeWei != nil && e.cfg.MaxTradeWei.Sign() > 0 &&
		tokenIn == chain.WETH && amountIn.Cmp(e.cfg.MaxTradeWei) > 0 {
		return Result{Reason: "exceeds max_trade_eth"},
			fmt.Errorf("amount %s exceeds max_trade_eth %s", amountIn, e.cfg.MaxTradeWei)
	}

	minOut := e.minOut(expectedOut)
	data := encodeExactInputSingle(tokenIn, tokenOut, pool.FeeTier,
		e.signer.Address(), amountIn, minOut)

	res := Result{AmountIn: new(big.Int).Set(amountIn), MinOut: minOut}

	// Simulate first. A revert here is the cheapest possible failure and is how
	// honeypots are caught: a sell that cannot execute reverts.
	if err := e.simulate(ctx, chain.V3Router, data); err != nil {
		res.Reason = "simulation reverted: " + err.Error()
		return res, fmt.Errorf("simulation reverted (not sent): %w", err)
	}
	res.Simulated = true

	tx, err := e.buildTx(ctx, chain.V3Router, data)
	if err != nil {
		res.Reason = "build failed"
		return res, err
	}
	signed, err := e.signer.SignTx(tx)
	if err != nil {
		res.Reason = "signing failed"
		return res, err
	}
	res.Hash = signed.Hash()

	if e.cfg.DryRun {
		res.Reason = "dry run: simulated and signed, not broadcast"
		return res, nil
	}

	raw, err := signed.MarshalBinary()
	if err != nil {
		res.Reason = "encoding failed"
		return res, err
	}
	var sent string
	if err := e.rpc.CallContext(ctx, &sent, "eth_sendRawTransaction",
		"0x"+common.Bytes2Hex(raw)); err != nil {
		e.resetNonce()
		res.Reason = "send failed"
		return res, fmt.Errorf("sending: %w", err)
	}
	e.advanceNonce()
	res.Sent = true
	res.Reason = "submitted"
	return res, nil
}

// minOut applies our slippage floor. A zero floor is refused outright: it is
// what the copied wallets send, and it is exactly wrong for a backrunner who
// arrives after the price has already moved.
func (e *Executor) minOut(expectedOut *big.Int) *big.Int {
	if expectedOut == nil || expectedOut.Sign() <= 0 {
		return big.NewInt(1) // never zero
	}
	bps := e.cfg.SlippageBps
	if bps <= 0 || bps >= 10000 {
		bps = 500 // 5% default rather than an unprotected order
	}
	keep := new(big.Int).Sub(big.NewInt(10000), big.NewInt(bps))
	out := new(big.Int).Div(new(big.Int).Mul(expectedOut, keep), big.NewInt(10000))
	if out.Sign() <= 0 {
		return big.NewInt(1)
	}
	return out
}

// simulate runs the call without state changes.
func (e *Executor) simulate(ctx context.Context, to common.Address, data string) error {
	var out string
	err := e.rpc.CallContext(ctx, &out, "eth_call", map[string]any{
		"from": e.signer.Address().Hex(),
		"to":   to.Hex(),
		"data": data,
	}, "latest")
	return err
}

// buildTx assembles a dynamic-fee transaction with a fresh nonce and gas.
func (e *Executor) buildTx(ctx context.Context, to common.Address, data string) (*types.Transaction, error) {
	nonce, err := e.nextNonce(ctx)
	if err != nil {
		return nil, err
	}
	gas, err := e.estimateGas(ctx, to, data)
	if err != nil {
		return nil, err
	}
	tip, feeCap, err := e.gasPrice(ctx)
	if err != nil {
		return nil, err
	}
	return types.NewTx(&types.DynamicFeeTx{
		ChainID:   e.signer.ChainID(),
		Nonce:     nonce,
		To:        &to,
		Gas:       gas,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Data:      common.FromHex(data),
	}), nil
}

func (e *Executor) estimateGas(ctx context.Context, to common.Address, data string) (uint64, error) {
	var out string
	err := e.rpc.CallContext(ctx, &out, "eth_estimateGas", map[string]any{
		"from": e.signer.Address().Hex(),
		"to":   to.Hex(),
		"data": data,
	})
	if err != nil {
		return 0, fmt.Errorf("estimating gas: %w", err)
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("undecodable gas estimate %q", out)
	}
	v.Mul(v, big.NewInt(gasLimitPad)).Div(v, big.NewInt(100))
	return v.Uint64(), nil
}

func (e *Executor) gasPrice(ctx context.Context) (tip, feeCap *big.Int, err error) {
	var out string
	if err := e.rpc.CallContext(ctx, &out, "eth_gasPrice"); err != nil {
		return nil, nil, fmt.Errorf("gas price: %w", err)
	}
	base, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16)
	if !ok {
		return nil, nil, fmt.Errorf("undecodable gas price %q", out)
	}
	// The sequencer orders by arrival, not by fee, so bidding up buys nothing.
	// The cap exists only to survive a base-fee move between build and inclusion.
	tip = big.NewInt(0)
	feeCap = new(big.Int).Mul(base, big.NewInt(2))
	return tip, feeCap, nil
}

// nextNonce returns the nonce to use, fetching once and then tracking locally.
// Refetching per transaction would race our own pending sends.
func (e *Executor) nextNonce(ctx context.Context) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nonceSet {
		return e.nonce, nil
	}
	var out string
	if err := e.rpc.CallContext(ctx, &out, "eth_getTransactionCount",
		e.signer.Address().Hex(), "pending"); err != nil {
		return 0, fmt.Errorf("fetching nonce: %w", err)
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("undecodable nonce %q", out)
	}
	e.nonce = v.Uint64()
	e.nonceSet = true
	return e.nonce, nil
}

func (e *Executor) advanceNonce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nonce++
}

// resetNonce forces a refetch after a failed send, since the local counter may
// have drifted from the chain.
func (e *Executor) resetNonce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nonceSet = false
}

// ensureApproval grants the router an allowance if it lacks one.
func (e *Executor) ensureApproval(ctx context.Context, token common.Address) error {
	var out string
	err := e.rpc.CallContext(ctx, &out, "eth_call", map[string]any{
		"to": token.Hex(),
		"data": "0x" + selAllowance + pad(e.signer.Address().Bytes()) +
			pad(chain.V3Router.Bytes()),
	}, "latest")
	if err == nil && out != "" && out != "0x" {
		if v, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16); ok && v.Sign() > 0 {
			return nil
		}
	}
	if e.cfg.DryRun {
		return nil // nothing to approve when nothing will be sent
	}

	// Approve exactly once, for the maximum, rather than per trade: an approval
	// transaction inside the hot path would cost a second round trip at the
	// worst possible moment.
	maxUint := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	data := "0x" + selApprove + pad(chain.V3Router.Bytes()) + padBig(maxUint)
	tx, err := e.buildTx(ctx, token, data)
	if err != nil {
		return err
	}
	signed, err := e.signer.SignTx(tx)
	if err != nil {
		return err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return err
	}
	var sent string
	if err := e.rpc.CallContext(ctx, &sent, "eth_sendRawTransaction",
		"0x"+common.Bytes2Hex(raw)); err != nil {
		e.resetNonce()
		return fmt.Errorf("approving %s: %w", token, err)
	}
	e.advanceNonce()
	return nil
}

// encodeExactInputSingle builds SwapRouter02 calldata. Every member of the
// params tuple is static, so it encodes inline with no head offset.
func encodeExactInputSingle(tokenIn, tokenOut common.Address, fee uint32,
	recipient common.Address, amountIn, minOut *big.Int) string {

	var b strings.Builder
	b.WriteString("0x")
	b.WriteString(selExactInputSingle)
	b.WriteString(pad(tokenIn.Bytes()))
	b.WriteString(pad(tokenOut.Bytes()))
	b.WriteString(fmt.Sprintf("%064x", fee))
	b.WriteString(pad(recipient.Bytes()))
	b.WriteString(padBig(amountIn))
	b.WriteString(padBig(minOut))
	b.WriteString(fmt.Sprintf("%064x", 0)) // sqrtPriceLimitX96: no limit
	return b.String()
}

func pad(b []byte) string { return fmt.Sprintf("%064x", new(big.Int).SetBytes(b)) }
func padBig(v *big.Int) string {
	if v == nil {
		return fmt.Sprintf("%064x", 0)
	}
	return fmt.Sprintf("%064x", v)
}

// QuoteSell reports what a position would fetch right now, by simulating the
// sell rather than deriving it from pool maths.
//
// A simulation is the honest answer to "what could I get out": it accounts for
// fee-on-transfer, transfer hooks and concentrated-liquidity range effects that
// a price calculation misses. It returning an error is itself a signal — a sell
// that cannot execute is a honeypot, which is the single most valuable thing to
// learn about a token we are holding.
func (e *Executor) QuoteSell(ctx context.Context, pool *chain.Pool,
	token common.Address, amountTokens *big.Int) (*big.Int, error) {

	if pool == nil || amountTokens == nil || amountTokens.Sign() <= 0 {
		return nil, fmt.Errorf("nothing to quote")
	}
	if pool.Venue == "uniswap-v4" {
		return nil, fmt.Errorf("v4 quoting not implemented")
	}
	// minOut of 1 so the quote is not rejected by its own slippage floor.
	data := encodeExactInputSingle(token, chain.WETH, pool.FeeTier,
		e.signer.Address(), amountTokens, big.NewInt(1))

	var out string
	if err := e.rpc.CallContext(ctx, &out, "eth_call", map[string]any{
		"from": e.signer.Address().Hex(),
		"to":   chain.V3Router.Hex(),
		"data": data,
	}, "latest"); err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("undecodable quote %q", out)
	}
	return v, nil
}
