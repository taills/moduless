package manifest

import (
	"testing"
	"time"
)

func TestParseScheduleRejectsBadExpressions(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",     // four fields
		"* * * * * *", // six, the seconds-first variant
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day of month starts at 1
		"* * * 13 *",  // month out of range
		"* * * * 7",   // day of week is 0-6
		"*/0 * * * *", // zero step
		"5-1 * * * *", // backwards range
		"MON * * * *", // names are not supported
		"* * * * JAN", //
		"abc * * * *", //
	} {
		t.Run(expr, func(t *testing.T) {
			if _, err := ParseSchedule(expr); err == nil {
				t.Errorf("ParseSchedule(%q) accepted an invalid expression", expr)
			}
		})
	}
}

func TestScheduleMatches(t *testing.T) {
	// A Wednesday.
	base := time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC)
	if base.Weekday() != time.Wednesday {
		t.Fatalf("the fixture date is a %v, not the Wednesday this test assumes", base.Weekday())
	}

	tests := []struct {
		expr string
		at   time.Time
		want bool
	}{
		{"* * * * *", base, true},
		{"17 3 * * *", base, true},
		{"17 3 * * *", base.Add(time.Minute), false},
		{"17 3 * * *", base.Add(time.Hour), false},
		{"*/5 * * * *", time.Date(2026, 8, 12, 0, 15, 0, 0, time.UTC), true},
		{"*/5 * * * *", time.Date(2026, 8, 12, 0, 16, 0, 0, time.UTC), false},
		{"0,30 * * * *", time.Date(2026, 8, 12, 0, 30, 0, 0, time.UTC), true},
		{"0,30 * * * *", time.Date(2026, 8, 12, 0, 31, 0, 0, time.UTC), false},
		{"17 1-5 * * *", base, true},
		{"17 4-5 * * *", base, false},
		{"17 3 12 * *", base, true},
		{"17 3 13 * *", base, false},
		{"17 3 * 8 *", base, true},
		{"17 3 * 9 *", base, false},
		{"17 3 * * 3", base, true}, // Wednesday
		{"17 3 * * 4", base, false},

		// Cron's day-of-month / day-of-week rule: when both are restricted,
		// either matching fires the job. The 12th is a Wednesday here, so try a
		// date that satisfies only one of the two.
		{"17 3 1 * 3", base, true},  // not the 1st, but is a Wednesday
		{"17 3 12 * 0", base, true}, // is the 12th, but not a Sunday
		{"17 3 1 * 0", base, false}, // neither
	}

	for _, tc := range tests {
		t.Run(tc.expr+"@"+tc.at.Format("2006-01-02T15:04"), func(t *testing.T) {
			s, err := ParseSchedule(tc.expr)
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tc.expr, err)
			}
			if got := s.Matches(tc.at); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// A schedule must fire once per matching minute, not once per check. The
// scheduler ticks more often than a minute, so anything time-dependent has to
// be stable across repeated calls within the same minute.
func TestScheduleIsStableWithinAMinute(t *testing.T) {
	s, err := ParseSchedule("17 3 * * *")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}

	at := time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC)
	for sec := range 60 {
		if !s.Matches(at.Add(time.Duration(sec) * time.Second)) {
			t.Fatalf("Matches went false %d seconds into the matching minute", sec)
		}
	}
	if s.Matches(at.Add(time.Minute)) {
		t.Error("Matches is still true in the following minute")
	}
}
