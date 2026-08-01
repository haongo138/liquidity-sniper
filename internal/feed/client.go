package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/gorilla/websocket"
)

// Well-known endpoints and chain IDs.
const (
	MainnetFeed = "wss://feed.mainnet.chain.robinhood.com"
	TestnetFeed = "wss://feed.testnet.chain.robinhood.com"

	MainnetChainID = 4663
	TestnetChainID = 46630

	// LocalRelay is the default port the Nitro relay binary serves on.
	LocalRelay = "ws://127.0.0.1:9642"
)

// resumeHeader asks the relay to replay from a specific sequence number.
//
// Omitting it replays the relay's whole backlog (~1,200 messages, ~124s stale).
// Sending a number past the relay's tail is worse: the lookup fails and nitro's
// fallback is to send the ENTIRE backlog. So only ever send a sequence number
// we have actually seen.
const resumeHeader = "Arbitrum-Requested-Sequence-Number"

// Block is one decoded L2 block. The sequence number is the block number.
type Block struct {
	SeqNum uint64
	// Hash is the sequencer's own block hash. Non-nil in practice; a mismatch
	// at a sequence number already seen indicates a feed reorg.
	Hash *common.Hash
	// Timestamp is the L1 header timestamp, not our clock.
	Timestamp uint64
	Txs       types.Transactions
	// ReceivedAt is when the frame landed locally. This is the clock that
	// matters for measuring the latency edge.
	ReceivedAt time.Time
}

// Client streams decoded blocks from a Nitro broadcaster endpoint.
type Client struct {
	URL     string
	ChainID *big.Int
	Log     *slog.Logger

	// DialTimeout bounds the initial handshake.
	DialTimeout time.Duration
	// ReadTimeout fails a connection that goes quiet, so we reconnect rather
	// than hang forever on a dead upstream.
	ReadTimeout time.Duration
}

// NewClient builds a client with sane defaults.
func NewClient(url string, chainID int64, log *slog.Logger) *Client {
	return &Client{
		URL:         url,
		ChainID:     big.NewInt(chainID),
		Log:         log,
		DialTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second,
	}
}

// Run streams blocks to handle until ctx is cancelled. It reconnects with
// backoff on any transport error, resuming from the last sequence number seen.
func (c *Client) Run(ctx context.Context, handle func(Block)) error {
	var lastSeq uint64
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := c.stream(ctx, &lastSeq, handle)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.Log.Warn("feed disconnected, reconnecting", "err", err, "last_seq", lastSeq, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff = backoff * 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// stream holds one connection open until it fails.
func (c *Client) stream(ctx context.Context, lastSeq *uint64, handle func(Block)) error {
	header := http.Header{}
	if *lastSeq > 0 {
		// Always a number we have seen, so it is always in the relay's lookup
		// table. Costs one duplicate message, which the seq check below drops.
		header.Set(resumeHeader, fmt.Sprintf("%d", *lastSeq))
	}

	dialer := &websocket.Dialer{HandshakeTimeout: c.DialTimeout}
	conn, resp, err := dialer.DialContext(ctx, c.URL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s (http %d): %w", c.URL, resp.StatusCode, err)
		}
		return fmt.Errorf("dial %s: %w", c.URL, err)
	}
	defer conn.Close()

	c.Log.Info("feed connected", "url", c.URL, "resume_from", *lastSeq)

	// Close the connection when ctx dies so the blocking read returns.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(c.ReadTimeout)); err != nil {
			return err
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		received := time.Now()

		var bm BroadcastMessage
		if err := json.Unmarshal(data, &bm); err != nil {
			c.Log.Warn("undecodable frame", "err", err)
			continue
		}

		for _, m := range bm.Messages {
			if m == nil || m.Message.Message == nil {
				continue
			}
			// Drop the replayed duplicate and anything stale.
			if *lastSeq > 0 && m.SequenceNumber <= *lastSeq {
				continue
			}
			*lastSeq = m.SequenceNumber

			txs, err := ParseTransactions(m.Message.Message)
			if err != nil {
				c.Log.Warn("undecodable l2 message", "seq", m.SequenceNumber, "err", err)
				continue
			}
			if len(txs) == 0 {
				continue
			}

			handle(Block{
				SeqNum:     m.SequenceNumber,
				Hash:       m.BlockHash,
				Timestamp:  m.Message.Message.Header.Timestamp,
				Txs:        txs,
				ReceivedAt: received,
			})
		}
	}
}

// Sender recovers the signer of tx. This is the single most expensive field to
// extract (~0.1ms of ECDSA recovery), so call it only when you need it.
func (c *Client) Sender(tx *types.Transaction) (common.Address, error) {
	return types.Sender(types.LatestSignerForChainID(c.ChainID), tx)
}
