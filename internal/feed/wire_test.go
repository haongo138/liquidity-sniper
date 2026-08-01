package feed

import (
	"encoding/binary"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const testChainID = MainnetChainID

// signedTxBytes builds a real signed tx and returns it with its wire encoding.
func signedTxBytes(t *testing.T, nonce uint64) (*types.Transaction, []byte) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	to := common.HexToAddress("0xcaf681a66d020601342297493863e78c959e5cb2")
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(testChainID),
		Nonce:     nonce,
		To:        &to,
		Value:     big.NewInt(1e15),
		Gas:       210000,
		GasFeeCap: big.NewInt(1e9),
		GasTipCap: big.NewInt(1),
		Data:      []byte{0x38, 0xed, 0x17, 0x39}, // swapExactTokensForTokens
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(testChainID)), key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return signed, raw
}

// wrapSignedTx produces an L2 payload of kind SignedTx.
func wrapSignedTx(raw []byte) []byte {
	return append([]byte{L2MessageKindSignedTx}, raw...)
}

// wrapBatch produces an L2 payload of kind Batch containing the given segments,
// each preceded by an 8-byte big-endian length prefix.
func wrapBatch(segments ...[]byte) []byte {
	out := []byte{L2MessageKindBatch}
	for _, seg := range segments {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(seg)))
		out = append(out, lenBuf[:]...)
		out = append(out, seg...)
	}
	return out
}

func msg(kind uint8, l2 []byte) *L1IncomingMessage {
	return &L1IncomingMessage{
		Header: &L1IncomingMessageHeader{Kind: kind, Timestamp: 1751328000},
		L2msg:  l2,
	}
}

func TestParseSingleSignedTx(t *testing.T) {
	want, raw := signedTxBytes(t, 7)

	got, err := ParseTransactions(msg(L1MessageTypeL2Message, wrapSignedTx(raw)))
	if err != nil {
		t.Fatalf("ParseTransactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d txs, want 1", len(got))
	}
	if got[0].Hash() != want.Hash() {
		t.Errorf("hash = %s, want %s", got[0].Hash(), want.Hash())
	}
}

func TestParseBatch(t *testing.T) {
	a, rawA := signedTxBytes(t, 1)
	b, rawB := signedTxBytes(t, 2)

	payload := wrapBatch(wrapSignedTx(rawA), wrapSignedTx(rawB))
	got, err := ParseTransactions(msg(L1MessageTypeL2Message, payload))
	if err != nil {
		t.Fatalf("ParseTransactions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d txs, want 2", len(got))
	}
	if got[0].Hash() != a.Hash() || got[1].Hash() != b.Hash() {
		t.Errorf("batch order wrong: got %s,%s want %s,%s",
			got[0].Hash(), got[1].Hash(), a.Hash(), b.Hash())
	}
}

// A corrupt segment must not blind us to its siblings.
func TestBatchSkipsUndecodableSegment(t *testing.T) {
	good, rawGood := signedTxBytes(t, 3)
	garbage := wrapSignedTx([]byte{0xde, 0xad, 0xbe, 0xef})

	payload := wrapBatch(garbage, wrapSignedTx(rawGood))
	got, err := ParseTransactions(msg(L1MessageTypeL2Message, payload))
	if err != nil {
		t.Fatalf("ParseTransactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d txs, want 1", len(got))
	}
	if got[0].Hash() != good.Hash() {
		t.Errorf("hash = %s, want %s", got[0].Hash(), good.Hash())
	}
}

// Deposits, retryables and rollup events are not swaps — no txs, no error.
func TestNonL2MessageKindIsIgnored(t *testing.T) {
	got, err := ParseTransactions(msg(12 /* EthDeposit */, []byte{0x01, 0x02}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d txs, want 0", len(got))
	}
}

func TestOversizePayloadRejected(t *testing.T) {
	_, err := ParseTransactions(msg(L1MessageTypeL2Message, make([]byte, MaxL2MessageSize+1)))
	if err != ErrMessageTooLarge {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestNilMessageRejected(t *testing.T) {
	if _, err := ParseTransactions(nil); err != ErrMalformed {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

// Nested batches must not recurse without bound.
func TestBatchDepthGuard(t *testing.T) {
	_, raw := signedTxBytes(t, 9)
	payload := wrapSignedTx(raw)
	for i := 0; i < maxBatchDepth+2; i++ {
		payload = wrapBatch(payload)
	}
	// The guard trips inside the nesting; the outer call must return cleanly
	// rather than blowing the stack.
	if _, err := ParseTransactions(msg(L1MessageTypeL2Message, payload)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The envelope must decode with the field names nitro actually emits — in
// particular signatureV2 (there is no "signature" key) and base64 l2Msg.
func TestEnvelopeJSONFieldNames(t *testing.T) {
	_, raw := signedTxBytes(t, 4)
	inner := L1IncomingMessage{
		Header: &L1IncomingMessageHeader{
			Kind:      L1MessageTypeL2Message,
			Poster:    common.HexToAddress("0xDaa526086787d9DEbE1D7F3FFdb1fE50cf8687F4"),
			Timestamp: 1751328000,
			L1BaseFee: big.NewInt(0),
		},
		L2msg: wrapSignedTx(raw),
	}
	hash := common.HexToHash("0xb7ef877253db5328f1e53b12afd6ca144186e69b7e7ab28b7886bbbf36870aeb")
	encoded, err := json.Marshal(BroadcastMessage{
		Version: 1,
		Messages: []*BroadcastFeedMessage{{
			SequenceNumber: 20555512,
			Message:        MessageWithMetadata{Message: &inner, DelayedMessagesRead: 5},
			BlockHash:      &hash,
			Signature:      make([]byte, 65),
		}},
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	// The signature field must serialise as signatureV2, never "signature".
	var probe map[string]any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatalf("probing: %v", err)
	}
	first := probe["messages"].([]any)[0].(map[string]any)
	if _, ok := first["signatureV2"]; !ok {
		t.Error("envelope missing signatureV2")
	}
	if _, ok := first["signature"]; ok {
		t.Error("envelope has a bogus 'signature' key")
	}

	var out BroadcastMessage
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(out.Messages))
	}
	m := out.Messages[0]
	if m.SequenceNumber != 20555512 {
		t.Errorf("seq = %d, want 20555512", m.SequenceNumber)
	}
	if m.BlockHash == nil || *m.BlockHash != hash {
		t.Errorf("blockHash = %v, want %s", m.BlockHash, hash)
	}
	if len(m.Signature) != 65 {
		t.Errorf("signature len = %d, want 65", len(m.Signature))
	}
	txs, err := ParseTransactions(m.Message.Message)
	if err != nil {
		t.Fatalf("ParseTransactions after round-trip: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("got %d txs, want 1", len(txs))
	}
}
