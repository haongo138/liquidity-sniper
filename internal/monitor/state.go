// Package monitor holds the daemon's live state so it can be rendered more than
// one way.
//
// The daemon previously printed straight to stdout, which meant a TUI would have
// had to duplicate the pipeline or scrape its own output. Instead the pipeline
// writes here, and stdout and the TUI are two readers of the same state. Nothing
// in the hot path knows which renderer is attached.
package monitor

import (
	"sort"
	"sync"
	"time"
)

// maxRecent bounds the decision ring buffer. The TUI shows a screenful and the
// rest is in the shadow log, so retaining more would only grow memory on a
// multi-day run.
const maxRecent = 500

// Decision is one evaluated swap, flattened for display.
type Decision struct {
	At        time.Time
	Approved  bool
	Direction string
	Token     string
	KOL       string
	Seq       uint64
	LatencyMS float64
	Reason    string
}

// State is the daemon's live state. Safe for concurrent use: the feed handler
// writes from several goroutines while a renderer reads.
type State struct {
	mu sync.RWMutex

	startedAt time.Time
	connected bool
	lastSeq   uint64

	blocks, txs        uint64
	matched            uint64
	approved, rejected int
	decodeOK, decodeNG uint64

	latencies []float64
	recent    []Decision

	// cache counters are pushed in rather than read, so this package does not
	// depend on the chain client.
	cacheHits, cacheMisses int

	shadowLog string
	live      bool
	watching  int

	// Feed view state: a sample of raw router traffic and a running tally of
	// which contract/selector pairs carry it. The tally is what catches a new
	// router before the decode rate silently drops.
	rawRecent []RawTx
	tally     map[TallyKey]*tallyEntry

	cfg Config
}

// RawTx is one observed router-bound transaction, before any decoding.
type RawTx struct {
	At       time.Time
	Seq      uint64
	From     string
	To       string
	Selector string
	Decoded  bool
}

// TallyKey identifies a call target and the function invoked on it.
type TallyKey struct {
	To       string
	Selector string
}

type tallyEntry struct {
	n       int
	senders map[string]struct{}
}

// TallyRow is one row of the discover tally.
type TallyRow struct {
	TallyKey
	N       int
	Senders int
}

// Config is the subset of the loaded configuration the UI displays.
type Config struct {
	Path         string
	Live         bool
	Feed         string
	RPC          string
	ChainID      int64
	Watch        []string
	TradeSizeETH float64
	LedgerPath   string
	Filters      []ConfigRow
}

// ConfigRow is one displayed filter setting.
type ConfigRow struct {
	Name, Value, Note string
}

// New builds an empty State.
func New(shadowLog string, live bool, watching int) *State {
	return &State{
		startedAt: time.Now(), shadowLog: shadowLog, live: live, watching: watching,
		tally: map[TallyKey]*tallyEntry{},
	}
}

// SetConfig records the loaded configuration for display.
func (s *State) SetConfig(c Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = c
}

// ObserveRouterTx records raw router traffic for the Feed view. This runs on
// every router-bound transaction, not just watched ones, so it stays cheap:
// a bounded ring plus two map writes.
func (s *State) ObserveRouterTx(tx RawTx) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rawRecent = append(s.rawRecent, tx)
	if len(s.rawRecent) > maxRecent {
		s.rawRecent = s.rawRecent[len(s.rawRecent)-maxRecent:]
	}

	k := TallyKey{To: tx.To, Selector: tx.Selector}
	e := s.tally[k]
	if e == nil {
		e = &tallyEntry{senders: map[string]struct{}{}}
		s.tally[k] = e
	}
	e.n++
	e.senders[tx.From] = struct{}{}
}

// SetConnected records feed connectivity.
func (s *State) SetConnected(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = v
}

// ObserveBlock records one decoded block.
func (s *State) ObserveBlock(seq uint64, txs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks++
	s.txs += uint64(txs)
	if seq > s.lastSeq {
		s.lastSeq = seq
	}
}

// ObserveMatch records a transaction from a watched wallet.
func (s *State) ObserveMatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matched++
}

// ObserveDecode records whether a matched transaction decoded. A decode rate
// that quietly falls is the expected symptom of a bot router changing layout.
func (s *State) ObserveDecode(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.decodeOK++
	} else {
		s.decodeNG++
	}
}

// SetCache records the profile cache counters.
func (s *State) SetCache(hits, misses int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheHits, s.cacheMisses = hits, misses
}

// Add records one decision.
func (s *State) Add(d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.Approved {
		s.approved++
	} else {
		s.rejected++
	}
	s.latencies = append(s.latencies, d.LatencyMS)
	s.recent = append(s.recent, d)
	if len(s.recent) > maxRecent {
		s.recent = s.recent[len(s.recent)-maxRecent:]
	}
}

// Snapshot is an immutable view for rendering.
type Snapshot struct {
	Uptime    time.Duration
	Connected bool
	LastSeq   uint64
	Blocks    uint64
	Txs       uint64
	Matched   uint64

	Approved, Rejected int
	DecodeOK, DecodeNG uint64
	DecodeRate         float64

	CacheHits, CacheMisses int
	CacheRate              float64

	P50, P90, Max float64
	Recent        []Decision

	RawRecent []RawTx
	Tally     []TallyRow
	Config    Config

	ShadowLog string
	Live      bool
	Watching  int
}

// Snapshot copies the state so a renderer never holds the lock while drawing.
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := Snapshot{
		Uptime: time.Since(s.startedAt), Connected: s.connected, LastSeq: s.lastSeq,
		Blocks: s.blocks, Txs: s.txs, Matched: s.matched,
		Approved: s.approved, Rejected: s.rejected,
		DecodeOK: s.decodeOK, DecodeNG: s.decodeNG,
		CacheHits: s.cacheHits, CacheMisses: s.cacheMisses,
		ShadowLog: s.shadowLog, Live: s.live, Watching: s.watching,
	}
	if total := s.decodeOK + s.decodeNG; total > 0 {
		snap.DecodeRate = 100 * float64(s.decodeOK) / float64(total)
	}
	if total := s.cacheHits + s.cacheMisses; total > 0 {
		snap.CacheRate = 100 * float64(s.cacheHits) / float64(total)
	}

	if len(s.latencies) > 0 {
		sorted := make([]float64, len(s.latencies))
		copy(sorted, s.latencies)
		sort.Float64s(sorted)
		snap.P50 = percentile(sorted, 0.50)
		snap.P90 = percentile(sorted, 0.90)
		snap.Max = sorted[len(sorted)-1]
	}

	snap.Recent = make([]Decision, len(s.recent))
	copy(snap.Recent, s.recent)

	snap.RawRecent = make([]RawTx, len(s.rawRecent))
	copy(snap.RawRecent, s.rawRecent)

	snap.Tally = make([]TallyRow, 0, len(s.tally))
	for k, e := range s.tally {
		snap.Tally = append(snap.Tally, TallyRow{TallyKey: k, N: e.n, Senders: len(e.senders)})
	}
	sort.Slice(snap.Tally, func(i, j int) bool {
		if snap.Tally[i].N != snap.Tally[j].N {
			return snap.Tally[i].N > snap.Tally[j].N
		}
		return snap.Tally[i].Senders > snap.Tally[j].Senders
	})

	snap.Config = s.cfg
	return snap
}

// DecodeAlarm reports whether the decode rate has fallen far enough to suggest a
// router changed its calldata layout. Below the sample floor there is not enough
// evidence to raise it.
func (s Snapshot) DecodeAlarm() bool {
	return s.DecodeOK+s.DecodeNG >= 10 && s.DecodeRate < 80
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
