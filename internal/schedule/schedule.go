// Package schedule turns a human schedule phrase ("every 6 hours",
// "Wednesday at 1pm", "weekly") OR a raw cron expression into a single
// canonical spec string, and computes the next fire time from it.
//
// The canonical form is whatever github.com/robfig/cron/v3 ParseStandard
// accepts: standard 5-field cron (minute hour dom month dow) plus the
// descriptors @hourly / @daily / @weekly and @every <duration>. English
// phrases are normalized to one of those; anything that isn't recognized
// English is tried as a raw cron expression so power users can still pass
// "0 13 * * 1-5" directly.
//
// All next-fire math runs in the machine's local timezone — flow is a
// single-host personal tool, and robfig's Next(t) honors the location of
// the time it's handed.
package schedule

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"

	"time"
)

// Kind classifies how a spec was produced, for display/grouping only.
const (
	KindPreset  = "preset"  // @hourly / @daily / @weekly
	KindEvery   = "every"   // @every Nh / @every Nm
	KindDaytime = "daytime" // day-and-time, e.g. "Wednesday at 1pm"
	KindCron    = "cron"    // raw cron passthrough
)

// Spec is a normalized, storable schedule.
type Spec struct {
	Input string // the operator's original phrase, verbatim, for display/edit
	Cron  string // canonical cron / descriptor robfig/cron parses
	Kind  string // one of the Kind* constants
}

// Parse normalizes an English phrase or raw cron expression into a Spec.
// Returns an error the caller can surface verbatim when the input is neither
// a recognized phrase nor a valid cron expression.
func Parse(input string) (Spec, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Spec{}, fmt.Errorf("schedule is empty")
	}

	// 1. Recognized English → canonical cron.
	if c, kind, ok := normalizeEnglish(strings.ToLower(raw)); ok {
		if _, err := cron.ParseStandard(c); err != nil {
			// A normalizer bug, not user error — surface loudly.
			return Spec{}, fmt.Errorf("internal: normalized %q to invalid spec %q: %w", raw, c, err)
		}
		return Spec{Input: raw, Cron: c, Kind: kind}, nil
	}

	// 2. Raw cron / descriptor passthrough.
	if _, err := cron.ParseStandard(raw); err == nil {
		return Spec{Input: raw, Cron: raw, Kind: KindCron}, nil
	}

	return Spec{}, fmt.Errorf(
		"could not understand schedule %q (try \"every hour\", \"every 6 hours\", \"weekly\", \"Wednesday at 1pm\", or a cron expression like \"0 13 * * 3\")",
		raw,
	)
}

// Validate reports whether a stored canonical spec still parses.
func Validate(canonicalCron string) error {
	_, err := cron.ParseStandard(canonicalCron)
	return err
}

// Next returns the next fire time strictly after `after`, computed in
// after's timezone (pass a local time for local-clock schedules).
func Next(canonicalCron string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(canonicalCron)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse schedule %q: %w", canonicalCron, err)
	}
	return sched.Next(after), nil
}

// Describe returns a human label for a spec, preferring the operator's own
// phrasing and falling back to the canonical cron when none was recorded
// (e.g. a schedule set directly from raw cron).
func Describe(s Spec) string {
	if strings.TrimSpace(s.Input) != "" {
		return s.Input
	}
	return s.Cron
}

// ---- English normalization ----

var weekdays = map[string]int{
	"sunday": 0, "sun": 0,
	"monday": 1, "mon": 1,
	"tuesday": 2, "tue": 2, "tues": 2,
	"wednesday": 3, "wed": 3,
	"thursday": 4, "thu": 4, "thur": 4, "thurs": 4,
	"friday": 5, "fri": 5,
	"saturday": 6, "sat": 6,
}

var months = map[string]int{
	"january": 1, "jan": 1,
	"february": 2, "feb": 2,
	"march": 3, "mar": 3,
	"april": 4, "apr": 4,
	"may":  5,
	"june": 6, "jun": 6,
	"july": 7, "jul": 7,
	"august": 8, "aug": 8,
	"september": 9, "sep": 9, "sept": 9,
	"october": 10, "oct": 10,
	"november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

var (
	everyNRe  = regexp.MustCompile(`^every\s+(\d+)\s+([a-z]+)$`)
	dailyAtRe = regexp.MustCompile(`^(?:every day|daily|each day) at (.+)$`)
	// dayListAtRe captures the day portion (one or more weekdays, in any
	// separator style) and the time portion of a "<days> at <time>" phrase.
	// The day portion is validated by parseWeekdayList, not by this regex.
	dayListAtRe = regexp.MustCompile(`^(?:every |on )?(.+?) at (.+)$`)
	// weekdaySplitRe matches the separators between days in a list:
	// commas, ampersands, plus signs, and the word "and".
	weekdaySplitRe = regexp.MustCompile(`[,&+]|\band\b`)
	// weekdayRangeRe captures the two endpoints of a weekday range, e.g.
	// "mon-fri", "monday to friday", "tuesday through thursday".
	weekdayRangeRe = regexp.MustCompile(`^([a-z]+)\s*(?:-|to|through|thru|until|–|—)\s*([a-z]+)$`)
	// ordinalRe matches a day-of-month number with an optional ordinal
	// suffix: "1", "1st", "22nd", "15th".
	ordinalRe = regexp.MustCompile(`^(\d{1,2})(?:st|nd|rd|th)?$`)
	clockRe   = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)
	hourUnits = map[string]bool{"hours": true, "hour": true, "hrs": true, "hr": true, "h": true}
	minUnits  = map[string]bool{"minutes": true, "minute": true, "mins": true, "min": true, "m": true}
)

