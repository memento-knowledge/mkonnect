package gitsync

import (
	"container/list"
	"context"
	"sync"
)

// slotPool is a counting semaphore whose capacity can change while it is in use. Waiters are served
// in arrival order. Shrinking never revokes a slot that is held: it only stops handing out new ones
// until enough are returned to fall below the new capacity.
type slotPool struct {
	mu      sync.Mutex
	limit   int
	held    int
	waiters list.List // of *slotWaiter, oldest first
}

type slotWaiter struct {
	ready   chan struct{} // closed when the slot has been granted
	granted bool          // guarded by slotPool.mu
}

func newSlotPool(limit int) *slotPool {
	return &slotPool{limit: max(limit, 1)}
}

// acquire waits for a slot and reports whether it got one; false means ctx ended first and no slot
// is held. A slot is held until release is called.
func (p *slotPool) acquire(ctx context.Context) bool {
	p.mu.Lock()
	if p.waiters.Len() == 0 && p.held < p.limit {
		p.held++
		p.mu.Unlock()
		return true
	}
	w := &slotWaiter{ready: make(chan struct{})}
	elem := p.waiters.PushBack(w)
	p.mu.Unlock()

	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if w.granted {
		// The grant raced the cancellation: the slot is ours, so hand it on through the normal path.
		p.held--
		p.grantLocked()
		return false
	}
	p.waiters.Remove(elem)
	return false
}

// release returns a slot.
func (p *slotPool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held--
	p.grantLocked()
}

// resize changes the capacity (to at least 1). Growing starts waiting acquirers at once.
func (p *slotPool) resize(limit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limit = max(limit, 1)
	p.grantLocked()
}

// grantLocked hands slots to the oldest waiters while there is room. p.mu must be held.
func (p *slotPool) grantLocked() {
	for p.held < p.limit {
		front := p.waiters.Front()
		if front == nil {
			return
		}
		w := p.waiters.Remove(front).(*slotWaiter)
		w.granted = true
		p.held++
		close(w.ready)
	}
}
