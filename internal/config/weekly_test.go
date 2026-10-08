package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseWeeklyReset(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in      string
		ok      bool
		wd      time.Weekday
		h, m    int
		loc     *time.Location
		wantErr string
	}{
		{in: "", ok: false},
		{in: "   ", ok: false},
		{in: "fri 17:00", ok: true, wd: time.Friday, h: 17, loc: time.Local},
		{in: "Monday 09:30 America/Los_Angeles", ok: true, wd: time.Monday, h: 9, m: 30, loc: la},
		{in: "SUN 00:00 UTC", ok: true, wd: time.Sunday, loc: time.UTC},
		{in: "friday", wantErr: "want a weekday, a time"},
		{in: "someday 17:00", wantErr: "is not a weekday"},
		{in: "fri 5pm", wantErr: "HH:MM"},
		{in: "fri 25:00", wantErr: "HH:MM"},
		{in: "fri 17:00 Mars/Olympus_Mons", wantErr: "time zone"},
	} {
		r, ok, err := ParseWeeklyReset(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: err = %v, want one mentioning %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || ok != tc.ok {
			t.Errorf("%q: ok=%v err=%v, want ok=%v", tc.in, ok, err, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if r.Weekday != tc.wd || r.Hour != tc.h || r.Minute != tc.m || r.Loc.String() != tc.loc.String() {
			t.Errorf("%q parsed as %+v", tc.in, r)
		}
	}
}

func TestWeeklyResetNextIsStrictlyAfter(t *testing.T) {
	r := WeeklyReset{Weekday: time.Friday, Hour: 17, Loc: time.UTC}
	// 2026-09-29 is a Tuesday.
	for _, tc := range []struct{ now, want string }{
		{"2026-09-29T12:00:00Z", "2026-10-02T17:00:00Z"}, // later this week
		{"2026-10-02T16:59:00Z", "2026-10-02T17:00:00Z"}, // the same day, before
		{"2026-10-02T17:00:00Z", "2026-10-09T17:00:00Z"}, // exactly at it: the next one
		{"2026-10-03T08:00:00Z", "2026-10-09T17:00:00Z"}, // the day after
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := r.Next(now).UTC().Format(time.RFC3339); got != tc.want {
			t.Errorf("Next(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestAQueueWithABadWeeklyResetIsRejected(t *testing.T) {
	c := Config{Queues: []Queue{{Name: "mac-grok", AgentKind: "grok", WeeklyReset: "fri 5pm"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mac-grok") {
		t.Fatalf("Validate = %v; a weekly_reset nobody can read must be refused, naming the queue", err)
	}
	c.Queues[0].WeeklyReset = "fri 17:00"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
