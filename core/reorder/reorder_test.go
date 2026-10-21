package reorder

import (
	"math/rand"
	"slices"
	"testing"
	"time"
)

const ms = int64(time.Millisecond)

func push(t *testing.T, b *Buffer[uint32], seq uint32, now int64) []uint32 {
	t.Helper()
	out, _ := b.Push(seq, seq, now, false, nil)
	return out
}

func TestInOrderPassesStraightThrough(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	for s := uint32(100); s < 200; s++ {
		if got := push(t, b, s, 0); !slices.Equal(got, []uint32{s}) {
			t.Fatalf("seq %d: got %v", s, got)
		}
	}
	if b.Held() != 0 {
		t.Fatal("nothing should be held")
	}
}

func TestReordersAcrossGap(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	if got := push(t, b, 3, 0); len(got) != 0 {
		t.Fatalf("3 should wait for 2, got %v", got)
	}
	if got := push(t, b, 4, 0); len(got) != 0 {
		t.Fatalf("4 should wait, got %v", got)
	}
	if got := push(t, b, 2, ms); !slices.Equal(got, []uint32{2, 3, 4}) {
		t.Fatalf("got %v", got)
	}
}

func TestHoldTimeoutSkipsGapThenDeliversLate(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	push(t, b, 3, 10*ms)
	out, dl := b.Release(20*ms, 0, false, nil)
	if len(out) != 0 || dl != 60*ms {
		t.Fatalf("early release: out=%v deadline=%d", out, dl)
	}
	out, dl = b.Release(60*ms, 0, false, nil)
	if !slices.Equal(out, []uint32{3}) || dl != 0 {
		t.Fatalf("timeout release: out=%v deadline=%d", out, dl)
	}
	// 2 shows up after all: delivered once, late.
	if got := push(t, b, 2, 70*ms); !slices.Equal(got, []uint32{2}) {
		t.Fatalf("late: %v", got)
	}
	if _, ok := b.Push(2, 2, 71*ms, false, nil); ok {
		t.Fatal("second late copy must be dropped")
	}
	if b.Late != 1 || b.Skipped != 1 || b.Dups != 1 {
		t.Fatalf("late=%d skipped=%d dups=%d", b.Late, b.Skipped, b.Dups)
	}
}

func TestFloorSkipsLostWithoutWaiting(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	push(t, b, 3, 0)
	push(t, b, 4, 0)
	// Every live path has delivered up to 4, so 2 is gone.
	out, dl := b.Release(ms, 4, true, nil)
	if !slices.Equal(out, []uint32{3, 4}) || dl != 0 {
		t.Fatalf("out=%v deadline=%d", out, dl)
	}
}

func TestFloorBelowGapKeepsWaiting(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	push(t, b, 3, 0)
	// The slow path has only reached 1; 2 may still be in flight on it.
	if out, dl := b.Release(ms, 1, true, nil); len(out) != 0 || dl == 0 {
		t.Fatalf("out=%v deadline=%d", out, dl)
	}
}

func TestDuplicatesDropped(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	if _, ok := b.Push(1, 1, 0, false, nil); ok {
		t.Fatal("dup of delivered accepted")
	}
	push(t, b, 3, 0)
	if _, ok := b.Push(3, 3, 0, false, nil); ok {
		t.Fatal("dup of held accepted")
	}
}

func TestBypassDeliversImmediatelyAndFillsGap(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	out, ok := b.Push(3, 3, 0, true, nil)
	if !ok || !slices.Equal(out, []uint32{3}) {
		t.Fatalf("bypass: %v %v", out, ok)
	}
	push(t, b, 4, 0) // held behind gap 2
	// 2 arrives: 2 goes out, 3 is already done, 4 follows.
	if got := push(t, b, 2, ms); !slices.Equal(got, []uint32{2, 4}) {
		t.Fatalf("got %v", got)
	}
}

func TestWrapAround(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	start := ^uint32(0) - 5
	push(t, b, start, 0)
	push(t, b, start+2, 0)
	var got []uint32
	got = append(got, push(t, b, start+1, 0)...)
	for s := start + 3; s != start+20; s++ {
		got = append(got, push(t, b, s, 0)...)
	}
	for i, v := range got {
		if v != start+1+uint32(i) {
			t.Fatalf("order broken at %d: %v", i, got)
		}
	}
	if len(got) != 19 {
		t.Fatalf("delivered %d", len(got))
	}
}

func TestFarJumpResyncs(t *testing.T) {
	b := New[uint32](256, 50*time.Millisecond)
	push(t, b, 1, 0)
	push(t, b, 3, 0)
	got := push(t, b, 1_000_000, 0)
	if !slices.Equal(got, []uint32{3, 1_000_000}) {
		t.Fatalf("got %v", got)
	}
	if got := push(t, b, 1_000_001, 0); !slices.Equal(got, []uint32{1_000_001}) {
		t.Fatalf("after resync: %v", got)
	}
}

func TestTooOldDropped(t *testing.T) {
	b := New[uint32](64, 50*time.Millisecond)
	for s := uint32(0); s < 1000; s++ {
		push(t, b, s, 0)
	}
	if _, ok := b.Push(10, 10, 0, false, nil); ok {
		t.Fatal("ancient seq accepted")
	}
	if b.TooOld != 1 {
		t.Fatalf("TooOld=%d", b.TooOld)
	}
}

// Shuffle within a bounded distance and check that everything comes out
// exactly once and in order when no hold timeout fires.
func TestRandomReorderExactlyOnceInOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const n = 20000
	seqs := make([]uint32, n)
	for i := range seqs {
		seqs[i] = uint32(i) + 0xfffff000 // crosses the wrap
	}
	// The first datagram seen anchors the sequence space, so leave block 0 alone.
	for i := 16; i < n; i += 16 {
		end := min(i+16, n)
		rng.Shuffle(end-i, func(a, c int) { seqs[i+a], seqs[i+c] = seqs[i+c], seqs[i+a] })
	}
	b := New[uint32](256, time.Hour)
	var got []uint32
	for _, s := range seqs {
		got, _ = b.Push(s, s, 0, false, got)
		if rng.Intn(4) == 0 { // sprinkle duplicates
			got, _ = b.Push(s, s, 0, false, got)
		}
	}
	if len(got) != n {
		t.Fatalf("delivered %d of %d", len(got), n)
	}
	for i, v := range got {
		if v != uint32(i)+0xfffff000 {
			t.Fatalf("out of order at %d", i)
		}
	}
}
