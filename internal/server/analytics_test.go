package server

import (
	"testing"
	"time"
)

// rangeWindow maps the UI range tokens to a [from,to) window and a bucket unit.
func TestRangeWindowUnits(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 30, 0, 0, time.Local)
	cases := []struct {
		rng      string
		wantUnit bucketUnit
	}{
		{"1d", bucketHour},
		{"7d", bucketDay},
		{"15d", bucketDay},
		{"30d", bucketDay},
		{"6m", bucketWeek},
	}
	for _, c := range cases {
		from, to, unit, ok := rangeWindow(now, c.rng)
		if !ok {
			t.Fatalf("%s: rangeWindow not ok", c.rng)
		}
		if unit != c.wantUnit {
			t.Errorf("%s: unit=%s want %s", c.rng, unit, c.wantUnit)
		}
		if !to.Equal(now) {
			t.Errorf("%s: to=%v want now %v", c.rng, to, now)
		}
		if !from.Before(to) {
			t.Errorf("%s: from %v not before to %v", c.rng, from, to)
		}
	}
	if _, _, _, ok := rangeWindow(now, "bogus"); ok {
		t.Errorf("bogus range should not be ok")
	}
}

func TestUnitForSpan(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want bucketUnit
	}{
		{36 * time.Hour, bucketHour},
		{48 * time.Hour, bucketHour},
		{10 * 24 * time.Hour, bucketDay},
		{90 * 24 * time.Hour, bucketDay},
		{200 * 24 * time.Hour, bucketWeek},
	}
	for _, c := range cases {
		if got := unitForSpan(c.d); got != c.want {
			t.Errorf("unitForSpan(%v)=%s want %s", c.d, got, c.want)
		}
	}
}

// Buckets must align to calendar boundaries: hour on the hour, day at local
// midnight, week on Monday. Steps are calendar-correct (DST-safe via AddDate).
func TestBucketsForAlignment(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 30, 0, 0, time.Local)

	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)
	if g.Unit != bucketDay {
		t.Fatalf("7d unit=%s want day", g.Unit)
	}
	if g.Len() == 0 {
		t.Fatal("7d grid empty")
	}
	for i, s := range g.Starts {
		if s.Hour() != 0 || s.Minute() != 0 || s.Second() != 0 {
			t.Errorf("day start %d not midnight: %v", i, s)
		}
		if i > 0 && !g.Starts[i].Equal(g.Starts[i-1].AddDate(0, 0, 1)) {
			t.Errorf("day starts not 1 calendar day apart at %d", i)
		}
	}
	if !g.Partial {
		t.Errorf("7d: expected the last bucket to be partial (window ends at now)")
	}

	from, to, unit, _ = rangeWindow(now, "6m")
	wg := bucketsFor(from, to, unit, now)
	if wg.Unit != bucketWeek {
		t.Fatalf("6m unit=%s want week", wg.Unit)
	}
	for i, s := range wg.Starts {
		if s.Weekday() != time.Monday {
			t.Errorf("week start %d not Monday: %v (%s)", i, s, s.Weekday())
		}
		if s.Hour() != 0 {
			t.Errorf("week start %d not midnight: %v", i, s)
		}
		if i > 0 && !wg.Starts[i].Equal(wg.Starts[i-1].AddDate(0, 0, 7)) {
			t.Errorf("week starts not 7 days apart at %d", i)
		}
	}

	from, to, unit, _ = rangeWindow(now, "1d")
	hg := bucketsFor(from, to, unit, now)
	if hg.Unit != bucketHour {
		t.Fatalf("1d unit=%s want hour", hg.Unit)
	}
	for i, s := range hg.Starts {
		if s.Minute() != 0 || s.Second() != 0 {
			t.Errorf("hour start %d not on the hour: %v", i, s)
		}
		if i > 0 && !hg.Starts[i].Equal(hg.Starts[i-1].Add(time.Hour)) {
			t.Errorf("hour starts not 1h apart at %d", i)
		}
	}
}

func TestBucketGridIndexOf(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 30, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)
	n := g.Len()

	if idx := g.indexOf(g.Starts[0].Add(-time.Hour)); idx != -1 {
		t.Errorf("before-window idx=%d want -1", idx)
	}
	if idx := g.indexOf(g.Starts[0].Add(time.Hour)); idx != 0 {
		t.Errorf("first-bucket idx=%d want 0", idx)
	}
	if idx := g.indexOf(now); idx != n-1 {
		t.Errorf("now idx=%d want %d (last/partial bucket)", idx, n-1)
	}
	if idx := g.indexOf(g.Starts[n-1].AddDate(0, 0, 5)); idx != -1 {
		t.Errorf("far-future idx=%d want -1", idx)
	}
}

// delta_pct is nil (omitted) when the prior window is zero — never Inf/NaN.
func TestDeltaPct(t *testing.T) {
	if d := deltaPct(110, 100); d == nil || *d != 10 {
		t.Errorf("deltaPct(110,100)=%v want 10", d)
	}
	if d := deltaPct(80, 100); d == nil || *d != -20 {
		t.Errorf("deltaPct(80,100)=%v want -20", d)
	}
	if d := deltaPct(5, 0); d != nil {
		t.Errorf("deltaPct(5,0)=%v want nil", d)
	}
}
