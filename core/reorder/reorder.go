// Package reorder puts datagrams that were striped across paths back into
// the order they were sent, without ever waiting longer than it has to.
//
// A gap at the head of the queue is given up on when either
//   - every path that is currently delivering data has already delivered
//     something newer (so the missing datagram is lost, not late), or
//   - the datagram waiting behind the gap has been held for Hold.
//
// Late arrivals are still delivered (out of order) exactly once; duplicates
// from redundant sending are dropped.
package reorder

import "time"

const (
	empty uint8 = iota
	holding
	done
)

// Buffer is not safe for concurrent use.
type Buffer[T any] struct {
	Hold time.Duration

	w, mask uint32 // w seqs ahead of next and w behind it are tracked; ring is 2w
	state   []uint8
	items   []T
	at      []int64

	next    uint32 // lowest seq not yet delivered or given up on
	high    uint32 // one past the highest seq seen
	started bool
	held    int

	Delivered, Dups, Late, Skipped, TooOld uint64
}

// New returns a buffer tracking window seqs either side of the head.
// window is rounded up to a power of two.
func New[T any](window int, hold time.Duration) *Buffer[T] {
	w := uint32(64)
	for int(w) < window {
		w <<= 1
	}
	return &Buffer[T]{
		Hold:  hold,
		w:     w,
		mask:  2*w - 1,
		state: make([]uint8, 2*w),
		items: make([]T, 2*w),
		at:    make([]int64, 2*w),
	}
}

// Reset forgets everything; the next Push starts a new sequence space.
// Held items are appended to out so the caller can release them.
func (b *Buffer[T]) Reset(out []T) []T {
	out = b.flush(out)
	clear(b.state)
	b.started = false
	return out
}

// Held is the number of datagrams waiting behind a gap.
func (b *Buffer[T]) Held() int { return b.held }

// Push offers one datagram. Datagrams that are now deliverable are appended
// to out in delivery order. ok is false if item was a duplicate or too old,
// in which case the caller still owns it.
//
// bypass delivers item immediately regardless of gaps (realtime class).
func (b *Buffer[T]) Push(seq uint32, item T, now int64, bypass bool, out []T) (_ []T, ok bool) {
	if !b.started {
		b.started = true
		b.next, b.high = seq, seq
	}
	d := int32(seq - b.next)
	switch {
	case d < 0:
		if d < -int32(b.w) {
			b.TooOld++
			return out, false
		}
		i := seq & b.mask
		if b.state[i] == done {
			b.Dups++
			return out, false
		}
		b.state[i] = done
		b.Late++
		b.Delivered++
		return append(out, item), true
	case d >= int32(b.w):
		// Far beyond the window: the sender jumped. Deliver what we have and resync.
		out = b.flush(out)
		clear(b.state)
		b.next, b.high = seq, seq
		d = 0
	}

	i := seq & b.mask
	if b.state[i] != empty {
		b.Dups++
		return out, false
	}
	if int32(seq-b.high) >= 0 {
		b.high = seq + 1
	}
	if bypass || d == 0 {
		b.state[i] = done
		b.Delivered++
		out = append(out, item)
	} else {
		b.state[i] = holding
		b.items[i] = item
		b.at[i] = now
		b.held++
	}
	return b.drain(out), true
}

// Release gives up on gaps that can no longer be filled in time and appends
// what that frees to out. floor, when valid, is the lowest "newest seq
// delivered" across the paths that are actively carrying data: anything
// below it that is still missing is lost. The returned deadline is when
// Release should be called again (0 = nothing is waiting).
func (b *Buffer[T]) Release(now int64, floor uint32, haveFloor bool, out []T) (_ []T, deadline int64) {
	for b.held > 0 {
		if b.state[b.next&b.mask] != empty {
			out = b.drain(out)
			continue
		}
		if haveFloor && int32(floor-b.next) > 0 {
			b.Skipped++
			b.advance()
			out = b.drain(out)
			continue
		}
		s := b.next + 1
		for b.state[s&b.mask] != holding {
			s++
		}
		dl := b.at[s&b.mask] + int64(b.Hold)
		if dl > now {
			return out, dl
		}
		for b.next != s {
			if b.state[b.next&b.mask] == empty {
				b.Skipped++
			}
			b.advance()
		}
		out = b.drain(out)
	}
	return out, 0
}

// drain delivers the contiguous run at the head.
func (b *Buffer[T]) drain(out []T) []T {
	for int32(b.high-b.next) > 0 {
		i := b.next & b.mask
		switch b.state[i] {
		case holding:
			out = append(out, b.items[i])
			var zero T
			b.items[i] = zero
			b.state[i] = done
			b.held--
			b.Delivered++
		case done:
		default:
			return out
		}
		b.advance()
	}
	return out
}

func (b *Buffer[T]) advance() {
	b.next++
	// The seq that just left the back of the window shares a slot with the
	// one that just entered the front.
	b.state[(b.next+b.w-1)&b.mask] = empty
}

func (b *Buffer[T]) flush(out []T) []T {
	for s := b.next; b.held > 0 && int32(b.high-s) > 0; s++ {
		i := s & b.mask
		if b.state[i] == holding {
			out = append(out, b.items[i])
			var zero T
			b.items[i] = zero
			b.state[i] = done
			b.held--
			b.Delivered++
		}
	}
	return out
}
