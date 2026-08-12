package clock

import (
	"testing"
	"time"
)

func TestManualClockAfterFiresOnAdvance(t *testing.T) {
	start := time.Unix(1700000000, 0)
	clk := NewManualClock(start)
	ch := clk.After(5 * time.Second)

	clk.Advance(5*time.Second - time.Nanosecond)
	select {
	case got := <-ch:
		t.Fatalf("timer fired early at %v", got)
	default:
	}

	clk.Advance(time.Nanosecond)
	select {
	case got := <-ch:
		if !got.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("timer fired at %v, want %v", got, start.Add(5*time.Second))
		}
	default:
		t.Fatal("timer did not fire at deadline")
	}
	select {
	case <-ch:
		t.Fatal("one-shot timer fired more than once")
	default:
	}
}

func TestManualClockTickerFiresAndStops(t *testing.T) {
	start := time.Unix(1700000000, 0)
	clk := NewManualClock(start)
	ticker := clk.NewTicker(2 * time.Second)

	clk.Advance(2 * time.Second)
	got := <-ticker.C()
	if !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("first tick at %v, want %v", got, start.Add(2*time.Second))
	}

	clk.Advance(2 * time.Second)
	got = <-ticker.C()
	if !got.Equal(start.Add(4 * time.Second)) {
		t.Fatalf("second tick at %v, want %v", got, start.Add(4*time.Second))
	}

	ticker.Stop()
	clk.Advance(2 * time.Second)
	select {
	case got := <-ticker.C():
		t.Fatalf("ticker fired after Stop at %v", got)
	default:
	}
}

func TestManualClockSleepUnblocksAtDeadline(t *testing.T) {
	start := time.Unix(1700000000, 0)
	clk := NewManualClock(start)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		clk.Sleep(3 * time.Second)
		close(done)
	}()
	<-started
	clk.Pump()

	clk.Advance(2999 * time.Millisecond)
	clk.Pump()
	select {
	case <-done:
		t.Fatal("Sleep unblocked before deadline")
	default:
	}

	clk.Advance(time.Millisecond)
	clk.Pump()
	if !clk.WaitSettled(func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("Sleep did not unblock at deadline")
	}
}

func TestSystemClockSatisfiesClock(t *testing.T) {
	var _ Clock = SystemClock{}
}
