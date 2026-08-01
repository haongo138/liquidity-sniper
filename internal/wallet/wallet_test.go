package wallet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key that is never in the repo, used only to exercise the loader.
const testKey = "4c0883a69102937d6231471b5dbb6204fe512961708279e2b9f37b5f1e7b4f37"

func TestLoadDerivesAddress(t *testing.T) {
	t.Setenv(KeyEnvVar, testKey)
	s, err := Load(4663)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Address().Big().Sign() == 0 {
		t.Error("derived the zero address")
	}
	if s.ChainID().Int64() != 4663 {
		t.Errorf("chainID = %s, want 4663", s.ChainID())
	}
}

func TestLoadAcceptsHexPrefixAndWhitespace(t *testing.T) {
	t.Setenv(KeyEnvVar, "  0x"+testKey+"\n")
	if _, err := Load(4663); err != nil {
		t.Errorf("a prefixed, padded key was rejected: %v", err)
	}
}

// A world-readable key file is a compromised key. Continuing would be
// pretending otherwise.
func TestKeyFilePermissionsEnforced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte(testKey), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KeyEnvVar+"_FILE", p)

	_, err := Load(4663)
	if err == nil {
		t.Fatal("a group/world-readable key file was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

func TestKeyFileWithCorrectPermissionsLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KeyEnvVar+"_FILE", p)
	if _, err := Load(4663); err != nil {
		t.Errorf("a 0600 key file was rejected: %v", err)
	}
}

// No error path may echo the key back. An error string ends up in logs, in a
// terminal scrollback, and in a bug report.
func TestErrorsNeverContainTheKey(t *testing.T) {
	secret := "deadbeef" + strings.Repeat("a", 56)
	cases := []struct{ name, value string }{
		{"too short", "abc123"},
		{"not hex", strings.Repeat("z", 64)},
		{"full-length invalid", secret},
	}
	for _, tc := range cases {
		t.Setenv(KeyEnvVar, tc.value)
		_, err := Load(4663)
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s: error leaked the key material: %v", tc.name, err)
		}
	}
}

// A missing key must fail loudly and say where to put one — never fall back to
// some default or unsigned mode.
func TestMissingKeyIsAClearError(t *testing.T) {
	t.Setenv(KeyEnvVar, "")
	t.Setenv(KeyEnvVar+"_FILE", "")
	_, err := Load(4663)
	if err == nil {
		t.Fatal("a missing key was accepted")
	}
	for _, want := range []string{KeyEnvVar, "YAML"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestSignTxRequiresChainID(t *testing.T) {
	t.Setenv(KeyEnvVar, testKey)
	a, err := Load(4663)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(46630)
	if err != nil {
		t.Fatal(err)
	}
	// Same key, different chains: the signers must not be interchangeable, or a
	// transaction meant for testnet could be valid on mainnet.
	if a.ChainID().Cmp(b.ChainID()) == 0 {
		t.Error("chain IDs collapsed")
	}
	if a.Address() != b.Address() {
		t.Error("the same key derived different addresses")
	}
}
