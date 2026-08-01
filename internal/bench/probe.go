// Package bench measures the only number that decides whether this project is
// viable: how much earlier the sequencer feed shows a transaction than the RPC.
//
// The feed cannot beat execution — the sequencer orders and executes before it
// broadcasts. What it beats is every node that must re-execute the block before
// it can answer eth_getTransactionByHash. That gap is the entire edge, and if it
// is small there is nothing to trade on.
package bench

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// Probe races the RPC to first-sight of a transaction hash.
type Probe struct {
	client   *rpc.Client
	interval time.Duration
	timeout  time.Duration
	sem      chan struct{}

	mu      sync.Mutex
	samples []time.Duration
	misses  int
	wg      sync.WaitGroup
}

// NewProbe dials rpcURL. interval is the poll cadence, and concurrency caps
// in-flight samples so a busy block cannot stampede the node.
//
// Measurement resolution is bounded by max(interval, RPC round-trip time), and
// in practice RTT dominates: polls are serialised, so against a 250ms-RTT
// endpoint a 10ms interval still only resolves to ~250ms. Runs from a host far
// from the node therefore report an edge quantised to whole round trips, and a
// sample equal to one RTT means "already known on the first poll" — a floor
// artefact, not a measurement. Co-locate before trusting the absolute number.
func NewProbe(ctx context.Context, rpcURL string, interval, timeout time.Duration, concurrency int) (*Probe, error) {
	client, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	return &Probe{
		client:   client,
		interval: interval,
		timeout:  timeout,
		sem:      make(chan struct{}, concurrency),
	}, nil
}

// Sample polls for hash until the RPC admits it exists, recording the delay
// relative to feedTime. It returns immediately; the measurement runs in the
// background. If no slot is free the sample is skipped rather than queued —
// a delayed start would inflate the very number we are measuring.
func (p *Probe) Sample(ctx context.Context, hash common.Hash, feedTime time.Time) {
	select {
	case p.sem <- struct{}{}:
	default:
		return
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()

		ackedAt, ok := p.poll(ctx, hash, feedTime.Add(p.timeout))
		p.mu.Lock()
		defer p.mu.Unlock()
		if !ok {
			p.misses++
			return
		}
		// The edge is measured from the feed frame landing, not from when this
		// goroutine happened to get scheduled.
		p.samples = append(p.samples, ackedAt.Sub(feedTime))
	}()
}

// poll returns the wall-clock instant at which the RPC first acknowledged hash.
func (p *Probe) poll(ctx context.Context, hash common.Hash, deadline time.Time) (time.Time, bool) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		var raw json.RawMessage
		err := p.client.CallContext(ctx, &raw, "eth_getTransactionByHash", hash)
		// A known hash returns an object; an unknown one returns JSON null.
		if err == nil && len(raw) > 0 && string(raw) != "null" {
			return time.Now(), true
		}

		if time.Now().After(deadline) {
			return time.Time{}, false
		}
		select {
		case <-ctx.Done():
			return time.Time{}, false
		case <-ticker.C:
		}
	}
}

// Stats is an immutable snapshot of the measurements so far.
type Stats struct {
	N      int
	Misses int
	P50    time.Duration
	P95    time.Duration
	Min    time.Duration
	Max    time.Duration
}

// Stats returns a snapshot. Safe to call while sampling continues.
func (p *Probe) Stats() Stats {
	p.mu.Lock()
	sorted := make([]time.Duration, len(p.samples))
	copy(sorted, p.samples)
	misses := p.misses
	p.mu.Unlock()

	if len(sorted) == 0 {
		return Stats{Misses: misses}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return Stats{
		N:      len(sorted),
		Misses: misses,
		P50:    percentile(sorted, 0.50),
		P95:    percentile(sorted, 0.95),
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
	}
}

// Wait blocks until in-flight samples finish, then closes the RPC connection.
func (p *Probe) Wait() {
	p.wg.Wait()
	p.client.Close()
}

// percentile indexes into a pre-sorted slice using nearest-rank.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
