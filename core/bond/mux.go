// Package bond is the multipath engine. A Mux takes WireGuard datagrams,
// stripes them over every usable path of a session, and on the far side
// stitches them back into one ordered stream. It plugs into wireguard-go as
// its conn.Bind, so WireGuard itself runs unmodified on top.
package bond

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/reorder"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/wire"
)

type Role uint8

const (
	// Hub is the end that owns the uplinks and opens paths.
	Hub Role = iota
	// Server is the aggregator: it learns paths from what arrives.
	Server
)

type Options struct {
	Role Role
	// Gen identifies this run of the process. It must be larger on every
	// restart so the peer knows to forget our old sequence numbers.
	Gen   uint64
	Sched sched.Config
	Log   *slog.Logger

	ProbeIdle  time.Duration // probe spacing on a path with nothing else to say
	FbInterval time.Duration // feedback spacing while data is arriving
	MinHold    time.Duration // reorder hold with a single path
	MaxHold    time.Duration // reorder hold ceiling
	Window     int           // reorder window, datagrams
	PathGC     time.Duration // server: forget a path silent this long
	MaxPaths   int           // server: paths per session

	// OnPath is called (off the hot path) when a path comes up, goes down,
	// appears or is removed.
	OnPath func(session uint32, p PathStatus, event string)
	// OnPeerRestart is called (off the hot path) when the far end of a
	// session turns out to be a new process. WireGuard on this side is still
	// holding keys the new process has never heard of and would only notice
	// after fifteen seconds of silence; this is the cue to handshake now.
	OnPeerRestart func(session uint32)
}

func (o *Options) fill() {
	if o.Sched == (sched.Config{}) {
		o.Sched = sched.Defaults()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Gen == 0 {
		o.Gen = uint64(time.Now().UnixMilli())
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.ProbeIdle, 250*time.Millisecond)
	def(&o.FbInterval, 25*time.Millisecond)
	def(&o.MinHold, 5*time.Millisecond)
	def(&o.MaxHold, 250*time.Millisecond)
	def(&o.PathGC, 60*time.Second)
	if o.Window == 0 {
		o.Window = 4096
	}
	if o.MaxPaths == 0 {
		o.MaxPaths = 32
	}
}

const (
	tickEvery      = 20 * time.Millisecond
	helloRetry     = int64(200 * time.Millisecond)
	helloRefresh   = int64(5 * time.Second)
	activeWindow   = int64(time.Second)            // a path "is delivering data" this long after its last datagram
	dataIdle       = int64(500 * time.Millisecond) // the sender's view of the same, deliberately shorter
	realtimeWindow = int64(500 * time.Millisecond)
	bringUpGap     = int64(5 * time.Millisecond)
	stashMax       = 4                      // handshake datagrams kept while there is no path at all
	stashTTL       = int64(4 * time.Second) // WireGuard retries after 5s anyway
	rxQueueLen     = 4096
)

type rxItem struct {
	b  *path.Buf
	ep *Endpoint
}

type Mux struct {
	opt   Options
	epoch time.Time

	mu       sync.RWMutex
	sessions map[uint32]*Session

	rxq chan rxItem

	bindMu     sync.Mutex
	bindOpen   bool
	bindClosed chan struct{}

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	RxUnknown atomic.Uint64 // datagrams for no session, wrong direction or bad tag
}

func New(opt Options) *Mux {
	opt.fill()
	m := &Mux{
		opt:      opt,
		epoch:    time.Now(),
		sessions: make(map[uint32]*Session),
		rxq:      make(chan rxItem, rxQueueLen),
		stop:     make(chan struct{}),
	}
	m.wg.Add(1)
	go m.run()
	return m
}

func (m *Mux) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	m.mu.Lock()
	for _, s := range m.sessions {
		s.close()
	}
	m.sessions = map[uint32]*Session{}
	m.mu.Unlock()
}

func (m *Mux) now() int64 { return int64(time.Since(m.epoch)) }

func ms(now int64) uint32 { return uint32(now / int64(time.Millisecond)) }

