package session

import "testing"

func TestDropStaleCacheOnPRSwitch(t *testing.T) {
	var calls []int
	prev := InvalidatePRCache
	InvalidatePRCache = func(number int) {
		calls = append(calls, number)
	}
	t.Cleanup(func() { InvalidatePRCache = prev })

	pr := func(num int) Focus {
		return Focus{Kind: FocusRange, PRNumber: num}
	}

	t.Run("same number skips", func(t *testing.T) {
		calls = nil
		dropStaleCacheOnPRSwitch(pr(1), pr(1))
		if len(calls) != 0 {
			t.Fatalf("calls=%v want none", calls)
		}
	})

	t.Run("different number invalidates", func(t *testing.T) {
		calls = nil
		dropStaleCacheOnPRSwitch(pr(2), pr(3))
		if len(calls) != 1 || calls[0] != 2 {
			t.Fatalf("calls=%v", calls)
		}
	})

	t.Run("zero PR number skips", func(t *testing.T) {
		calls = nil
		dropStaleCacheOnPRSwitch(pr(0), pr(1))
		if len(calls) != 0 {
			t.Fatalf("calls=%v want none", calls)
		}
	})

	t.Run("nil hook is safe", func(t *testing.T) {
		InvalidatePRCache = nil
		dropStaleCacheOnPRSwitch(pr(1), pr(2))
	})
}
