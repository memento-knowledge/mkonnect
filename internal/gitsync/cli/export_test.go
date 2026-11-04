package cli

import "time"

// SetNowForTest replaces the clock relative timestamps are measured against and returns a function
// that restores it.
func SetNowForTest(t time.Time) (restore func()) {
	prev := now
	now = func() time.Time { return t }
	return func() { now = prev }
}
