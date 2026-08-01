// Package feed decodes the Arbitrum Nitro sequencer broadcaster wire format.
//
// Robinhood Chain has no public mempool. The sequencer announces its ordering
// decision on one WebSocket, and this package turns that into transactions.
//
// Note the ordering guarantee this does NOT give you: the envelope carries a
// populated blockHash, so the sequencer has already ordered AND executed the
// block before broadcasting. You are ahead of every node that must re-execute
// the block to serve a receipt — you are not ahead of execution itself.
//
// Struct shapes mirror nitro/broadcaster/message/message.go and
// nitro/arbos/arbostypes/incomingmessage.go. We reimplement rather than import:
// nitro's go.mod carries a `replace` pointing go-ethereum at a vendored fork,
// which makes it hostile to use as a library alongside upstream go-ethereum.
package feed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// MaxL2MessageSize mirrors arbostypes.MaxL2MessageSize.
const MaxL2MessageSize = 256 * 1024

// L1 message kinds (arbostypes). Only L2Message carries user transactions.
const (
	L1MessageTypeL2Message = 3
)

// L2 message kinds (arbos/parse_l2.go).
const (
	L2MessageKindUnsignedUserTx   = 0
	L2MessageKindContractTx       = 1
	L2MessageKindNonmutatingCall  = 2
	L2MessageKindBatch            = 3
	L2MessageKindSignedTx         = 4
	L2MessageKindHeartbeat        = 6 // deprecated
	L2MessageKindSignedCompressed = 7
)

// maxBatchDepth mirrors nitro's own recursion guard.
const maxBatchDepth = 16

var (
	// ErrMessageTooLarge means the payload exceeded MaxL2MessageSize.
	ErrMessageTooLarge = errors.New("feed: l2 message too large")
	// ErrBatchTooDeep means nested batches exceeded maxBatchDepth.
	ErrBatchTooDeep = errors.New("feed: batch nesting too deep")
	// ErrMalformed means the envelope was structurally invalid.
	ErrMalformed = errors.New("feed: malformed message")
)

// BroadcastMessage is one frame off the WebSocket. Exactly one of the message
// fields is populated.
type BroadcastMessage struct {
	Version                        int                             `json:"version"`
	Messages                       []*BroadcastFeedMessage         `json:"messages,omitempty"`
	ConfirmedSequenceNumberMessage *ConfirmedSequenceNumberMessage `json:"confirmedSequenceNumberMessage,omitempty"`
}

// ConfirmedSequenceNumberMessage reports the sequencer's confirmed tip.
type ConfirmedSequenceNumberMessage struct {
	SequenceNumber uint64 `json:"sequenceNumber"`
}

// BroadcastFeedMessage is one L2 block. The sequence number IS the L2 block
// number; one message equals one block.
//
// Signature is tagged signatureV2, not "signature" — there is no "signature"
// key in the envelope, and reading the wrong one makes a signed feed look
// unsigned.
type BroadcastFeedMessage struct {
	SequenceNumber uint64              `json:"sequenceNumber"`
	Message        MessageWithMetadata `json:"message"`
	BlockHash      *common.Hash        `json:"blockHash,omitempty"`
	Signature      []byte              `json:"signatureV2"`
	BlockMetadata  []byte              `json:"blockMetadata,omitempty"`
}

// MessageWithMetadata wraps the L1 incoming message with delayed-message count.
type MessageWithMetadata struct {
	Message             *L1IncomingMessage `json:"message"`
	DelayedMessagesRead uint64             `json:"delayedMessagesRead"`
}

// L1IncomingMessage carries the header and the opaque L2 payload. L2msg is
// JSON-encoded as base64 because encoding/json renders []byte that way.
type L1IncomingMessage struct {
	Header *L1IncomingMessageHeader `json:"header"`
	L2msg  []byte                   `json:"l2Msg"`
}

// L1IncomingMessageHeader mirrors arbostypes.L1IncomingMessageHeader.
// Poster is tagged "sender" upstream — keep it.
type L1IncomingMessageHeader struct {
	Kind        uint8          `json:"kind"`
	Poster      common.Address `json:"sender"`
	BlockNumber uint64         `json:"blockNumber"`
	Timestamp   uint64         `json:"timestamp"`
	RequestID   *common.Hash   `json:"requestId"`
	L1BaseFee   *big.Int       `json:"baseFeeL1"`
}

// ParseTransactions extracts user transactions from one feed message.
//
// It handles only the kinds that carry signed user transactions: L2Message
// containing SignedTx or Batch. Deposits, retryables and rollup events return
// no transactions and no error — they are legitimately not swaps.
func ParseTransactions(msg *L1IncomingMessage) (types.Transactions, error) {
	if msg == nil || msg.Header == nil {
		return nil, ErrMalformed
	}
	if len(msg.L2msg) > MaxL2MessageSize {
		return nil, ErrMessageTooLarge
	}
	if msg.Header.Kind != L1MessageTypeL2Message {
		return nil, nil
	}
	return parseL2Message(bytes.NewReader(msg.L2msg), 0)
}

// parseL2Message walks one L2 payload. A batch is a sequence of length-prefixed
// sub-messages; nitro terminates the batch on any read error, so a short read
// means "no further messages" rather than corruption.
func parseL2Message(rd *bytes.Reader, depth int) (types.Transactions, error) {
	if depth >= maxBatchDepth {
		return nil, ErrBatchTooDeep
	}
	kind, err := rd.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("feed: reading l2 kind: %w", err)
	}

	switch kind {
	case L2MessageKindSignedTx:
		data, err := io.ReadAll(rd)
		if err != nil {
			return nil, fmt.Errorf("feed: reading signed tx: %w", err)
		}
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(data); err != nil {
			return nil, fmt.Errorf("feed: decoding signed tx: %w", err)
		}
		return types.Transactions{tx}, nil

	case L2MessageKindBatch:
		var out types.Transactions
		for {
			segment, err := readBytestring(rd)
			if err != nil {
				// No further messages in the batch. Matches nitro's behaviour.
				return out, nil
			}
			txs, err := parseL2Message(bytes.NewReader(segment), depth+1)
			if err != nil {
				// ponytail: skip the undecodable segment rather than dropping the
				// whole block — one bad segment must not blind us to its siblings.
				continue
			}
			out = append(out, txs...)
		}

	default:
		// Unsigned/contract/compressed kinds carry no signed user tx we can
		// attribute to a watchlist wallet. Not an error.
		return nil, nil
	}
}

// readBytestring reads an 8-byte big-endian length prefix and that many bytes.
func readBytestring(rd *bytes.Reader) ([]byte, error) {
	var lenBuf [8]byte
	if _, err := io.ReadFull(rd, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint64(lenBuf[:])
	if n > MaxL2MessageSize {
		return nil, ErrMessageTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
