package sched

import (
	"testing"
	"time"

	"github.com/CarterCole/bond/core/wire"
)

const (
	msec = int64(time.Millisecond)
	sec  = int64(time.Second)
)

// up returns a path that has completed a round trip with the given RTT.
func up(policy Policy, rtt time.Duration) *State {
	return &State{Policy: policy, Up: true, SRTT: rtt, MinRTT: rtt}
}

func pick(paths []*State, class wire.Class, now int64) []int {
	cfg := Defaults()
	return Pick(paths, class, 1200, now, &cfg, false, nil)
}

func TestBulkTakesLowestLatency(t *testing.T) {
	ps := []*State{up(PolicyBonded, 80*time.Millisecond), up(PolicyBonded, 20*time.Millisecond)}
	if got := pick(ps, wire.ClassBulk, 0); len(got) != 1 || got[0] != 1 {
		t.Fatalf("picked %v, want the 20ms path", got)
	}
}

func TestPrimaryFillsBeforeBonded(t *testing.T) {
	// Equal paths: the primary wins on bias alone.
	ps := []*State{up(PolicyBonded, 20*time.Millisecond), up(PolicyPrimary, 20*time.Millisecond)}
	if got := pick(ps, wire.ClassBulk, 0); got[0] != 1 {
		t.Fatalf("picked %v, want the primary", got)
	}
	// But not at any price: a clearly faster bonded path is used.
	ps = []*State{up(PolicyBonded, 20*time.Millisecond), up(PolicyPrimary, 120*time.Millisecond)}
	if got := pick(ps, wire.ClassBulk, 0); got[0] != 0 {
		t.Fatalf("picked %v, want the much faster bonded path", got)
	}
}

func TestSpillsWhenPathFills(t *testing.T) {
	cfg := Defaults()
	fast, slow := up(PolicyBonded, 20*time.Millisecond), up(PolicyBonded, 60*time.Millisecond)
	fast.Capacity, slow.Capacity = 500_000, 500_000 // 4 Mbit/s each
	ps := []*State{fast, slow}
	// Send a burst with no acknowledgements: the fast path's queue grows
	// until the slow path becomes the earlier arrival.
	spilled := -1
	for i := 0; i < 200; i++ {
		got := Pick(ps, wire.ClassBulk, 1200, 0, &cfg, false, nil)
		ps[got[0]].OnSend(1200, 0)
		if got[0] == 1 {
			spilled = i
			break
		}
	}
	if spilled < 0 {
		t.Fatal("never spilled to the second path")
	}
	// Allowance is Gain × capacity × (rtt + lag) = 1.25 × 500kB/s × 45ms ≈ 28kB
	// (23 datagrams), then 20ms of extra queue (the latency gap, 8 more)
	// plus nothing for bias since both are bonded.
	if spilled < 20 || spilled > 45 {
		t.Fatalf("spilled after %d datagrams; expected roughly 31", spilled)
	}
}

func TestBackupOnlyWhenNothingElseIsHealthy(t *testing.T) {
	prim, back := up(PolicyPrimary, 50*time.Millisecond), up(PolicyBackup, 5*time.Millisecond)
	ps := []*State{prim, back}
	if got := pick(ps, wire.ClassBulk, 0); got[0] != 0 {
		t.Fatalf("picked %v: backup used while the primary is healthy", got)
	}
	prim.Up = false
	if got := pick(ps, wire.ClassBulk, 0); got[0] != 1 {
		t.Fatalf("picked %v: backup not used with the primary down", got)
	}
}

func TestOffIsNeverUsed(t *testing.T) {
	ps := []*State{up(PolicyOff, time.Millisecond)}
	for _, c := range []wire.Class{wire.ClassBulk, wire.ClassRealtime, wire.ClassControl} {
		if got := pick(ps, c, 0); len(got) != 0 {
			t.Fatalf("class %v used an off path", c)
		}
	}
}

