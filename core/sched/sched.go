// Package sched holds what the bond knows about each path and decides which
// path(s) every datagram leaves on.
//
// The placement rule is "earliest arrival": a datagram goes to the path with
// the lowest
//
//	srtt/2 + queue
//
// where queue is how long the datagram would wait behind bytes already sent
// and not yet acknowledged, beyond what the path holds in flight anyway when
// running at its estimated capacity. At light load that is simply the
// lowest-latency path. As a path fills, its queue term grows and traffic
// spills to the next one, so at saturation each path carries what it can —
// which is aggregation. No rates need configuring.
package sched

import (
	"fmt"
	"time"

	"github.com/CarterCole/bond/core/wire"
)

// Policy is the user's intent for a path.
type Policy uint8

const (
	// PolicyBonded paths carry traffic whenever that gets a datagram there
	// sooner, after a small bias that lets primaries fill first.
	PolicyBonded Policy = iota
	// PolicyPrimary paths are preferred while healthy.
	PolicyPrimary
	// PolicyBackup paths stay idle (probes only) until no primary or bonded
	// path is healthy.
	PolicyBackup
	// PolicyOff paths are never used.
	PolicyOff
)

var policyNames = [...]string{"bonded", "primary", "backup", "off"}

func (p Policy) String() string {
	if int(p) < len(policyNames) {
		return policyNames[p]
	}
	return "unknown"
}

func ParsePolicy(s string) (Policy, error) {
	for i, n := range policyNames {
		if n == s {
			return Policy(i), nil
		}
	}
	return 0, fmt.Errorf("unknown path policy %q (want bonded, primary, backup or off)", s)
}

// Config tunes the scheduler. The zero value is not usable; start from Defaults.
type Config struct {
	SpillBias    time.Duration // head start primaries get over bonded paths
	LossPenalty  float64       // cost multiplier per unit of loss
	DegradedLoss float64       // loss above this makes a path unhealthy
	InitCapacity float64       // bytes/s assumed for a path nobody has loaded yet
	MinCapacity  float64       // bytes/s floor for the estimate
	Redundancy   int           // copies of each realtime datagram
	// Gain is how far past its estimated capacity a path may be driven
	// before the excess counts as queue. Above 1 so that the estimate can
	// discover headroom; the price is a standing queue of (Gain-1) × the
	// path's round trip when everything is saturated.
	Gain float64
	// FbLag is the usual delay before the peer's feedback about a datagram
	// can reach us, on top of the round trip.
	FbLag time.Duration
	// BulkQueueLimit protects realtime traffic: while realtime is flowing,
	// bulk datagrams are dropped rather than queued deeper than this on
	// every path. Dropping is how the inner TCP flows learn to slow down.
	BulkQueueLimit time.Duration
}

func Defaults() Config {
	return Config{
		SpillBias:      10 * time.Millisecond,
		LossPenalty:    8,
		DegradedLoss:   0.20,
		InitCapacity:   250_000, // 2 Mbit/s
		MinCapacity:    12_500,  // 100 kbit/s
		Redundancy:     2,
		Gain:           1.25,
		FbLag:          25 * time.Millisecond,
		BulkQueueLimit: 100 * time.Millisecond,
	}
}

const (
	ringSize = 4096 // sent-bytes history per path, in datagrams
	ringMask = ringSize - 1

	rateWindowMS   = 100
	lossWindowPkts = 25
	minRTTWindow   = int64(10 * time.Second)
	mtuGuess       = 1400
)

// State is everything known about one path, from one end. Times are
// nanoseconds on the owner's monotonic clock. Not safe for concurrent use.
type State struct {
	Policy Policy
	Up     bool

	// Transmit side.
	TxSeq   uint32 // next path seq
	TxBytes uint64
	TxPkts  uint64
	LastTx  int64
	ring    [ringSize]uint64 // TxBytes just after seq was sent

	// What the peer has told us.
	acked      bool
	AckedSeq   uint32
	ackedBytes uint64
	SRTT       time.Duration
	RTTVar     time.Duration
	MinRTT     time.Duration
	minCur     time.Duration
	minCurAt   int64
	minPrev    time.Duration
	Loss       float64 // smoothed, 0..1
	Capacity   float64 // bytes/s estimate; 0 = never measured
	Delivery   float64 // bytes/s the peer actually received over the last window
	LastFbRx   int64

	fbInit    bool
	fbHighest uint32
	fbPkts    uint32
	lossSent  uint32
	lossRecv  uint32
	lossSince int64
	lossInit  bool

	rateInit  bool
	rateClock uint32
	rateBytes uint64
	rateTx    uint64  // TxBytes when the current rate window opened
	rateAt    int64   // our clock when it opened
	Offered   float64 // bytes/s we sent over the last window

	lossySince int64 // when Loss first went over the degraded threshold; 0 = it is under

	// Receive side (feeds the feedback we send).
	rxAny       bool
	RxHighest   uint32
	RxPkts      uint32
	RxBytes     uint64
	RxMaxGlobal uint32
	rxGlobalAny bool
	LastRx      int64
	LastDataRx  int64
	LastFbTx    int64
	RxPending   bool // data arrived since the last feedback left
	echoTS      uint32
	echoAt      int64
}