func (m *Mux) run() {
	defer m.wg.Done()
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	var list []*Session
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		list = list[:0]
		m.mu.RLock()
		for _, s := range m.sessions {
			list = append(list, s)
		}
		m.mu.RUnlock()
		now := m.now()
		for _, s := range list {
			s.tick(now)
		}
	}
}

// AddSession registers a hub↔aggregator session. Both ends must use the same
// id and key.
func (m *Mux) AddSession(id uint32, key [16]byte, name string) *Session {
	s := &Session{
		m:    m,
		ID:   id,
		Name: name,
		key:  wire.NewKey(key),
		ro:   reorder.New[*path.Buf](m.opt.Window, m.opt.MinHold),
	}
	s.ep = &Endpoint{m: m, id: id}
	if m.opt.Role == Server {
		s.dir = wire.FlagFromServer
	}
	s.timer = time.AfterFunc(time.Hour, s.onTimer)
	s.timer.Stop()
	m.mu.Lock()
	if old := m.sessions[id]; old != nil {
		old.close()
	}
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func (m *Mux) RemoveSession(id uint32) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (m *Mux) Session(id uint32) *Session {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	return s
}

// Path is one uplink of a session.
type Path struct {
	sched.State
	ID   uint8
	Name string
	Kind string

	remote path.Remote
	fixed  bool // hub side: the remote never roams

	created    int64
	awaitSince int64 // first data sent since we last heard anything; 0 = not waiting
	lastData   int64 // when we last sent data on this path
	// A path that has carried no data for a while is "warming" when it is
	// first used again: see Session.warm.
	warming    bool
	warmSeq    uint32
	warmAt     int64
	helloAcked bool
	lastHello  int64
	wentDown   int64
	inst       uint32 // hub: which incarnation of this id this is
	peerInst   uint32 // aggregator: the newest incarnation announced

	txErrs atomic.Uint64

	// Rates, refreshed once a second for status.
	rateAt         int64
	rateTx, rateRx uint64
	txBps, rxBps   float64
}

// Session is the bond between one hub and the aggregator.
type Session struct {
	m    *Mux
	ID   uint32
	Name string
	key  wire.Key
	ep   *Endpoint
	dir  uint8
	// Classes tells the session which class the next WireGuard datagrams
	// belong to; see ClassQueue.
	Classes ClassQueue

	mu      sync.Mutex
	dead    bool
	paths   []*Path
	states  []*sched.State // parallel to paths, for sched.Pick
	byID    [256]*Path
	txSeq   uint32
	ro      *reorder.Buffer[*path.Buf]
	timer   *time.Timer
	timerAt int64
	peerGen uint64

	lastRealtime int64
	nextInst     uint32
	stash        []stashed
	// holdExtra widens the reorder hold after datagrams arrive that had
	// already been given up on; it decays while that stops happening.
	holdExtra time.Duration
	seenLate  uint64
	extraAt   int64
	pickBuf   [16]int
	dlvBuf    []*path.Buf

	txPkts, txBytes, txDropped  uint64
	txClass                     [3]uint64 // datagrams sent, by class
	rxPkts, rxBytes, rxOverflow uint64
	rxBadTag                    atomic.Uint64
}

type stashed struct {
	payload []byte
	at      int64
}

type outPkt struct {
	r path.Remote
	b *path.Buf
	p *Path
}

func flush(out []outPkt) {
	for _, o := range out {
		if err := o.r.Send(o.b.Bytes()); err != nil {
			o.p.txErrs.Add(1)
		}
		path.Put(o.b)
	}
}

func (s *Session) close() {
	s.mu.Lock()
	s.dead = true
	s.timer.Stop()
	for _, b := range s.ro.Reset(nil) {
		path.Put(b)
	}
	s.mu.Unlock()
}

// AddPath opens a path towards remote. Hub side only: the aggregator learns
// its paths from what arrives.
func (s *Session) AddPath(id uint8, name, kind string, policy sched.Policy, remote path.Remote) {
	now := s.m.now()
	s.mu.Lock()
	if old := s.byID[id]; old != nil {
		s.removeLocked(old)
	}
	p := s.addLocked(id, name, kind, policy, remote, now)
	p.fixed = true
	s.nextInst++
	p.inst = s.nextInst
	var one [2]outPkt
	out := append(one[:0], s.buildHello(p, now), s.buildProbe(p, now))
	st := s.status(p, now)
	stash := s.stash
	s.stash = nil
	s.mu.Unlock()
	flush(out)
	for _, k := range stash {
		if now-k.at <= stashTTL {
			s.Send(k.payload, wire.ClassControl)
		}
	}
	s.event(st, "added")
}

func (s *Session) addLocked(id uint8, name, kind string, policy sched.Policy, remote path.Remote, now int64) *Path {
	p := &Path{ID: id, Name: name, Kind: kind, remote: remote, created: now, rateAt: now}
	p.Policy = policy
	s.byID[id] = p
	s.paths = append(s.paths, p)
	s.states = append(s.states, &p.State)
	return p
}

func (s *Session) removeLocked(p *Path) {
	s.byID[p.ID] = nil
	for i, q := range s.paths {
		if q == p {
			s.paths = append(s.paths[:i], s.paths[i+1:]...)
			s.states = append(s.states[:i], s.states[i+1:]...)
			break
		}
	}
}

func (s *Session) RemovePath(id uint8) {
	now := s.m.now()
	s.mu.Lock()
	p := s.byID[id]
	if p == nil {
		s.mu.Unlock()
		return
	}
	st := s.status(p, now)
	s.removeLocked(p)
	s.mu.Unlock()
	s.event(st, "removed")
}

// SetPolicy changes how a path may be used and tells the peer.
func (s *Session) SetPolicy(id uint8, policy sched.Policy) {
	s.mu.Lock()
	if p := s.byID[id]; p != nil && p.Policy != policy {
		p.Policy = policy
		p.helloAcked = false
		p.lastHello = 0
	}
	s.mu.Unlock()
}

// HasPath reports whether id is in use.
func (s *Session) HasPath(id uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id] != nil
}

