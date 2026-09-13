package ws

import "time"

// SetHeartbeatIntervalForTest overrides the heartbeat interval for external tests
// (package ws_test) that need to observe a heartbeat without waiting the real 30s. Call
// the returned restore func (e.g. via defer) to put the original interval back.
func SetHeartbeatIntervalForTest(d time.Duration) (restore func()) {
	old := heartbeatInterval
	heartbeatInterval = d
	return func() { heartbeatInterval = old }
}