// OnSend accounts for an n-byte datagram and returns its path seq.
func (s *State) OnSend(n int, now int64) uint32 {
	seq := s.TxSeq
	s.TxSeq++
	s.TxBytes += uint64(n)
	s.TxPkts++
	s.ring[seq&ringMask] = s.TxBytes
	s.LastTx = now
	return seq
}

// Inflight is the number of bytes sent that the peer has not yet reported seeing.
func (s *State) Inflight() uint64 {
	if !s.acked {
		return s.TxBytes
	}
	return s.TxBytes - s.ackedBytes
}

// OnRecv accounts for a datagram from the peer. newest reports whether it
// carries the highest path seq so far, which is what gates address roaming
// and metadata updates against replays.
//
// Only datagrams that advance the sequence count as signs of life: a replayed
// old datagram must not be able to keep a dead path looking alive.
func (s *State) OnRecv(h *wire.Header, n int, now int64) (newest bool) {
	s.RxPkts++
	s.RxBytes += uint64(n)
	if !s.rxAny || wire.SeqLess(s.RxHighest, h.PathSeq) {
		s.RxHighest = h.PathSeq
		s.echoTS = h.SendTS
		s.echoAt = now
		s.LastRx = now
		newest = true
	}
	s.rxAny = true
	if h.Type == wire.TypeData {
		s.RxPending = true
		if !s.rxGlobalAny || wire.SeqLess(s.RxMaxGlobal, h.GlobalSeq) {
			s.RxMaxGlobal = h.GlobalSeq
			s.rxGlobalAny = true
			s.LastDataRx = now
		}
	}
	return newest
}

// Acked reports whether the peer has seen path seq or something after it.
func (s *State) Acked(seq uint32) bool { return s.acked && !wire.SeqLess(s.AckedSeq, seq) }

// HasGlobal reports whether any data has arrived on this path.
func (s *State) HasGlobal() bool { return s.rxGlobalAny }

// MakeFeedback builds the report to send to the peer now.
func (s *State) MakeFeedback(now int64, nowMS uint32) wire.Feedback {
	f := wire.Feedback{
		EchoDelay: wire.NoEcho,
		RxHighest: s.RxHighest,
		RxPackets: s.RxPkts,
		RxBytes:   s.RxBytes,
		RxClock:   nowMS,
	}
	if s.rxAny {
		f.EchoTS = s.echoTS
		f.EchoDelay = uint32((now - s.echoAt) / int64(time.Millisecond))
	}
	s.LastFbTx = now
	s.RxPending = false
	return f
}