func (s *Session) event(st PathStatus, ev string) {
	s.m.opt.Log.Info("path "+ev, "session", s.Name, "path", st.Name, "id", st.ID, "remote", st.Remote)
	if f := s.m.opt.OnPath; f != nil {
		go f(s.ID, st, ev)
	}
}

// Send schedules one WireGuard datagram onto the session's paths.
func (s *Session) Send(payload []byte, class wire.Class) {
	n := len(payload) + wire.HeaderLen
	if n > path.MaxPacket {
		return
	}
	m := s.m
	now := m.now()
	var arr [4]outPkt
	out := arr[:0]

	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	if class == wire.ClassRealtime {
		s.lastRealtime = now
	}
	rt := s.lastRealtime != 0 && now-s.lastRealtime < realtimeWindow
	idx := sched.Pick(s.states, class, n, now, &m.opt.Sched, rt, s.pickBuf[:0])
	if len(idx) == 0 {
		if class == wire.ClassControl && len(s.paths) == 0 {
			// A handshake with no path to leave on yet: at start-up the
			// tunnel can be asked to carry traffic a moment before the first
			// uplink is open. Keep it for that path rather than making
			// WireGuard wait out its five-second retry timer.
			if len(s.stash) == stashMax {
				s.stash = s.stash[1:]
			}
			s.stash = append(s.stash, stashed{append([]byte(nil), payload...), now})
		} else {
			s.txDropped++
		}
		s.mu.Unlock()
		return
	}
	if class == wire.ClassBulk && len(idx) == 1 {
		idx = s.warm(idx, n, now)
	}
	h := wire.Header{Type: wire.TypeData, Flags: s.dir, Class: class, Session: s.ID, GlobalSeq: s.txSeq, SendTS: ms(now)}
	s.txSeq++
	if len(idx) > 1 {
		h.Flags |= wire.FlagDup
	}
	for _, i := range idx {
		p := s.paths[i]
		p.lastData = now
		b := path.Get()
		b.N = wire.HeaderLen + copy(b.B[wire.HeaderLen:], payload)
		h.PathID = p.ID
		h.PathSeq = p.OnSend(b.N, now)
		h.Put(b.B[:])
		wire.Seal(&s.key, b.B[:b.N])
		if p.awaitSince == 0 {
			p.awaitSince = now
		}
		out = append(out, outPkt{p.remote, b, p})
	}
	s.txPkts++
	s.txBytes += uint64(len(payload))
	if int(class) < len(s.txClass) {
		s.txClass[class]++
	}
	s.mu.Unlock()
	flush(out)
}

