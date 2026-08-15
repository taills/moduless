package manifest

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression: minute, hour, day of month,
// month, day of week.
//
// It answers one question — does this expression fire at this minute — rather
// than computing the next occurrence. The scheduler ticks once a minute and
// asks, which needs no state, survives Core restarting mid-minute, and cannot
// drift. Computing next-fire times would be more precise than cron itself is:
// the expression has no finer resolution than a minute to begin with.
type Schedule struct {
	minute uint64 // 0-59
	hour   uint32 // 0-23
	dom    uint32 // 1-31
	month  uint16 // 1-12
	dow    uint8  // 0-6, Sunday = 0

	// domRestricted and dowRestricted record whether each field was something
	// other than "*". Cron's oddest rule is that when both are restricted a
	// match on either is enough, rather than both — "0 0 1 * 1" means the
	// first of the month AND every Monday, not Mondays that fall on the first.
	domRestricted bool
	dowRestricted bool
}

// ParseSchedule reads a standard five-field cron expression.
//
// Supported per field: "*", a number, a range "a-b", a step "*/n" or "a-b/n",
// and comma-separated lists of those. Names for months and weekdays are not
// supported; a manifest is machine-written often enough that accepting "JAN"
// buys little and quietly accepting a typo as a name would cost more.
func ParseSchedule(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d",
			expr, len(fields))
	}

	minute, _, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	hour, _, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	dom, domRestricted, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron %q: day of month: %w", expr, err)
	}
	month, _, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	dow, dowRestricted, err := parseField(fields[4], 0, 6)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron %q: day of week: %w", expr, err)
	}

	return Schedule{
		minute:        minute,
		hour:          uint32(hour),
		dom:           uint32(dom),
		month:         uint16(month),
		dow:           uint8(dow),
		domRestricted: domRestricted,
		dowRestricted: dowRestricted,
	}, nil
}

// Matches reports whether the schedule fires during t's minute.
func (s Schedule) Matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(t.Month())) == 0 {
		return false
	}

	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0

	// Both restricted: either one matching is enough. This is cron's actual
	// behaviour and it surprises people, so it is spelled out rather than
	// falling out of the code.
	if s.domRestricted && s.dowRestricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// parseField turns one cron field into a bitmask, and reports whether it was
// restricted (anything other than "*").
func parseField(field string, min, max int) (mask uint64, restricted bool, err error) {
	if field == "*" {
		return fullMask(min, max), false, nil
	}

	for part := range strings.SplitSeq(field, ",") {
		m, err := parsePart(part, min, max)
		if err != nil {
			return 0, false, err
		}
		mask |= m
	}
	return mask, true, nil
}

func parsePart(part string, min, max int) (uint64, error) {
	step := 1
	if base, stepStr, hasStep := strings.Cut(part, "/"); hasStep {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad step %q", stepStr)
		}
		step = n
		part = base
	}

	lo, hi := min, max
	switch {
	case part == "*":
		// keep the full range
	case strings.Contains(part, "-"):
		loStr, hiStr, _ := strings.Cut(part, "-")
		var err error
		if lo, err = strconv.Atoi(loStr); err != nil {
			return 0, fmt.Errorf("bad range start %q", loStr)
		}
		if hi, err = strconv.Atoi(hiStr); err != nil {
			return 0, fmt.Errorf("bad range end %q", hiStr)
		}
		if lo > hi {
			return 0, fmt.Errorf("range %d-%d is backwards", lo, hi)
		}
	default:
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("bad value %q", part)
		}
		lo, hi = n, n
	}

	if lo < min || hi > max {
		return 0, fmt.Errorf("value out of range %d-%d", min, max)
	}

	var mask uint64
	for v := lo; v <= hi; v += step {
		mask |= 1 << uint(v)
	}
	return mask, nil
}

func fullMask(min, max int) uint64 {
	var mask uint64
	for v := min; v <= max; v++ {
		mask |= 1 << uint(v)
	}
	return mask
}