// OnFeedback digests the peer's report.
func (s *State) OnFeedback(f *wire.Feedback, now int64, nowMS uint32, cfg *Config) {
	s.LastFbRx = now

	if f.EchoDelay != wire.NoEcho {
		if rtt := int32(nowMS - f.EchoTS - f.EchoDelay); rtt >= 0 && rtt < 60_000 {
			s.rttSample(time.Duration(rtt)*time.Millisecond, now)
		}
	}

	if f.RxPackets == 0 {
		return // the peer has seen nothing yet; no counters to learn from
	}

	if !s.acked || wire.SeqLess(s.AckedSeq, f.RxHighest) {
		s.acked = true
		s.AckedSeq = f.RxHighest
		if behind := s.TxSeq - f.RxHighest; behind >= 1 && behind <= ringSize {
			s.ackedBytes = s.ring[f.RxHighest&ringMask]
		} else {
			s.ackedBytes = s.TxBytes
		}
	}

	// Loss: datagrams the peer counted versus path seqs it advanced over.
	if s.fbInit {
		dSent := int32(f.RxHighest - s.fbHighest)
		dRecv := f.RxPackets - s.fbPkts
		if dSent < 0 || dRecv > 1<<20 {
			// Counters went backwards: the peer restarted its accounting.
			s.lossSent, s.lossRecv = 0, 0
		} else {
			if s.lossSent == 0 && s.lossRecv == 0 {
				s.lossSince = now
			}
			s.lossSent += uint32(dSent)
			s.lossRecv += dRecv
			if s.lossSent >= lossWindowPkts || (s.lossSent > 0 && now-s.lossSince >= int64(time.Second)) {
				sample := 1 - float64(s.lossRecv)/float64(s.lossSent)
				sample = min(max(sample, 0), 1)
				if s.lossInit {
					s.Loss = 0.75*s.Loss + 0.25*sample
				} else {
					s.Loss, s.lossInit = sample, true
				}
				s.lossSent, s.lossRecv = 0, 0
				switch {
				case s.Loss <= cfg.DegradedLoss:
					s.lossySince = 0
				case s.lossySince == 0:
					s.lossySince = now
				}
			}
		}
	}
	s.fbInit, s.fbHighest, s.fbPkts = true, f.RxHighest, f.RxPackets

	// Delivery rate and capacity.
	if !s.rateInit || f.RxBytes < s.rateBytes {
		s.rateInit, s.rateClock, s.rateBytes = true, f.RxClock, f.RxBytes
		s.rateTx, s.rateAt = s.TxBytes, now
		return
	}
	if dt := int32(f.RxClock - s.rateClock); dt >= rateWindowMS {
		s.Delivery = float64(f.RxBytes-s.rateBytes) * 1000 / float64(dt)
		if local := now - s.rateAt; local > 0 {
			s.Offered = float64(s.TxBytes-s.rateTx) / (float64(local) / float64(time.Second))
		}
		s.rateClock, s.rateBytes = f.RxClock, f.RxBytes
		s.rateTx, s.rateAt = s.TxBytes, now
		s.updateCapacity(cfg)
	}
}

func (s *State) rttSample(rtt time.Duration, now int64) {
	if s.SRTT == 0 && s.RTTVar == 0 {
		s.SRTT, s.RTTVar = rtt, rtt/2
	} else {
		d := s.SRTT - rtt
		if d < 0 {
			d = -d
		}
		s.RTTVar = (3*s.RTTVar + d) / 4
		s.SRTT = (7*s.SRTT + rtt) / 8
	}
	// Windowed minimum over the last one to two windows.
	if s.minCurAt == 0 || now-s.minCurAt > minRTTWindow {
		if s.minCurAt != 0 {
			s.minPrev = s.minCur
		} else {
			s.minPrev = rtt
		}
		s.minCur, s.minCurAt = rtt, now
	} else if rtt < s.minCur {
		s.minCur = rtt
	}
	s.MinRTT = min(s.minCur, s.minPrev)
}

// updateCapacity moves the estimate after a fresh Delivery sample.
//
// It rises on evidence: the peer really received that much. It falls only on
// evidence about *this direction under load*: we offered the path more than
// it delivered. A path that is idle, lightly used, or merely slow to answer
// because the opposite direction is busy keeps the last thing learned about
// it — otherwise a download would talk the hub out of its uplinks.
func (s *State) updateCapacity(cfg *Config) {
	// Capacity stays 0 ("not measured") until there is evidence either way,
	// so a status display never passes the starting guess off as a reading.
	c := s.Capacity
	if c == 0 {
		c = cfg.InitCapacity
	}
	if s.Delivery > c {
		s.Capacity = s.Delivery
		return
	}
	// Overdriven: we pushed clearly more than came out the other end, and
	// there is a backlog to show for it beyond what the pipe itself holds.
	pipe := cfg.Gain*c*(s.MinRTT+cfg.FbLag).Seconds() + 3*mtuGuess
	overdriven := s.Offered > 1.15*s.Delivery && float64(s.Inflight()) > pipe
	if overdriven && s.Delivery < 0.9*c {
		s.Capacity = max(0.7*c+0.3*s.Delivery, cfg.MinCapacity)
	}
}

// EffCapacity is the capacity figure the scheduler works with.
func (s *State) EffCapacity(cfg *Config) float64 {
	if s.Capacity == 0 {
		return cfg.InitCapacity
	}
	return max(s.Capacity, cfg.MinCapacity)
}

