package monitor

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// The TUI itself needs a real terminal, so these tests cover everything
// underneath it: the state the daemon writes and the string the renderer
// produces. View() is exercised directly against a snapshot, which catches
// panics and formatting regressions without a TTY.

func TestSnapshotCounts(t *testing.T) {
	s := New("shadow.jsonl", false, 3)
	s.SetConnected(true)
	s.ObserveBlock(100, 5)
	s.ObserveBlock(102, 7)
	s.ObserveMatch()
	s.ObserveDecode(true)
	s.ObserveDecode(false)
	s.SetCache(6, 2)
	s.Add(Decision{Approved: true, LatencyMS: 100})
	s.Add(Decision{Approved: false, LatencyMS: 300})

	snap := s.Snapshot()
	if !snap.Connected {
		t.Error("connected = false, want true")
	}
	if snap.LastSeq != 102 {
		t.Errorf("lastSeq = %d, want 102", snap.LastSeq)
	}
	if snap.Blocks != 2 || snap.Txs != 12 {
		t.Errorf("blocks/txs = %d/%d, want 2/12", snap.Blocks, snap.Txs)
	}
	if snap.Approved != 1 || snap.Rejected != 1 {
		t.Errorf("verdicts = %d/%d, want 1/1", snap.Approved, snap.Rejected)
	}
	if snap.DecodeRate != 50 {
		t.Errorf("decodeRate = %.1f, want 50", snap.DecodeRate)
	}
	if snap.CacheRate != 75 {
		t.Errorf("cacheRate = %.1f, want 75", snap.CacheRate)
	}
}

