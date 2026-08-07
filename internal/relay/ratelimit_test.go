package relay

import (
	"testing"
	"time"
)

func TestFixedWindowLimiter(t *testing.T) {
	now := time.Unix(100, 0)
	l := newLimiter(time.Minute, 2, func() time.Time { return now })
	if !l.allow("client") || !l.allow("client") || l.allow("client") {
		t.Fatal("limit was not enforced")
	}
	if !l.allow("other") {
		t.Fatal("keys should have independent limits")
	}
	now = now.Add(time.Minute)
	if !l.allow("client") {
		t.Fatal("window did not reset")
	}
}

func TestFixedWindowLimiterEvictsStaleKeys(t *testing.T) {
	now := time.Unix(100, 0)
	l := newLimiter(time.Minute, 1, func() time.Time { return now })
	for _, key := range []string{"old-a", "old-b", "current"} {
		if !l.allow(key) {
			t.Fatalf("first request for %q was rejected", key)
		}
	}
	now = now.Add(time.Minute)
	if !l.allow("current") {
		t.Fatal("new window was rejected")
	}
	if len(l.keys) != 1 {
		t.Fatalf("stale keys retained: %#v", l.keys)
	}
}

func TestDisabledLimiterAllowsRequests(t *testing.T) {
	l := newLimiter(time.Minute, 0, time.Now)
	for range 100 {
		if !l.allow("client") {
			t.Fatal("disabled limiter rejected a request")
		}
	}
}
