package bond

import (
	"sort"
	"time"
)

// PathStatus is a snapshot of one path for humans and dashboards.
type PathStatus struct {
	ID          uint8   `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Policy      string  `json:"policy"`
	Up          bool    `json:"up"`
	Remote      string  `json:"remote"`
	RTTMs       float64 `json:"rtt_ms"`
	MinRTTMs    float64 `json:"min_rtt_ms"`
	Loss        float64 `json:"loss"`
	CapacityBps float64 `json:"capacity_bps"`
	TxBps       float64 `json:"tx_bps"`
	RxBps       float64 `json:"rx_bps"`
	TxBytes     uint64  `json:"tx_bytes"`
	RxBytes     uint64  `json:"rx_bytes"`
	Inflight    uint64  `json:"inflight_bytes"`
	TxErrors    uint64  `json:"tx_errors"`
}

// SessionStatus is a snapshot of one bonded session.
type SessionStatus struct {
	ID        uint32       `json:"id"`
	Name      string       `json:"name"`
	Paths     []PathStatus `json:"paths"`
	TxBytes   uint64       `json:"tx_bytes"`
	RxBytes   uint64       `json:"rx_bytes"`
	TxPackets uint64       `json:"tx_packets"`
	RxPackets uint64       `json:"rx_packets"`
	TxDropped uint64       `json:"tx_dropped"`
	// Datagrams sent in each class. Realtime and control datagrams go out
	// on more than one path.
	TxBulk     uint64  `json:"tx_bulk"`
	TxRealtime uint64  `json:"tx_realtime"`
	TxControl  uint64  `json:"tx_control"`
	HoldMs     float64 `json:"reorder_hold_ms"`
	Held       int     `json:"reorder_held"`
	Reordered  uint64  `json:"reorder_late"`
	Skipped    uint64  `json:"reorder_skipped"`
	Dups       uint64  `json:"duplicates_dropped"`
	BadTag     uint64  `json:"bad_tag"`
}

func msf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// status must be called with s.mu held.
func (s *Session) status(p *Path, _ int64) PathStatus {
	st := PathStatus{
		ID:       p.ID,
		Name:     p.Name,
		Kind:     p.Kind,
		Policy:   p.Policy.String(),
		Up:       p.Up,
		RTTMs:    msf(p.SRTT),
		MinRTTMs: msf(p.MinRTT),
		Loss:     p.Loss,
		TxBps:    p.txBps,
		RxBps:    p.rxBps,
		TxBytes:  p.TxBytes,
		RxBytes:  p.RxBytes,
		Inflight: p.Inflight(),
		TxErrors: p.txErrs.Load(),
	}
	if p.remote.S != nil {
		st.Remote = p.remote.String()
	}
	if p.Capacity > 0 {
		st.CapacityBps = p.Capacity * 8
	}
	return st
}

// Status snapshots one session.
func (s *Session) Status() SessionStatus {
	now := s.m.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SessionStatus{
		ID:         s.ID,
		Name:       s.Name,
		TxBytes:    s.txBytes,
		RxBytes:    s.rxBytes,
		TxPackets:  s.txPkts,
		RxPackets:  s.rxPkts,
		TxDropped:  s.txDropped,
		TxBulk:     s.txClass[0],
		TxRealtime: s.txClass[1],
		TxControl:  s.txClass[2],
		HoldMs:     msf(s.ro.Hold),
		Held:       s.ro.Held(),
		Reordered:  s.ro.Late,
		Skipped:    s.ro.Skipped,
		Dups:       s.ro.Dups,
		BadTag:     s.rxBadTag.Load(),
		Paths:      make([]PathStatus, 0, len(s.paths)),
	}
	for _, p := range s.paths {
		st.Paths = append(st.Paths, s.status(p, now))
	}
	sort.Slice(st.Paths, func(i, j int) bool { return st.Paths[i].ID < st.Paths[j].ID })
	return st
}

// Snapshot returns the status of every session, ordered by id.
func (m *Mux) Snapshot() []SessionStatus {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()
	out := make([]SessionStatus, 0, len(list))
	for _, s := range list {
		out = append(out, s.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
