package artifacts

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression: minute hour day-of-month month
// day-of-week. Fields accept "*", numbers, ranges "a-b", lists "a,b" and
// steps "*/n" or "a-b/n". Day of week is 0-7 (0 and 7 are Sunday). As in
// standard cron, when both day fields are restricted a time matches if
// either matches.
type Cron struct {
	min, hour, dom, month, dow uint64
	domStar, dowStar           bool
	loc                        *time.Location
}

var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// ParseCron parses expr evaluated in the given IANA timezone ("" = UTC).
func ParseCron(expr, tz string) (*Cron, error) {
	loc := time.UTC
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("timezone %q: %v", tz, err)
		}
		loc = l
	}
	e := strings.TrimSpace(expr)
	if m, ok := cronMacros[e]; ok {
		e = m
	}
	f := strings.Fields(e)
	if len(f) != 5 {
		return nil, fmt.Errorf("schedule %q: want 5 fields (minute hour day month weekday) or a macro like @daily", expr)
	}
	c := &Cron{loc: loc}
	var err error
	if c.min, err = cronField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("schedule %q: minute: %v", expr, err)
	}
	if c.hour, err = cronField(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("schedule %q: hour: %v", expr, err)
	}
	if c.dom, err = cronField(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("schedule %q: day of month: %v", expr, err)
	}
	if c.month, err = cronField(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("schedule %q: month: %v", expr, err)
	}
	if c.dow, err = cronField(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("schedule %q: day of week: %v", expr, err)
	}
	if c.dow&(1<<7) != 0 {
		c.dow |= 1
	}
	c.domStar = f[2] == "*" || strings.HasPrefix(f[2], "*/")
	c.dowStar = f[4] == "*" || strings.HasPrefix(f[4], "*/")
	return c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			step, part = n, part[:i]
		}
		a, b := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			x, y, _ := strings.Cut(part, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("bad range %q", part)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, fmt.Errorf("bad value %q", part)
			}
			a, b = n, n
			if step != 1 {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	d := c.dom&(1<<uint(t.Day())) != 0
	w := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return w
	case c.dowStar:
		return d
	default:
		return d || w
	}
}

// Next returns the first matching time strictly after t, or the zero time
// if none exists within five years (e.g. "0 0 30 2 *").
func (c *Cron) Next(t time.Time) time.Time {
	t = t.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, c.loc)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, c.loc)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, c.loc)
			continue
		}
		if c.min&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// MinGap estimates the shortest interval between consecutive runs by
// looking at the next n fire times after from.
func (c *Cron) MinGap(from time.Time, n int) time.Duration {
	var gap time.Duration
	prev := c.Next(from)
	for i := 0; i < n && !prev.IsZero(); i++ {
		next := c.Next(prev)
		if next.IsZero() {
			break
		}
		if d := next.Sub(prev); gap == 0 || d < gap {
			gap = d
		}
		prev = next
	}
	return gap
}
