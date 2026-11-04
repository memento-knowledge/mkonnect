package gitsync

import (
	"context"
	"sync"
	"testing"
	"time"
)

// acquireAsync starts an acquire and returns a channel that reports its outcome.
func acquireAsync(ctx context.Context, p *slotPool) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- p.acquire(ctx) }()
	return out
}

func expectPending(t *testing.T, ch <-chan bool, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s: acquire returned %v, want it to still be waiting", what, v)
	case <-time.After(30 * time.Millisecond):
	}
}

func expectAcquired(t *testing.T, ch <-chan bool, what string) {
	t.Helper()
	select {
	case v := <-ch:
		if !v {
			t.Fatalf("%s: acquire returned false, want a slot", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: acquire did not return", what)
	}
}

func TestSlotPoolLimitsHolders(t *testing.T) {
	p := newSlotPool(2)
	ctx := context.Background()
	for i := range 2 {
		if !p.acquire(ctx) {
			t.Fatalf("acquire %d of 2 must succeed", i+1)
		}
	}
	third := acquireAsync(ctx, p)
	expectPending(t, third, "third acquire at limit 2")
	p.release()
	expectAcquired(t, third, "third acquire after a release")
}

func TestSlotPoolGrowStartsWaitersWithoutAnyRelease(t *testing.T) {
	p := newSlotPool(1)
	ctx := context.Background()
	p.acquire(ctx)
	a, b := acquireAsync(ctx, p), acquireAsync(ctx, p)
	expectPending(t, a, "a at limit 1")
	p.resize(3)
	expectAcquired(t, a, "a after growing")
	expectAcquired(t, b, "b after growing")
}

func TestSlotPoolShrinkKeepsHoldersAndHoldsBackNewOnesUntilBelowTheLimit(t *testing.T) {
	p := newSlotPool(3)
	ctx := context.Background()
	for range 3 {
		p.acquire(ctx)
	}
	p.resize(1)
	w := acquireAsync(ctx, p)
	expectPending(t, w, "waiter with 3 held and limit 1")
	p.release() // 2 held
	expectPending(t, w, "waiter with 2 held and limit 1")
	p.release() // 1 held
	expectPending(t, w, "waiter with 1 held and limit 1")
	p.release() // 0 held
	expectAcquired(t, w, "waiter once below the limit")
}

func TestSlotPoolServesWaitersInArrivalOrder(t *testing.T) {
	p := newSlotPool(1)
	ctx := context.Background()
	p.acquire(ctx)
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.acquire(ctx)
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			p.release()
		}()
		// Let each goroutine queue before starting the next, so arrival order is defined.
		expectQueued(t, p, i+1)
	}
	p.release()
	wg.Wait()
	for i, v := range order {
		if v != i {
			t.Fatalf("grant order = %v, want arrival order", order)
		}
	}
}

func TestSlotPoolCancelledWaiterLeavesNoSlotAndNoStaleQueueEntry(t *testing.T) {
	p := newSlotPool(1)
	p.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	w := acquireAsync(ctx, p)
	expectPending(t, w, "waiter before cancel")
	cancel()
	if v := <-w; v {
		t.Fatal("a cancelled acquire reported success")
	}
	next := acquireAsync(context.Background(), p)
	p.release()
	expectAcquired(t, next, "a later waiter must get the freed slot, not a cancelled one")
	if p.held != 1 {
		t.Errorf("held = %d, want 1 (the later waiter's)", p.held)
	}
}

// Whatever order a grant and a cancellation land in, the pool must end with no leaked slot and no
// stalled waiter.
func TestSlotPoolGrantRacingCancellationNeverLeaksASlot(t *testing.T) {
	for range 200 {
		p := newSlotPool(1)
		p.acquire(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		w := acquireAsync(ctx, p)
		expectQueued(t, p, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); p.release() }()
		wg.Wait()
		got := <-w
		if got {
			p.release()
		}
		p.mu.Lock()
		held, waiting := p.held, p.waiters.Len()
		p.mu.Unlock()
		if held != 0 || waiting != 0 {
			t.Fatalf("after the race (acquired=%v): held=%d waiters=%d, want 0 and 0", got, held, waiting)
		}
	}
}

func expectQueued(t *testing.T, p *slotPool, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		l := p.waiters.Len()
		p.mu.Unlock()
		if l == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiters never reached %d", n)
}

func TestSlotPoolNeverDropsBelowOne(t *testing.T) {
	p := newSlotPool(0)
	if !p.acquire(context.Background()) {
		t.Fatal("a pool built with 0 must still allow one holder")
	}
	p.resize(-5)
	p.release()
	if !p.acquire(context.Background()) {
		t.Fatal("a pool resized below 1 must still allow one holder")
	}
}

// The two ways a wait can end by cancellation, driven directly so neither depends on scheduling.
func TestSlotPoolAbandonAfterTheSlotWasGrantedHandsItBack(t *testing.T) {
	p := newSlotPool(1)
	p.acquire(context.Background())

	first := &slotWaiter{ready: make(chan struct{})}
	second := &slotWaiter{ready: make(chan struct{})}
	p.mu.Lock()
	e1 := p.waiters.PushBack(first)
	p.waiters.PushBack(second)
	p.mu.Unlock()

	p.release() // grants the slot to first
	p.mu.Lock()
	granted := first.granted
	p.mu.Unlock()
	if !granted {
		t.Fatal("setup: the slot was not granted to the first waiter")
	}
	p.abandon(first, e1) // first was cancelled before it noticed

	p.mu.Lock()
	passedOn := second.granted
	held, queued := p.held, p.waiters.Len()
	p.mu.Unlock()
	if !passedOn {
		t.Error("the slot first gave up was not passed to the next waiter")
	}
	if held != 1 || queued != 0 {
		t.Errorf("held=%d queued=%d, want 1 held (by second) and nothing queued", held, queued)
	}
}

func TestSlotPoolAbandonBeforeAnyGrantLeavesTheQueue(t *testing.T) {
	p := newSlotPool(1)
	p.acquire(context.Background())
	w := &slotWaiter{ready: make(chan struct{})}
	p.mu.Lock()
	e := p.waiters.PushBack(w)
	p.mu.Unlock()

	p.abandon(w, e)
	p.release()

	p.mu.Lock()
	granted, held, queued := w.granted, p.held, p.waiters.Len()
	p.mu.Unlock()
	if granted || held != 0 || queued != 0 {
		t.Errorf("granted=%v held=%d queued=%d, want an abandoned waiter to be skipped and the pool empty", granted, held, queued)
	}
}
