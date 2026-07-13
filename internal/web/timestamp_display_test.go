package web

import (
	"testing"
	"time"
)

// Timestamps are stored UTC but displayed in the process timezone so they
// match the operator's wall clock. This test pins a non-UTC time.Local and
// asserts the render helpers convert instead of echoing UTC. It must not
// call t.Parallel(): it mutates the process-global time.Local, which the
// parallel scheduler tests read.
func TestTimestampHelpers_RenderInLocalZone(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST-3", -3*60*60)
	defer func() { time.Local = orig }()

	if got := humanISOTime("2026-07-05T19:51:03Z"); got != "2026-07-05 16:51:03" {
		t.Errorf("humanISOTime = %q, want 2026-07-05 16:51:03", got)
	}
	// 01:30Z falls on the previous day at UTC-3, so the date must roll back.
	if got := humanISODate("2026-07-05T01:30:00Z"); got != "2026-07-04" {
		t.Errorf("humanISODate = %q, want 2026-07-04", got)
	}

	ingested := time.Date(2026, 7, 5, 19, 51, 3, 0, time.UTC)
	local := templateFuncs()["localTime"].(func(time.Time) string)
	if got := local(ingested); got != "2026-07-05 16:51:03" {
		t.Errorf("localTime = %q, want 2026-07-05 16:51:03", got)
	}
	localPtr := templateFuncs()["localTimePtr"].(func(*time.Time) string)
	if got := localPtr(&ingested); got != "2026-07-05 16:51:03" {
		t.Errorf("localTimePtr = %q, want 2026-07-05 16:51:03", got)
	}
	if got := localPtr(nil); got != "" {
		t.Errorf("localTimePtr(nil) = %q, want empty", got)
	}
}
