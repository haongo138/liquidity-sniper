// Package config loads and validates the daemon's YAML configuration.
//
// Validation is strict and happens once at startup: a bad threshold should stop
// the process immediately, not surface as a silently-approved trade an hour in.
package config

import (
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/antono/hoodsniper/internal/filter"
	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	// Feed is "mainnet", "testnet", or a ws:// URL. Prefer a local relay.
	Feed string `yaml:"feed"`
	// RPC is the node used for state reads.
	RPC string `yaml:"rpc"`
	// ChainID guards against pointing at the wrong network.
	ChainID int64 `yaml:"chain_id"`

	// Watch lists the KOL wallets to copy.
	Watch []string `yaml:"watch"`

	// TradeSizeETH is what we would spend per copied buy.
	TradeSizeETH float64 `yaml:"trade_size_eth"`

	Filters FilterConfig `yaml:"filters"`

	// ShadowLog is the JSONL path for shadow-mode decisions.
	ShadowLog string `yaml:"shadow_log"`
	// Live arms real execution. Absent or false means shadow mode.
	Live bool `yaml:"live"`

	// MaxTradeETH is a hard per-trade ceiling, enforced even when Live.
	MaxTradeETH float64 `yaml:"max_trade_eth"`
	// DailyLossLimitETH trips the kill switch.
	DailyLossLimitETH float64 `yaml:"daily_loss_limit_eth"`
}

// FilterConfig mirrors the filter thresholds in YAML-friendly units.
type FilterConfig struct {
	MinLiquidityETH  float64  `yaml:"min_liquidity_eth"`
	MaxLiquidityETH  float64  `yaml:"max_liquidity_eth"`
	RequireLPSecured bool     `yaml:"require_lp_secured"`
	MinLPBurnedPct   float64  `yaml:"min_lp_burned_pct"`
	RequireRenounced bool     `yaml:"require_renounced"`
	MinTradeETH      float64  `yaml:"min_trade_eth"`
	AllowSells       bool     `yaml:"allow_sells"`
	Blocklist        []string `yaml:"token_blocklist"`
	Allowlist        []string `yaml:"token_allowlist"`
}

// Load reads and validates a config file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a typo'd key must fail loudly, not be ignored
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.RPC == "" {
		return fmt.Errorf("rpc is required")
	}
	if c.Feed == "" {
		return fmt.Errorf("feed is required")
	}
	if c.ChainID == 0 {
		return fmt.Errorf("chain_id is required")
	}
	if len(c.Watch) == 0 {
		return fmt.Errorf("watch must list at least one wallet")
	}
	for _, w := range c.Watch {
		if !common.IsHexAddress(w) {
			return fmt.Errorf("watch: %q is not an address", w)
		}
	}
	for _, t := range append(append([]string{}, c.Filters.Blocklist...), c.Filters.Allowlist...) {
		if !common.IsHexAddress(t) {
			return fmt.Errorf("token list: %q is not an address", t)
		}
	}
	if c.TradeSizeETH <= 0 {
		return fmt.Errorf("trade_size_eth must be positive")
	}
	// The ceiling exists to bound a bug, so it must be set whenever real money
	// is at stake and must actually bind.
	if c.Live {
		if c.MaxTradeETH <= 0 {
			return fmt.Errorf("max_trade_eth must be set when live is true")
		}
		if c.TradeSizeETH > c.MaxTradeETH {
			return fmt.Errorf("trade_size_eth %.4f exceeds max_trade_eth %.4f",
				c.TradeSizeETH, c.MaxTradeETH)
		}
		if c.DailyLossLimitETH <= 0 {
			return fmt.Errorf("daily_loss_limit_eth must be set when live is true")
		}
	}
	if c.Filters.MinLPBurnedPct < 0 || c.Filters.MinLPBurnedPct > 100 {
		return fmt.Errorf("min_lp_burned_pct must be within 0..100")
	}
	if c.Filters.MaxLiquidityETH > 0 && c.Filters.MaxLiquidityETH < c.Filters.MinLiquidityETH {
		return fmt.Errorf("max_liquidity_eth is below min_liquidity_eth")
	}
	if c.ShadowLog == "" {
		c.ShadowLog = "shadow.jsonl"
	}
	return nil
}

// WatchSet builds the wallet lookup used on the hot path.
func (c Config) WatchSet() map[common.Address]bool {
	out := make(map[common.Address]bool, len(c.Watch))
	for _, w := range c.Watch {
		out[common.HexToAddress(w)] = true
	}
	return out
}

// FilterConfig converts YAML units into the filter package's wei-based config.
func (c Config) FilterConfig() filter.Config {
	return filter.Config{
		MinLiquidityWei:  ethToWei(c.Filters.MinLiquidityETH),
		MaxLiquidityWei:  ethToWei(c.Filters.MaxLiquidityETH),
		RequireLPSecured: c.Filters.RequireLPSecured,
		MinLPBurnedPct:   c.Filters.MinLPBurnedPct,
		RequireRenounced: c.Filters.RequireRenounced,
		MinTradeWei:      ethToWei(c.Filters.MinTradeETH),
		AllowSells:       c.Filters.AllowSells,
		Blocklist:        addrSet(c.Filters.Blocklist),
		Allowlist:        addrSet(c.Filters.Allowlist),
	}
}

// TradeSizeWei is the configured trade size in wei.
func (c Config) TradeSizeWei() *big.Int { return ethToWei(c.TradeSizeETH) }

func addrSet(in []string) map[common.Address]bool {
	out := make(map[common.Address]bool, len(in))
	for _, s := range in {
		out[common.HexToAddress(s)] = true
	}
	return out
}

// ethToWei converts a float ETH amount to wei via big.Float, so a value like
// 0.1 does not accumulate binary float error on the way to an integer.
func ethToWei(eth float64) *big.Int {
	if eth <= 0 {
		return big.NewInt(0)
	}
	wei, _ := new(big.Float).Mul(big.NewFloat(eth), big.NewFloat(1e18)).Int(nil)
	return wei
}