// warm covers the moment a path that has been idle starts carrying data.
//
// The receiver gives up on a missing datagram as soon as every path it knows
// to be delivering data has moved past it. It cannot know that an idle path
// has just been put back to work until something arrives on it — and by
// then the first datagrams sent that way, still in flight, would already
// have been written off and would show up late and out of order. To a TCP
// flow that looks like loss, right when it is trying to speed up.
//
// So for the first round trip on a reawakened path, each datagram placed on
// it is also sent on a path that is already in use. The copy keeps the
// stream in order; the original announces the path. Once the peer reports
// seeing data there, the path carries its share alone.
func (s *Session) warm(idx []int, n int, now int64) []int {
	p := s.paths[idx[0]]
	if p.warming && (p.Acked(p.warmSeq) || now-p.warmAt > int64(2*p.SRTT)+int64(200*time.Millisecond)) {
		p.warming = false
	}
	if !p.warming && p.lastData != 0 && now-p.lastData <= dataIdle {
		return idx
	}
	// The cheapest other path that is already carrying data.
	buddy, best := -1, int64(0)
	for i, q := range s.paths {
		if q == p || !q.Up || q.warming || q.Policy == sched.PolicyOff || q.lastData == 0 || now-q.lastData > dataIdle {
			continue
		}
		if c := int64(q.Cost(n, &s.m.opt.Sched)); buddy < 0 || c < best {
			buddy, best = i, c
		}
	}
	if buddy < 0 {
		p.warming = false // nothing else is in use, so there is no order to protect
		return idx
	}
	if !p.warming {
		p.warming, p.warmSeq, p.warmAt = true, p.TxSeq, now
	}
	return append(idx, buddy)
}

func (s *Session) buildCtl(p *Path, typ wire.Type, payload []byte, now int64) outPkt {
	b := path.Get()
	b.N = wire.HeaderLen + copy(b.B[wire.HeaderLen:], payload)
	h := wire.Header{Type: typ, PathID: p.ID, Flags: s.dir, Class: wire.ClassControl, Session: s.ID, SendTS: ms(now)}
	h.PathSeq = p.OnSend(b.N, now)
	h.Put(b.B[:])
	wire.Seal(&s.key, b.B[:b.N])
	return outPkt{p.remote, b, p}
}

func (s *Session) buildProbe(p *Path, now int64) outPkt {
	var buf [wire.FeedbackLen]byte
	fb := p.MakeFeedback(now, ms(now))
	fb.Put(buf[:])
	return s.buildCtl(p, wire.TypeProbe, buf[:], now)
}

// helloMsg announces a path and the sender's generation.
type helloMsg struct {
	Gen    uint64 `json:"gen"`
	Ack    uint64 `json:"ack,omitempty"` // the peer generation this end has adopted
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Policy string `json:"policy,omitempty"`
	// Inst numbers the incarnations of a path id on the hub. An uplink that
	// goes away and comes back gets a new socket, and its sequence numbers
	// start over, under the same id; a higher Inst is how the aggregator
	// tells that apart from old traffic arriving late.
	Inst uint32 `json:"inst,omitempty"`
}

func (s *Session) buildHello(p *Path, now int64) outPkt {
	hm := helloMsg{Gen: s.m.opt.Gen, Ack: s.peerGen}
	if s.m.opt.Role == Hub {
		hm.Name, hm.Kind, hm.Policy, hm.Inst = p.Name, p.Kind, p.Policy.String(), p.inst
	}
	js, _ := json.Marshal(hm)
	p.lastHello = now
	return s.buildCtl(p, wire.TypeHello, js, now)
}

