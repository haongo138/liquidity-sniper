// Package wallet loads the signing key and signs transactions.
//
// The key never appears in the YAML config, in a log line, in an error message,
// or in the shadow ledger. It is read from the environment or from a file with
// owner-only permissions, and the only thing this package will print about it is
// the derived public address.
package wallet

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// KeyEnvVar is where the private key is read from.
const KeyEnvVar = "HOODSNIPER_PRIVATE_KEY"

// Signer holds the key and derived address.
type Signer struct {
	key     *ecdsa.PrivateKey
	address common.Address
	chainID *big.Int
	signer  types.Signer
}

// Address returns the public address. This is the only key-derived value that
// is safe to log.
func (s *Signer) Address() common.Address { return s.address }

// Load reads the key from HOODSNIPER_PRIVATE_KEY, or from the file named by
// HOODSNIPER_PRIVATE_KEY_FILE.
//
// The file variant is preferred: an environment variable is visible to anything
// that can read /proc or run `ps e`, while a 0600 file is not. The permission
// check is enforced rather than advised — a world-readable key file is a
// compromised key, and continuing would be pretending otherwise.
func Load(chainID int64) (*Signer, error) {
	raw, source, err := readKey()
	if err != nil {
		return nil, err
	}
	// Overwrite the decoded text as soon as it is parsed. It still exists in
	// the environment, but this bounds how long an extra copy lives.
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()

	hexKey := strings.TrimSpace(string(raw))
	hexKey = strings.TrimPrefix(hexKey, "0x")
	if len(hexKey) != 64 {
		return nil, fmt.Errorf("key from %s is %d hex chars, want 64 "+
			"(the key itself is never logged)", source, len(hexKey))
	}
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		// Deliberately does not wrap err: go-ethereum's message can echo the
		// input back.
		return nil, fmt.Errorf("key from %s is not a valid secp256k1 key", source)
	}

	return &Signer{
		key:     key,
		address: crypto.PubkeyToAddress(key.PublicKey),
		chainID: big.NewInt(chainID),
		signer:  types.LatestSignerForChainID(big.NewInt(chainID)),
	}, nil
}

func readKey() ([]byte, string, error) {
	if path := os.Getenv(KeyEnvVar + "_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, "", fmt.Errorf("reading %s_FILE: %w", KeyEnvVar, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, "", fmt.Errorf(
				"key file %s has permissions %04o — group or world readable. "+
					"Run: chmod 600 %s", path, perm, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("reading %s_FILE: %w", KeyEnvVar, err)
		}
		return b, KeyEnvVar + "_FILE", nil
	}

	if v := os.Getenv(KeyEnvVar); v != "" {
		return []byte(v), KeyEnvVar, nil
	}
	return nil, "", fmt.Errorf(
		"no signing key: set %s_FILE to a 0600 file (preferred) or %s. "+
			"The key must never be placed in the YAML config", KeyEnvVar, KeyEnvVar)
}

// SignTx signs a transaction for the configured chain.
func (s *Signer) SignTx(tx *types.Transaction) (*types.Transaction, error) {
	return types.SignTx(tx, s.signer, s.key)
}

// ChainID returns the chain this signer is bound to. Signing for the wrong
// chain would produce a transaction that is either rejected or, worse, valid
// somewhere it was not intended.
func (s *Signer) ChainID() *big.Int { return new(big.Int).Set(s.chainID) }