// normalizeEnglish maps a lowercased phrase to a canonical cron string.
// Returns ok=false when the phrase is not recognized English.
func normalizeEnglish(s string) (canonical, kind string, ok bool) {
	s = strings.TrimSuffix(strings.Join(strings.Fields(s), " "), ".")

	switch s {
	case "every hour", "hourly", "once an hour", "every 1 hour":
		return "@hourly", KindPreset, true
	case "every minute", "every 1 minute":
		return "@every 1m", KindEvery, true
	case "every day", "daily", "once a day", "every 1 day":
		return "@daily", KindPreset, true
	case "every week", "weekly", "weekly once", "once a week", "once weekly", "every 1 week":
		return "@weekly", KindPreset, true
	case "every month", "monthly", "monthly once", "once a month", "once monthly", "every 1 month":
		return "@monthly", KindPreset, true
	case "every year", "yearly", "annually", "once a year", "every 1 year":
		return "@yearly", KindPreset, true
	}

	if m := everyNRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			switch {
			case hourUnits[m[2]]:
				return fmt.Sprintf("@every %dh", n), KindEvery, true
			case minUnits[m[2]]:
				return fmt.Sprintf("@every %dm", n), KindEvery, true
			}
		}
		return "", "", false
	}

	// "every day at 9am" / "daily at 09:30"
	if m := dailyAtRe.FindStringSubmatch(s); m != nil {
		if h, min, ok := parseClock(m[1]); ok {
			return fmt.Sprintf("%d %d * * *", min, h), KindDaytime, true
		}
		return "", "", false
	}

	// "[every|on] <day-spec> at <time>" — the day-spec is a weekday, a
	// weekday list/range, "weekdays"/"weekends", a day-of-month list, or a
	// month+day. parseCalendarSpec resolves it to cron's dom/month/dow fields.
	if m := dayListAtRe.FindStringSubmatch(s); m != nil {
		if dom, month, dow, ok := parseCalendarSpec(m[1]); ok {
			if h, min, ok := parseClock(m[2]); ok {
				return fmt.Sprintf("%d %d %s %s %s", min, h, dom, month, dow), KindDaytime, true
			}
		}
		return "", "", false
	}

	return "", "", false
}

// parseCalendarSpec interprets the day portion of a "<day> at <time>" phrase
// and returns cron's day-of-month, month, and day-of-week fields (each
// defaulting to "*"). ok=false when the phrase is not a recognized calendar
// spec, so the caller falls through to other interpretations / raw cron.
func parseCalendarSpec(s string) (dom, month, dow string, ok bool) {
	s = strings.TrimSpace(s)

	switch s {
	case "weekday", "weekdays", "every weekday":
		return "*", "*", "1-5", true
	case "weekend", "weekends", "every weekend":
		return "*", "*", "0,6", true
	}

	// Weekday range: "monday to friday", "mon-fri".
	if d, ok := parseWeekdayRange(s); ok {
		return "*", "*", d, true
	}
	// Weekday list: "monday, wednesday and friday".
	if dows, ok := parseWeekdayList(s); ok {
		return "*", "*", joinInts(dows), true
	}
	// Month + day-of-month: "january 1", "1st of january".
	if d, mo, ok := parseMonthDay(s); ok {
		return d, mo, "*", true
	}
	// Day-of-month list: "the 1st", "the 1st and 15th", "15th of every month".
	if d, ok := parseDayOfMonthList(s); ok {
		return d, "*", "*", true
	}
	return "*", "*", "*", false
}

// lookupWeekday resolves a single weekday token (tolerating a plural "s",
// e.g. "mondays") to its cron day-of-week number.
func lookupWeekday(tok string) (int, bool) {
	if d, ok := weekdays[tok]; ok {
		return d, true
	}
	d, ok := weekdays[strings.TrimSuffix(tok, "s")]
	return d, ok
}