// Receive takes one datagram off a transport. It owns b from here on.
func (m *Mux) Receive(b *path.Buf, from path.Remote) {
	pkt := b.Bytes()
	h, err := wire.Parse(pkt)
	if err != nil {
		path.Put(b)
		m.RxUnknown.Add(1)
		return
	}
	s := m.Session(h.Session)
	if s == nil || (h.Flags&wire.FlagFromServer != 0) == (m.opt.Role == Server) {
		path.Put(b)
		m.RxUnknown.Add(1)
		return
	}
	if !wire.Verify(&s.key, pkt) {
		path.Put(b)
		s.rxBadTag.Add(1)
		m.RxUnknown.Add(1)
		return
	}

	now := m.now()
	var arr [4]outPkt
	out := arr[:0]
	var up *PathStatus
	kept := false

	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		path.Put(b)
		return
	}
	p := s.byID[h.PathID]
	if p == nil {
		if m.opt.Role != Server || len(s.paths) >= m.opt.MaxPaths {
			s.mu.Unlock()
			path.Put(b)
			return
		}
		p = s.addLocked(h.PathID, fmt.Sprintf("path%d", h.PathID), "", sched.PolicyBonded, from, now)
		// Tell the hub which generation of the aggregator it is talking to.
		out = append(out, s.buildHello(p, now))
	}
	// A hello that announces a restarted peer wipes what we knew about its
	// sequence numbers. Do that before accounting for the hello itself, so
	// that it is the first thing recorded in the fresh state and can be
	// echoed straight back.
	var hello helloMsg
	bumped := false
	if h.Type == wire.TypeHello {
		hello, bumped = s.readHello(p, pkt[wire.HeaderLen:], now)
	}
	newest := p.OnRecv(&h, len(pkt), now) || bumped
	if newest {
		p.awaitSince = 0
	}

	switch h.Type {
	case wire.TypeData:
		s.rxPkts++
		s.rxBytes += uint64(len(pkt) - wire.HeaderLen)
		res, ok := s.ro.Push(h.GlobalSeq, b, now, h.Class != wire.ClassBulk, s.dlvBuf[:0])
		kept = ok
		s.releaseLocked(res, now)
	case wire.TypeProbe:
		if fb, err := wire.ParseFeedback(pkt[wire.HeaderLen:]); err == nil {
			p.OnFeedback(&fb, now, ms(now), &m.opt.Sched)
			// A path only comes up on a round trip: the peer must echo
			// something we sent since the path was created or last died.
			// Replaying old traffic cannot fake that.
			since := ms(max(p.created, p.wentDown))
			if !p.Up && fb.EchoDelay != wire.NoEcho && int32(fb.EchoTS-since) >= 0 {
				p.Up = true
				st := s.status(p, now)
				up = &st
				// Echo straight back so the peer can confirm its side too.
				out = append(out, s.buildProbe(p, now))
			}
		}
	case wire.TypeHello:
		if hello.Gen != 0 && newest {
			s.onHello(p, &hello, now, &out)
		}
	}

	if newest && !p.fixed && p.remote != from {
		if p.Up {
			m.opt.Log.Info("path roamed", "session", s.Name, "path", p.Name, "to", from.String())
		}
		p.remote = from
	}
	since := now - p.LastFbTx
	if p.LastFbTx == 0 {
		since = int64(time.Hour) // never sent anything on this path: certainly due
	}
	switch {
	case p.RxPending && since >= int64(m.opt.FbInterval):
		// Acknowledge data promptly.
		out = append(out, s.buildProbe(p, now))
	case !p.Up && newest && since >= bringUpGap:
		// On a path that is not up yet any sign of life is worth an answer,
		// so both ends can complete a round trip without waiting for the
		// next idle probe. It ends as soon as both are up; the gap only
		// bounds the exchange if something keeps that from happening.
		out = append(out, s.buildProbe(p, now))
	}
	s.mu.Unlock()

	if !kept {
		path.Put(b)
	}
	flush(out)
	if up != nil {
		s.event(*up, "up")
	}
}