// Queue estimates how long an n-byte datagram sent now would sit waiting.
// A path running at capacity always has about capacity × (round trip +
// feedback lag) bytes unacknowledged; only what exceeds that (with Gain of
// slack) is a queue. Without this allowance a long path would look
// congested merely for being long, and would never be filled.
func (s *State) Queue(n int, cfg *Config) time.Duration {
	c := s.EffCapacity(cfg)
	room := cfg.Gain * c * (s.MinRTT + cfg.FbLag).Seconds()
	excess := float64(s.Inflight()+uint64(n)) - room
	if excess <= 0 {
		return 0
	}
	return time.Duration(excess / c * float64(time.Second))
}

// Cost is the scheduler's estimate of when an n-byte datagram sent now
// would arrive, inflated for loss and biased by policy.
func (s *State) Cost(n int, cfg *Config) time.Duration {
	rtt := s.SRTT
	if rtt == 0 {
		rtt = time.Millisecond
	}
	eta := float64(rtt/2+s.Queue(n, cfg)) * (1 + cfg.LossPenalty*s.Loss)
	c := time.Duration(eta)
	if s.Policy == PolicyBonded {
		c += cfg.SpillBias
	}
	return c
}

// degradedAfter is how long loss must stay over the threshold before a path
// counts as unhealthy. A TCP flow probing for bandwidth overruns a path's
// queue for a moment every so often; that is the path working, not failing.
const degradedAfter = int64(2 * time.Second)

// Healthy paths are up and have not been shedding a large share of what
// they carry for a sustained period.
func (s *State) Healthy(now int64) bool {
	return s.Up && (s.lossySince == 0 || now-s.lossySince < degradedAfter)
}

// ResetPeer forgets everything learned from or about the peer's counters.
// Used when the peer announces that it restarted.
func (s *State) ResetPeer() {
	// Nothing sent before the reset counts as in flight any more.
	s.acked, s.AckedSeq, s.ackedBytes = true, s.TxSeq-1, s.TxBytes
	s.fbInit, s.rateInit, s.lossInit = false, false, false
	s.lossSent, s.lossRecv, s.lossySince = 0, 0, 0
	s.rxAny, s.rxGlobalAny, s.RxPending = false, false, false
	s.RxHighest, s.RxPkts, s.RxBytes, s.RxMaxGlobal = 0, 0, 0, 0
}

const (
	tierActive = iota // healthy primary or bonded
	tierBackup        // healthy backup
	tierSick          // up but degraded
	tierDown          // not up; last resort
	tierNever         // off
)

func (s *State) tier(now int64) int {
	switch {
	case s.Policy == PolicyOff:
		return tierNever
	case !s.Up:
		return tierDown
	case !s.Healthy(now):
		return tierSick
	case s.Policy == PolicyBackup:
		return tierBackup
	}
	return tierActive
}

// Pick chooses the paths an n-byte datagram of the given class leaves on,
// appending indexes into paths to out. It returns nothing when the datagram
// should be dropped (no usable path, or bulk shed to protect realtime).
//
// realtimeActive tells Pick that realtime traffic is currently flowing.
func Pick(paths []*State, class wire.Class, n int, now int64, cfg *Config, realtimeActive bool, out []int) []int {
	if class == wire.ClassControl {
		for i, p := range paths {
			if p.Up && p.Policy != PolicyOff {
				out = append(out, i)
			}
		}
		if len(out) == 0 {
			for i, p := range paths {
				if p.Policy != PolicyOff {
					out = append(out, i)
				}
			}
		}
		return out
	}

	want := 1
	if class == wire.ClassRealtime {
		want = max(cfg.Redundancy, 1)
	}
	base := len(out)
	for len(out)-base < want {
		best, bestTier, bestCost := -1, tierNever, int64(0)
		for i, p := range paths {
			if taken(out[base:], i) {
				continue
			}
			t := p.tier(now)
			if t == tierNever || t > bestTier {
				continue
			}
			var c int64
			if t == tierDown {
				c = now - p.LastRx // most recently alive first
			} else {
				c = int64(p.Cost(n, cfg))
			}
			if best < 0 || t < bestTier || c < bestCost {
				best, bestTier, bestCost = i, t, c
			}
		}
		if best < 0 {
			break
		}
		if len(out) > base && bestTier == tierDown {
			break // never spend a redundant copy on a dead path
		}
		if class == wire.ClassBulk && realtimeActive && bestTier != tierDown && cfg.BulkQueueLimit > 0 {
			if paths[best].Queue(n, cfg) > cfg.BulkQueueLimit {
				return out[:base]
			}
		}
		out = append(out, best)
	}
	return out
}

func taken(xs []int, i int) bool {
	for _, x := range xs {
		if x == i {
			return true
		}
	}
	return false
}