func TestRealtimeTakesTwoBestAndReachesIntoBackup(t *testing.T) {
	ps := []*State{
		up(PolicyBonded, 90*time.Millisecond),
		up(PolicyBonded, 20*time.Millisecond),
		up(PolicyBonded, 40*time.Millisecond),
	}
	got := pick(ps, wire.ClassRealtime, 0)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("picked %v, want the 20ms and 40ms paths", got)
	}
	// One healthy path plus a backup: redundancy is worth waking the backup for.
	ps = []*State{up(PolicyPrimary, 20*time.Millisecond), up(PolicyBackup, 40*time.Millisecond)}
	if got := pick(ps, wire.ClassRealtime, 0); len(got) != 2 {
		t.Fatalf("picked %v, want both", got)
	}
	// But a dead path gets no copy.
	ps[1].Up = false
	if got := pick(ps, wire.ClassRealtime, 0); len(got) != 1 || got[0] != 0 {
		t.Fatalf("picked %v, want only the live path", got)
	}
}

func TestControlGoesEverywhereAlive(t *testing.T) {
	ps := []*State{up(PolicyPrimary, 20*time.Millisecond), up(PolicyBackup, 20*time.Millisecond), up(PolicyOff, 20*time.Millisecond), {Policy: PolicyBonded}}
	if got := pick(ps, wire.ClassControl, 0); len(got) != 2 {
		t.Fatalf("picked %v, want the two live usable paths", got)
	}
	// Nothing alive (startup): try everything that is allowed.
	ps = []*State{{Policy: PolicyBonded}, {Policy: PolicyBackup}, {Policy: PolicyOff}}
	if got := pick(ps, wire.ClassControl, 0); len(got) != 2 {
		t.Fatalf("picked %v, want both allowed paths", got)
	}
}

func TestAllDownUsesMostRecentlyAlive(t *testing.T) {
	a, b := &State{Policy: PolicyBonded, LastRx: 1 * sec}, &State{Policy: PolicyBonded, LastRx: 5 * sec}
	if got := pick([]*State{a, b}, wire.ClassBulk, 6*sec); len(got) != 1 || got[0] != 1 {
		t.Fatalf("picked %v, want the path heard from most recently", got)
	}
}

func TestLossySustainedBecomesUnhealthy(t *testing.T) {
	cfg := Defaults()
	s := up(PolicyPrimary, 20*time.Millisecond)
	report := func(now int64, highest, pkts uint32) {
		f := wire.Feedback{EchoDelay: wire.NoEcho, RxHighest: highest, RxPackets: pkts, RxClock: uint32(now / msec)}
		s.OnFeedback(&f, now, uint32(now/msec), &cfg)
	}
	report(0, 0, 1)
	// A one-off burst: 50% of 100 datagrams lost.
	report(100*msec, 100, 51)
	if s.Loss < 0.4 {
		t.Fatalf("loss %.2f after a 50%% burst", s.Loss)
	}
	if !s.Healthy(200 * msec) {
		t.Fatal("a momentary loss burst made the path unhealthy")
	}
	// Still unhealthy two seconds on, with loss continuing.
	hi, got := uint32(100), uint32(51)
	for now := 200 * msec; now <= 3*sec; now += 100 * msec {
		hi, got = hi+100, got+50
		report(now, hi, got)
	}
	if s.Healthy(3 * sec) {
		t.Fatalf("path healthy after 3s at 50%% loss (loss estimate %.2f)", s.Loss)
	}
	// Clean again: recovers as soon as the estimate drops under the threshold.
	for now := 3*sec + 100*msec; now <= 5*sec; now += 100 * msec {
		hi, got = hi+100, got+100
		report(now, hi, got)
	}
	if !s.Healthy(5 * sec) {
		t.Fatalf("path did not recover (loss estimate %.2f)", s.Loss)
	}
}