// readHello decodes a path announcement and, if it shows that the peer has
// restarted (or is being heard from for the first time), forgets everything
// derived from the peer's old sequence numbers. A hello from before the
// peer's last restart is a replay and comes back as the zero message.
func (s *Session) readHello(p *Path, payload []byte, now int64) (hm helloMsg, bumped bool) {
	if json.Unmarshal(payload, &hm) != nil || hm.Gen == 0 || hm.Gen < s.peerGen {
		return helloMsg{}, false
	}
	if hm.Gen == s.peerGen {
		// Same process at the far end. But is it the same path? A higher
		// instance number means the hub closed this path and opened a new
		// one under the same id: forget the old one's sequence numbers and
		// make the new one prove itself. (Lower or equal is old traffic.)
		if hm.Inst > p.peerInst {
			if p.peerInst != 0 {
				p.ResetPeer()
				if p.Up {
					p.Up = false
					p.wentDown = now
				}
				bumped = true
			}
			p.peerInst = hm.Inst
		}
		return hm, bumped
	}
	if s.peerGen != 0 {
		s.m.opt.Log.Info("peer restarted", "session", s.Name)
		if f := s.m.opt.OnPeerRestart; f != nil {
			go f(s.ID)
		}
	}
	for _, q := range s.paths {
		q.ResetPeer()
		q.helloAcked = false
		q.lastHello = 0
		q.peerInst = 0 // the new process numbers its paths from scratch
		// Every path has to prove itself against the new process. The ones
		// that led to the old process's sockets never will; until some path
		// is confirmed, handshakes go out on all of them, the new one included.
		if s.peerGen != 0 {
			q.Up = false
			q.wentDown = now
		}
	}
	for _, b := range s.ro.Reset(nil) {
		path.Put(b)
	}
	s.peerGen = hm.Gen
	p.peerInst = hm.Inst
	return hm, true
}

// onHello applies a path announcement that is current (not a replay).
func (s *Session) onHello(p *Path, hm *helloMsg, now int64, out *[]outPkt) {
	if s.m.opt.Role == Server {
		if hm.Name != "" {
			p.Name = hm.Name
		}
		p.Kind = hm.Kind
		if pol, err := sched.ParsePolicy(hm.Policy); err == nil {
			p.Policy = pol
		}
		*out = append(*out, s.buildHello(p, now))
	} else if hm.Ack == s.m.opt.Gen {
		p.helloAcked = true
	}
}

// floor is the lowest "newest datagram delivered" among paths that are
// currently carrying data. A missing datagram older than that is lost.
func (s *Session) floor(now int64) (f uint32, ok bool) {
	for _, p := range s.paths {
		if !p.HasGlobal() || now-p.LastDataRx > activeWindow {
			continue
		}
		if !ok || wire.SeqLess(p.RxMaxGlobal, f) {
			f, ok = p.RxMaxGlobal, true
		}
	}
	return f, ok
}

// releaseLocked hands res plus anything the reorder buffer is now willing to
// let go to WireGuard, in order, and re-arms the hold timer.
func (s *Session) releaseLocked(res []*path.Buf, now int64) {
	f, ok := s.floor(now)
	res, deadline := s.ro.Release(now, f, ok, res)
	for _, b := range res {
		select {
		case s.m.rxq <- rxItem{b, s.ep}:
		default:
			s.rxOverflow++
			path.Put(b)
		}
	}
	clear(res)
	s.dlvBuf = res[:0]
	if deadline != 0 && (s.timerAt == 0 || deadline < s.timerAt) {
		s.timerAt = deadline
		s.timer.Reset(time.Duration(deadline - now))
	}
}

func (s *Session) onTimer() {
	now := s.m.now()
	s.mu.Lock()
	s.timerAt = 0
	if !s.dead {
		s.releaseLocked(s.dlvBuf[:0], now)
	}
	s.mu.Unlock()
}

// downAfter is how long a path may stay silent before it is declared down.
func (s *Session) downAfter(p *Path, now int64) int64 {
	o := &s.m.opt
	rtt := int64(2*p.SRTT + 4*p.RTTVar)
	if p.awaitSince != 0 {
		// We are sending data and the peer answers data within FbInterval.
		t := 4*int64(o.FbInterval) + rtt
		return min(max(t, int64(300*time.Millisecond)), int64(800*time.Millisecond))
	}
	t := 3*int64(o.ProbeIdle) + rtt
	return min(max(t, int64(600*time.Millisecond)), int64(3*time.Second))
}

