package decode

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Calldata arrives from the chain and is fully attacker-controlled: anyone can
// send a transaction to a router with a malformed body. Every read here is
// bounds-checked and every length is capped, so a hostile payload yields
// ErrNotASwap rather than a panic or a huge allocation in the hot path.

const wordSize = 32

// maxElements caps array lengths. Real swap paths are a handful of hops and
// real multicalls a handful of calls; anything larger is an allocation bomb.
const maxElements = 512

var errBadEncoding = errors.New("decode: malformed calldata")

// word returns the i-th 32-byte word.
func word(data []byte, i int) ([]byte, bool) {
	start := i * wordSize
	if start < 0 || start+wordSize > len(data) {
		return nil, false
	}
	return data[start : start+wordSize], true
}

// wordAddress reads the i-th word as an address (low 20 bytes).
func wordAddress(data []byte, i int) common.Address {
	w, ok := word(data, i)
	if !ok {
		return common.Address{}
	}
	return common.BytesToAddress(w[12:])
}

// wordBig reads the i-th word as an unsigned integer. A short read yields zero,
// which callers treat as "absent" rather than as a valid amount.
func wordBig(data []byte, i int) *big.Int {
	w, ok := word(data, i)
	if !ok {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(w)
}

// readOffset reads the i-th word as a byte offset into data.
func readOffset(data []byte, i int) (int, error) {
	w, ok := word(data, i)
	if !ok {
		return 0, errBadEncoding
	}
	v := new(big.Int).SetBytes(w)
	if !v.IsInt64() {
		return 0, errBadEncoding
	}
	n := v.Int64()
	if n < 0 || n > int64(len(data)) {
		return 0, errBadEncoding
	}
	return int(n), nil
}

// readLength reads a length prefix at off and checks it against maxElements.
func readLength(data []byte, off int) (int, error) {
	if off < 0 || off+wordSize > len(data) {
		return 0, errBadEncoding
	}
	v := new(big.Int).SetBytes(data[off : off+wordSize])
	if !v.IsInt64() {
		return 0, errBadEncoding
	}
	n := v.Int64()
	if n < 0 || n > maxElements {
		return 0, errBadEncoding
	}
	return int(n), nil
}

// readBytes reads a dynamic bytes value located at off.
func readBytes(data []byte, off int) ([]byte, error) {
	if off < 0 || off+wordSize > len(data) {
		return nil, errBadEncoding
	}
	v := new(big.Int).SetBytes(data[off : off+wordSize])
	if !v.IsInt64() {
		return nil, errBadEncoding
	}
	n := v.Int64()
	// Bound by the remaining buffer, not by maxElements: a V3 path is bytes,
	// not an element count.
	if n < 0 || int64(off)+wordSize+n > int64(len(data)) {
		return nil, errBadEncoding
	}
	return data[off+wordSize : off+wordSize+int(n)], nil
}

// readAddressArray reads an address[] located at off. Swap paths need at least
// two hops, so a shorter array is rejected as malformed.
func readAddressArray(data []byte, off int) ([]common.Address, error) {
	n, err := readLength(data, off)
	if err != nil {
		return nil, err
	}
	if n < 2 {
		return nil, errBadEncoding
	}
	base := off + wordSize
	if base+n*wordSize > len(data) {
		return nil, errBadEncoding
	}
	out := make([]common.Address, n)
	for i := range out {
		out[i] = common.BytesToAddress(data[base+i*wordSize+12 : base+(i+1)*wordSize])
	}
	return out, nil
}

// readBytesArray reads a bytes[] whose head offset sits at word index
// offsetWord. Element offsets are relative to the start of the array's data.
func readBytesArray(data []byte, offsetWord int) ([][]byte, error) {
	base, err := readOffset(data, offsetWord)
	if err != nil {
		return nil, err
	}
	n, err := readLength(data, base)
	if err != nil {
		return nil, err
	}
	body := base + wordSize
	if body+n*wordSize > len(data) {
		return nil, errBadEncoding
	}

	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		rel := new(big.Int).SetBytes(data[body+i*wordSize : body+(i+1)*wordSize])
		if !rel.IsInt64() {
			return nil, errBadEncoding
		}
		elem, err := readBytes(data, body+int(rel.Int64()))
		if err != nil {
			// One malformed element must not discard its siblings — the swap we
			// want may be any entry in the bundle.
			continue
		}
		out = append(out, elem)
	}
	if len(out) == 0 {
		return nil, errBadEncoding
	}
	return out, nil
}
