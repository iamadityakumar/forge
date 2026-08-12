package clock

import (
	"container/heap"
	"runtime"
	"sync"
	"time"
)

type timerEvent struct {
	fireAt  time.Time
	period  time.Duration // 0 for one-shot; >0 for ticker
	ch      chan time.Time
	stopped bool
	seq     int64 // tiebreaker for stable ordering
	index   int   // heap index
}

type timerFire struct {
	ch chan time.Time
	at time.Time
}

type timerHeap []*timerEvent

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].fireAt.Equal(h[j].fireAt) {
		return h[i].seq < h[j].seq
	}
	return h[i].fireAt.Before(h[j].fireAt)
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *timerHeap) Push(x any) {
	n := len(*h)
	item := x.(*timerEvent)
	item.index = n
	*h = append(*h, item)
}
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[:n-1]
	return item
}

// manualTicker implements Ticker backed by the ManualClock's event queue.
type manualTicker struct {
	clk *ManualClock
	ev  *timerEvent
}

func (m *manualTicker) C() <-chan time.Time { return m.ev.ch }
func (m *manualTicker) Stop() {
	m.clk.mu.Lock()
	defer m.clk.mu.Unlock()
	m.ev.stopped = true
}

// ManualClock is a thread-safe virtual clock with timer support.
type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	heap    timerHeap
	nextSeq int64
}

// NewManualClock creates a ManualClock starting at time t.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t}
}

// Now returns the current virtual time.
func (m *ManualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// After returns a channel that fires once after duration d in virtual time.
func (m *ManualClock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan time.Time, 1)
	m.scheduleLocked(&timerEvent{
		fireAt: m.now.Add(d),
		ch:     ch,
		seq:    m.nextSeq,
	})
	m.nextSeq++
	return ch
}

// NewTicker returns a Ticker that fires every d in virtual time until Stop().
func (m *ManualClock) NewTicker(d time.Duration) Ticker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		d = time.Nanosecond
	}
	ev := &timerEvent{
		fireAt: m.now.Add(d),
		period: d,
		ch:     make(chan time.Time, 1),
		seq:    m.nextSeq,
	}
	m.nextSeq++
	m.scheduleLocked(ev)
	return &manualTicker{clk: m, ev: ev}
}

// Sleep blocks the calling goroutine until d has elapsed in virtual time.
func (m *ManualClock) Sleep(d time.Duration) {
	<-m.After(d)
}

// Advance moves the clock forward by d, firing all due timers in order.
func (m *ManualClock) Advance(d time.Duration) {
	m.mu.Lock()
	newNow := m.now.Add(d)
	m.now = newNow
	var fires []timerFire
	for m.heap.Len() > 0 && !m.heap[0].fireAt.After(newNow) {
		ev := heap.Pop(&m.heap).(*timerEvent)
		if ev.stopped {
			continue
		}
		fireAt := ev.fireAt
		if ev.period > 0 {
			ev.fireAt = ev.fireAt.Add(ev.period)
			heap.Push(&m.heap, ev)
		}
		fires = append(fires, timerFire{ch: ev.ch, at: fireAt})
	}
	m.mu.Unlock()

	for _, fire := range fires {
		// Non-blocking send avoids deadlocking Advance when a ticker consumer is
		// slow. This matches time.Ticker's documented dropped-tick behavior.
		select {
		case fire.ch <- fire.at:
		default:
		}
	}
}

// Pump runs a bounded Gosched loop to let timer goroutines run,
// with a small real sleep to ensure they get scheduled.
func (m *ManualClock) Pump() {
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	// Small real sleep ensures goroutines waiting on timers get CPU time
	// to process fired events and make progress.
	time.Sleep(2 * time.Millisecond)
}

// WaitSettled polls pred up to 1000 times with Gosched, returning true when it
// observes the desired state.
func (m *ManualClock) WaitSettled(pred func() bool) bool {
	for i := 0; i < 1000; i++ {
		if pred() {
			return true
		}
		runtime.Gosched()
	}
	return pred()
}

func (m *ManualClock) scheduleLocked(ev *timerEvent) {
	heap.Push(&m.heap, ev)
}