func (s *Session) tick(now int64) {
	o := &s.m.opt
	var arr [8]outPkt
	out := arr[:0]
	var events []struct {
		st PathStatus
		ev string
	}

	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	var minRTT, maxRTT, maxVar time.Duration
	active := 0
	for i := 0; i < len(s.paths); i++ {
		p := s.paths[i]

		if p.Up {
			silent := now - p.LastRx
			if p.awaitSince != 0 {
				silent = min(silent, now-p.awaitSince)
			}
			if silent > s.downAfter(p, now) {
				p.Up = false
				p.wentDown = now
				events = append(events, struct {
					st PathStatus
					ev string
				}{s.status(p, now), "down"})
			}
		}
		gc := int64(o.PathGC)
		if p.wentDown == 0 && !p.Up {
			gc = min(gc, int64(10*time.Second)) // never completed a round trip
		}
		if o.Role == Server && now-max(p.LastRx, p.created) > gc {
			events = append(events, struct {
				st PathStatus
				ev string
			}{s.status(p, now), "removed"})
			s.removeLocked(p)
			i--
			continue
		}

		if o.Role == Hub && ((!p.helloAcked && now-p.lastHello >= helloRetry) || now-p.lastHello >= helloRefresh) {
			if p.Policy != sched.PolicyOff || !p.helloAcked {
				out = append(out, s.buildHello(p, now))
			}
		}
		if p.Policy != sched.PolicyOff {
			due := now-p.LastFbTx >= int64(o.ProbeIdle)
			if p.RxPending && now-p.LastFbTx >= int64(o.FbInterval) {
				due = true
			}
			// The aggregator only talks on paths the hub has opened towards it.
			if due && (o.Role == Hub || p.Up) {
				out = append(out, s.buildProbe(p, now))
			}
		}

		if p.Up && p.HasGlobal() && now-p.LastDataRx <= activeWindow && p.SRTT > 0 {
			if active == 0 || p.SRTT < minRTT {
				minRTT = p.SRTT
			}
			maxRTT = max(maxRTT, p.SRTT)
			maxVar = max(maxVar, p.RTTVar)
			active++
		}

		if now-p.rateAt >= int64(time.Second) {
			dt := float64(now-p.rateAt) / float64(time.Second)
			p.txBps = float64(p.TxBytes-p.rateTx) * 8 / dt
			p.rxBps = float64(p.RxBytes-p.rateRx) * 8 / dt
			p.rateAt, p.rateTx, p.rateRx = now, p.TxBytes, p.RxBytes
		}
	}
	// Hold long enough to cover the latency spread between the paths that
	// are delivering; with one path there is nothing to wait for.
	hold := o.MinHold
	if active > 1 {
		// Arrivals that had already been skipped mean the hold was too
		// short for what the paths are really doing: back off quickly,
		// creep back slowly.
		if late := s.ro.Late; late != s.seenLate {
			s.seenLate = late
			s.holdExtra = min(s.holdExtra*3/2+10*time.Millisecond, o.MaxHold)
			s.extraAt = now
		} else if s.holdExtra > 0 && now-s.extraAt >= int64(time.Second) {
			s.holdExtra = s.holdExtra * 9 / 10
			if s.holdExtra < time.Millisecond {
				s.holdExtra = 0
			}
			s.extraAt = now
		}
		hold = (maxRTT-minRTT)*3/4 + 2*maxVar + o.MinHold + s.holdExtra
		hold = min(max(hold, o.MinHold), o.MaxHold)
	} else {
		s.holdExtra, s.seenLate = 0, s.ro.Late
	}
	s.ro.Hold = hold
	if s.ro.Held() > 0 {
		s.releaseLocked(s.dlvBuf[:0], now)
	}
	s.mu.Unlock()

	flush(out)
	for _, e := range events {
		s.event(e.st, e.ev)
	}
}
