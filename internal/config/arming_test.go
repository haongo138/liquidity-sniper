package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the properties that stand between a typo and a drained wallet.

const baseCfg = `
feed: testnet
rpc: https://example.invalid
chain_id: 46630
watch:
  - "0x85b605b47a5323912615cb8Af834BB1c4716b794"
trade_size_eth: 0.01
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Arming execution must not start spending on the strength of an unset field.
func TestDryRunDefaultsTrue(t *testing.T) {
	c, err := Load(write(t, baseCfg+`
live: true
max_trade_eth: 0.05
daily_loss_limit_eth: 0.25
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.IsDryRun() {
		t.Fatal("dry_run defaulted to false — arming would broadcast immediately")
	}
	if !c.ExecConfig().DryRun {
		t.Fatal("executor config did not inherit the dry-run default")
	}
}

func TestDryRunCanBeTurnedOffExplicitly(t *testing.T) {
	c, err := Load(write(t, baseCfg+`
live: true
dry_run: false
max_trade_eth: 0.05
daily_loss_limit_eth: 0.25
exits:
  max_hold_seconds: 600
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.IsDryRun() {
		t.Fatal("explicit dry_run: false was ignored")
	}
}

// An armed bot with every exit disabled opens positions it can never close.
func TestLiveRequiresAnExitTrigger(t *testing.T) {
	_, err := Load(write(t, baseCfg+`
live: true
dry_run: false
max_trade_eth: 0.05
daily_loss_limit_eth: 0.25
exits:
  take_profit_pct: 0
  stop_loss_pct: 0
  max_hold_seconds: 0
  follow_kol_sell: false
`))
	if err == nil {
		t.Fatal("live execution accepted with no exit trigger at all")
	}
	if !strings.Contains(err.Error(), "exit trigger") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

// The caps exist to bound a bug, so they must be mandatory when live.
func TestLiveRequiresCaps(t *testing.T) {
	for _, missing := range []string{
		"live: true\ndaily_loss_limit_eth: 0.25\n", // no max_trade_eth
		"live: true\nmax_trade_eth: 0.05\n",        // no daily_loss_limit_eth
	} {
		if _, err := Load(write(t, baseCfg+missing)); err == nil {
			t.Errorf("live accepted without a cap:\n%s", missing)
		}
	}
}

func TestTradeSizeCannotExceedCap(t *testing.T) {
	_, err := Load(write(t, `
feed: testnet
rpc: https://example.invalid
chain_id: 46630
watch:
  - "0x85b605b47a5323912615cb8Af834BB1c4716b794"
trade_size_eth: 1.0
live: true
max_trade_eth: 0.05
daily_loss_limit_eth: 0.25
`))
	if err == nil {
		t.Fatal("trade_size_eth above max_trade_eth was accepted")
	}
}

// A private key in the config file would end up in git. The loader uses
// KnownFields, so any such key is a hard parse error rather than an ignored
// field that lulls the operator into thinking it was used.
func TestPrivateKeyInConfigIsRejected(t *testing.T) {
	for _, field := range []string{"private_key", "privateKey", "key", "secret"} {
		_, err := Load(write(t, baseCfg+field+`: "0xabc123"`+"\n"))
		if err == nil {
			t.Errorf("config field %q was silently accepted", field)
		}
	}
}

func TestShadowModeNeedsNoCaps(t *testing.T) {
	c, err := Load(write(t, baseCfg+"live: false\n"))
	if err != nil {
		t.Fatalf("shadow mode rejected: %v", err)
	}
	if c.Live {
		t.Error("live parsed as true")
	}
}

func TestExitRulesFillDefaults(t *testing.T) {
	c, err := Load(write(t, baseCfg+"live: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.ExitRules()
	if r.TakeProfitPct <= 0 || r.StopLossPct <= 0 || r.MaxHold <= 0 || !r.FollowKOLSell {
		t.Errorf("defaults left a trigger disabled: %+v", r)
	}
}

func TestSlippageBoundsValidated(t *testing.T) {
	for _, bad := range []string{"slippage_bps: -1\n", "slippage_bps: 10000\n"} {
		if _, err := Load(write(t, baseCfg+"live: false\n"+bad)); err == nil {
			t.Errorf("invalid %s accepted", strings.TrimSpace(bad))
		}
	}
}
