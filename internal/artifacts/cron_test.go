package artifacts

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	base := time.Date(2026, 10, 9, 14, 32, 10, 0, time.UTC) // a Friday
	cases := []struct {
		expr, tz string
		want     string
	}{
		{"* * * * *", "", "2026-10-09T14:33:00Z"},
		{"*/15 * * * *", "", "2026-10-09T14:45:00Z"},
		{"0 6 * * *", "", "2026-10-10T06:00:00Z"},
		{"@hourly", "", "2026-10-09T15:00:00Z"},
		{"@daily", "", "2026-10-10T00:00:00Z"},
		{"30 9 * * 1-5", "", "2026-10-12T09:30:00Z"},          // next weekday after Friday 14:32 is Monday
		{"0 0 1 * *", "", "2026-11-01T00:00:00Z"},             // monthly
		{"0 0 * * 7", "", "2026-10-11T00:00:00Z"},             // 7 = Sunday
		{"0 12 16 * 2", "", "2026-10-13T12:00:00Z"},           // both day fields restricted: either matches (Tuesday the 13th)
		{"0 9 * * *", "Asia/Kolkata", "2026-10-10T03:30:00Z"}, // 09:00 IST
		{"5,10 14-15 * * *", "", "2026-10-09T15:05:00Z"},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr, c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got := cr.Next(base).UTC().Format(time.RFC3339)
		if got != c.want {
			t.Errorf("%s (%s): next = %s, want %s", c.expr, c.tz, got, c.want)
		}
	}
}

func TestCronInvalid(t *testing.T) {
	for _, e := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := ParseCron(e, ""); err == nil {
			t.Errorf("%q: expected error", e)
		}
	}
	if _, err := ParseCron("* * * * *", "Mars/Olympus"); err == nil {
		t.Error("bad timezone accepted")
	}
	c, _ := ParseCron("0 0 30 2 *", "")
	if !c.Next(time.Now()).IsZero() {
		t.Error("Feb 30 should never fire")
	}
}

func TestCronMinGap(t *testing.T) {
	c, _ := ParseCron("*/5 * * * *", "")
	if g := c.MinGap(time.Now(), 20); g != 5*time.Minute {
		t.Fatalf("gap %s", g)
	}
}