// parseWeekdayList turns a weekday phrase ("monday, wednesday and friday",
// "mon, wed, fri", "saturday") into a sorted, de-duplicated list of cron
// day-of-week numbers (Sunday=0 .. Saturday=6). ok=false if the phrase
// contains any token that is not a recognized weekday — callers treat that
// as "not a weekday list" and fall through to other interpretations.
func parseWeekdayList(s string) (dows []int, ok bool) {
	fields := strings.Fields(weekdaySplitRe.ReplaceAllString(s, " "))
	seen := make(map[int]bool, len(fields))
	for _, f := range fields {
		dow, found := lookupWeekday(f)
		if !found {
			return nil, false
		}
		if !seen[dow] {
			seen[dow] = true
			dows = append(dows, dow)
		}
	}
	if len(dows) == 0 {
		return nil, false
	}
	sort.Ints(dows)
	return dows, true
}

// parseWeekdayRange turns "monday to friday" / "mon-fri" into a cron
// day-of-week field. A forward range (mon→fri) renders as "1-5"; a
// wrap-around range (fri→mon) expands to an explicit sorted list.
func parseWeekdayRange(s string) (string, bool) {
	m := weekdayRangeRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	a, okA := lookupWeekday(m[1])
	b, okB := lookupWeekday(m[2])
	if !okA || !okB {
		return "", false
	}
	if a == b {
		return strconv.Itoa(a), true
	}
	if a < b {
		return fmt.Sprintf("%d-%d", a, b), true
	}
	// Wrap-around (e.g. fri→mon): walk forward mod 7 into an explicit list.
	seen := map[int]bool{}
	var days []int
	for d := a; ; d = (d + 1) % 7 {
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
		if d == b {
			break
		}
	}
	sort.Ints(days)
	return joinInts(days), true
}

// parseDayOfMonthList turns a day-of-month phrase ("the 1st", "1st and 15th",
// "15th of every month") into a sorted, de-duplicated cron day-of-month
// field. ok=false if any token is not a valid 1–31 day.
func parseDayOfMonthList(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "the ")
	for _, suf := range []string{" of every month", " of each month", " of the month"} {
		s = strings.TrimSuffix(s, suf)
	}
	s = strings.TrimPrefix(s, "days ")
	s = strings.TrimPrefix(s, "day ")
	fields := strings.Fields(weekdaySplitRe.ReplaceAllString(s, " "))
	seen := make(map[int]bool, len(fields))
	var doms []int
	for _, f := range fields {
		if f == "the" {
			continue
		}
		n, ok := parseDayNum(f)
		if !ok {
			return "", false
		}
		if !seen[n] {
			seen[n] = true
			doms = append(doms, n)
		}
	}
	if len(doms) == 0 {
		return "", false
	}
	sort.Ints(doms)
	return joinInts(doms), true
}

// parseMonthDay turns "<month> <day>" ("january 1", "jan 1st") or
// "<day> of <month>" ("1st of january") into cron day-of-month and month
// fields. ok=false if the phrase is not that shape.
func parseMonthDay(s string) (dom, month string, ok bool) {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(s), "the "))
	switch {
	case len(fields) == 2: // "<month> <day>"
		if mo, okM := months[fields[0]]; okM {
			if d, okD := parseDayNum(fields[1]); okD {
				return strconv.Itoa(d), strconv.Itoa(mo), true
			}
		}
	case len(fields) == 3 && fields[1] == "of": // "<day> of <month>"
		if d, okD := parseDayNum(fields[0]); okD {
			if mo, okM := months[fields[2]]; okM {
				return strconv.Itoa(d), strconv.Itoa(mo), true
			}
		}
	}
	return "", "", false
}

// parseDayNum parses a 1–31 day-of-month token with an optional ordinal
// suffix ("1", "1st", "22nd", "31st").
func parseDayNum(tok string) (int, bool) {
	m := ordinalRe.FindStringSubmatch(tok)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > 31 {
		return 0, false
	}
	return n, true
}

// joinInts renders a cron day-of-week list field, e.g. []int{1,3,5} -> "1,3,5".
func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// parseClock parses a clock phrase ("1pm", "1:30pm", "13:00", "9am", "noon",
// "midnight") into 24-hour (hour, minute). ok=false on anything unparseable.
func parseClock(s string) (hour, min int, ok bool) {
	s = strings.TrimSpace(s)
	switch s {
	case "noon":
		return 12, 0, true
	case "midnight":
		return 0, 0, true
	}
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(m[1])
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	switch m[3] {
	case "am":
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		if h != 12 {
			h += 12
		}
	default: // 24-hour
		if h > 23 {
			return 0, 0, false
		}
	}
	if min > 59 {
		return 0, 0, false
	}
	return h, min, true
}