func TestRTTFromEcho(t *testing.T) {
	cfg := Defaults()
	s := &State{}
	// We sent at 1000ms; the peer held it 15ms; the echo arrives at 1055ms: rtt 40ms.
	f := wire.Feedback{EchoTS: 1000, EchoDelay: 15, RxPackets: 1}
	s.OnFeedback(&f, 1055*msec, 1055, &cfg)
	if s.SRTT != 40*time.Millisecond || s.MinRTT != 40*time.Millisecond {
		t.Fatalf("srtt %v min %v, want 40ms", s.SRTT, s.MinRTT)
	}
}

func TestInflightTracksAcks(t *testing.T) {
	cfg := Defaults()
	s := &State{}
	for i := 0; i < 10; i++ {
		s.OnSend(1000, 0)
	}
	if s.Inflight() != 10_000 {
		t.Fatalf("inflight %d before any ack", s.Inflight())
	}
	f := wire.Feedback{EchoDelay: wire.NoEcho, RxHighest: 5, RxPackets: 6}
	s.OnFeedback(&f, 10*msec, 10, &cfg)
	if s.Inflight() != 4_000 {
		t.Fatalf("inflight %d after seqs 0-5 acked", s.Inflight())
	}
	if !s.Acked(5) || s.Acked(6) {
		t.Fatal("Acked disagrees with the feedback")
	}
}

// The estimate must follow what the peer measures, hold while the path is
// idle or lightly used, and come down only when the path is overdriven.
func TestCapacityEstimate(t *testing.T) {
	cfg := Defaults()
	s := &State{}
	var rxBytes uint64
	var seq, rxPkts uint32
	now := int64(0)
	// step advances 100ms, during which we send `sent` bytes and the peer receives `got`.
	step := func(sent, got int) {
		for n := sent; n > 0; n -= 1000 {
			seq = s.OnSend(1000, now)
		}
		now += 100 * msec
		rxBytes += uint64(got)
		rxPkts += uint32(got / 1000)
		f := wire.Feedback{EchoTS: uint32(now/msec) - 20, EchoDelay: 0, RxHighest: seq, RxPackets: rxPkts, RxBytes: rxBytes, RxClock: uint32(now / msec)}
		s.OnFeedback(&f, now, uint32(now/msec), &cfg)
	}
	step(0, 0)
	// Delivering 1 MB/s: the estimate rises to it.
	for i := 0; i < 5; i++ {
		step(100_000, 100_000)
	}
	if s.Capacity < 950_000 || s.Capacity > 1_050_000 {
		t.Fatalf("capacity %.0f after delivering 1 MB/s", s.Capacity)
	}
	// Barely used for a while: nothing is learned, nothing is forgotten.
	for i := 0; i < 20; i++ {
		step(2_000, 2_000)
	}
	if s.Capacity < 950_000 {
		t.Fatalf("capacity fell to %.0f while the path was merely idle", s.Capacity)
	}
	// Overdriven: we push 1 MB/s, only 300 kB/s arrives, the rest is not acknowledged.
	rxPkts = 0 // peer's packet counter is irrelevant here; avoid it running ahead of seq
	for i := 0; i < 15; i++ {
		for n := 100_000; n > 0; n -= 1000 {
			s.OnSend(1000, now)
		}
		now += 100 * msec
		rxBytes += 30_000
		acked := s.AckedSeq + 30 // the peer only got through 30 of the 100 datagrams
		f := wire.Feedback{EchoTS: uint32(now/msec) - 20, EchoDelay: 0, RxHighest: acked, RxPackets: acked, RxBytes: rxBytes, RxClock: uint32(now / msec)}
		s.OnFeedback(&f, now, uint32(now/msec), &cfg)
	}
	if s.Capacity > 400_000 {
		t.Fatalf("capacity still %.0f after 1.5s of delivering 300 kB/s under load", s.Capacity)
	}
}

func TestPolicyNames(t *testing.T) {
	for _, n := range []string{"bonded", "primary", "backup", "off"} {
		p, err := ParsePolicy(n)
		if err != nil || p.String() != n {
			t.Fatalf("%q: %v %v", n, p, err)
		}
	}
	if _, err := ParsePolicy("fast"); err == nil {
		t.Fatal("accepted an unknown policy")
	}
}