// A sequence number must never go backwards on screen, even though blocks are
// handled concurrently and can arrive out of order.
func TestLastSeqNeverRegresses(t *testing.T) {
	s := New("x", false, 1)
	s.ObserveBlock(500, 1)
	s.ObserveBlock(490, 1)
	if got := s.Snapshot().LastSeq; got != 500 {
		t.Errorf("lastSeq = %d, want 500", got)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	s := New("x", false, 1)
	for i := 1; i <= 100; i++ {
		s.Add(Decision{LatencyMS: float64(i)})
	}
	snap := s.Snapshot()
	if snap.P50 < 45 || snap.P50 > 55 {
		t.Errorf("p50 = %.0f, want ~50", snap.P50)
	}
	if snap.P90 < 85 || snap.P90 > 95 {
		t.Errorf("p90 = %.0f, want ~90", snap.P90)
	}
	if snap.Max != 100 {
		t.Errorf("max = %.0f, want 100", snap.Max)
	}
}

// The ring buffer must bound memory on a multi-day run while keeping the
// newest decisions.
func TestRecentIsBounded(t *testing.T) {
	s := New("x", false, 1)
	for i := 0; i < maxRecent+50; i++ {
		s.Add(Decision{Seq: uint64(i)})
	}
	snap := s.Snapshot()
	if len(snap.Recent) != maxRecent {
		t.Fatalf("recent = %d, want %d", len(snap.Recent), maxRecent)
	}
	if last := snap.Recent[len(snap.Recent)-1].Seq; last != uint64(maxRecent+49) {
		t.Errorf("newest seq = %d, want %d", last, maxRecent+49)
	}
}

// The alarm is the only warning that a bot router changed its calldata layout,
// so it must fire on a low rate but stay quiet on a small sample.
func TestDecodeAlarm(t *testing.T) {
	quiet := New("x", false, 1)
	for i := 0; i < 5; i++ {
		quiet.ObserveDecode(false)
	}
	if quiet.Snapshot().DecodeAlarm() {
		t.Error("alarm fired below the sample floor")
	}

	loud := New("x", false, 1)
	for i := 0; i < 8; i++ {
		loud.ObserveDecode(false)
	}
	for i := 0; i < 4; i++ {
		loud.ObserveDecode(true)
	}
	if !loud.Snapshot().DecodeAlarm() {
		t.Error("alarm did not fire at 33% decode rate")
	}

	healthy := New("x", false, 1)
	for i := 0; i < 20; i++ {
		healthy.ObserveDecode(true)
	}
	if healthy.Snapshot().DecodeAlarm() {
		t.Error("alarm fired at 100% decode rate")
	}
}

// The daemon writes from many goroutines while the renderer reads. Run with
// -race to make this meaningful.
func TestConcurrentAccess(t *testing.T) {
	s := New("x", false, 2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.ObserveBlock(uint64(n*1000+j), 3)
				s.ObserveDecode(j%2 == 0)
				s.Add(Decision{Approved: j%3 == 0, LatencyMS: float64(j)})
				s.SetCache(j, j/2)
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()

	if got := s.Snapshot().Blocks; got != 1600 {
		t.Errorf("blocks = %d, want 1600", got)
	}
}

func TestViewRendersWithoutTTY(t *testing.T) {
	s := New("shadow.jsonl", false, 3)
	s.SetConnected(true)
	s.ObserveBlock(24382740, 12)
	s.SetCache(49, 35)
	s.Add(Decision{At: time.Now(), Approved: true, Direction: "buy",
		Token: "0x8CA07e1bb4677BCF36b63d5dF35291f473b40A64",
		Seq:   24382740, LatencyMS: 260, Reason: "approved"})
	s.Add(Decision{At: time.Now(), Approved: false, Direction: "sell",
		Token: "0xaF3D76f1834A1d425780943C99Ea8A608f8a93f9",
		Seq:   24368725, LatencyMS: 826, Reason: "min_liquidity: 0.22 ETH below floor 0.25 ETH"})

	m := model{state: s, snap: s.Snapshot(), width: 100, height: 30}
	out := m.View()

	for _, want := range []string{"hoodsniper", "SHADOW", "feed", "health", "decisions",
		"APPROVE", "REJECT", "connected", "q quit", "Wallets", "Ledger", "Config"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q", want)
		}
	}
	// Shadow mode must never render as live — that would misrepresent whether
	// real money is at risk.
	if strings.Contains(out, "LIVE") {
		t.Error("shadow-mode view rendered as LIVE")
	}
}

func TestViewMarksLiveMode(t *testing.T) {
	s := New("shadow.jsonl", true, 1)
	m := model{state: s, snap: s.Snapshot(), width: 100, height: 30}
	if out := m.View(); !strings.Contains(out, "LIVE") {
		t.Error("live-mode view did not render the LIVE banner")
	}
}

func TestViewSurvivesNarrowTerminal(t *testing.T) {
	s := New("x", false, 1)
	s.Add(Decision{Approved: false, Direction: "sell", Token: "0xabc",
		LatencyMS: 5000, Reason: strings.Repeat("very long reason ", 20)})
	for _, w := range []int{0, 20, 40, 200} {
		m := model{state: s, snap: s.Snapshot(), width: w, height: 10}
		if m.View() == "" {
			t.Errorf("width %d produced an empty view", w)
		}
	}
}

func TestApprovedOnlyFilter(t *testing.T) {
	s := New("x", false, 1)
	s.Add(Decision{Approved: false, Direction: "sell", Token: "0xdead", Reason: "rejected-marker"})
	s.Add(Decision{Approved: true, Direction: "buy", Token: "0xbeef", Reason: "approved-marker"})

	all := model{state: s, snap: s.Snapshot(), width: 100, height: 30}
	if !strings.Contains(all.View(), "rejected-marker") {
		t.Error("unfiltered view hid a rejection")
	}
	only := model{state: s, snap: s.Snapshot(), width: 100, height: 30, live: liveView{approvedOnly: true}}
	view := only.View()
	if strings.Contains(view, "rejected-marker") {
		t.Error("approved-only view showed a rejection")
	}
	if !strings.Contains(view, "approved-marker") {
		t.Error("approved-only view hid an approval")
	}
}
