package market

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// ---- independent rule-based generator used to cross-check the static table ----

// nthWeekday returns the n-th (1-based) given weekday of a month.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	d := time.Date(year, month, 1, 12, 0, 0, 0, time.UTC)
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, 7*(n-1))
}

// lastWeekday returns the last given weekday of a month.
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	d := time.Date(year, month+1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// westernEaster implements the anonymous Gregorian (Meeus/Jones/Butcher) algorithm.
func westernEaster(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

// observed applies the NYSE Saturday->Friday / Sunday->Monday shift.
func observed(d time.Time) time.Time {
	switch d.Weekday() {
	case time.Saturday:
		return d.AddDate(0, 0, -1)
	case time.Sunday:
		return d.AddDate(0, 0, 1)
	}
	return d
}

func isWeekday(d time.Time) bool { return d.Weekday() != time.Saturday && d.Weekday() != time.Sunday }

// ruleCalendar derives the expected calendar for a year from the NYSE rules.
// Values are "closed" or "early".
func ruleCalendar(year int) map[string]string {
	out := map[string]string{}
	key := func(d time.Time) string { return d.Format("2006-01-02") }
	utc := func(m time.Month, day int) time.Time { return time.Date(year, m, day, 12, 0, 0, 0, time.UTC) }

	// New Year's Day: Sunday -> Monday; Saturday -> NOT observed on the Friday before.
	ny := utc(time.January, 1)
	if ny.Weekday() != time.Saturday {
		out[key(observed(ny))] = "closed"
	}
	out[key(nthWeekday(year, time.January, time.Monday, 3))] = "closed"   // MLK
	out[key(nthWeekday(year, time.February, time.Monday, 3))] = "closed"  // Presidents
	out[key(westernEaster(year).AddDate(0, 0, -2))] = "closed"            // Good Friday
	out[key(lastWeekday(year, time.May, time.Monday))] = "closed"         // Memorial
	out[key(observed(utc(time.June, 19)))] = "closed"                     // Juneteenth
	out[key(observed(utc(time.July, 4)))] = "closed"                      // Independence Day
	out[key(nthWeekday(year, time.September, time.Monday, 1))] = "closed" // Labor
	tg := nthWeekday(year, time.November, time.Thursday, 4)
	out[key(tg)] = "closed" // Thanksgiving
	out[key(observed(utc(time.December, 25)))] = "closed"

	// Early closes.
	jul3, jul4 := utc(time.July, 3), utc(time.July, 4)
	if isWeekday(jul3) && isWeekday(jul4) {
		out[key(jul3)] = "early"
	}
	out[key(tg.AddDate(0, 0, 1))] = "early"
	if eve := utc(time.December, 24); isWeekday(eve) {
		if _, closed := out[key(eve)]; !closed {
			out[key(eve)] = "early"
		}
	}
	return out
}

func TestUSMarketCalendarMatchesRules(t *testing.T) {
	want := map[string]string{}
	for y := usMarketCalendarFirstYear; y <= usMarketCalendarLastYear; y++ {
		for k, v := range ruleCalendar(y) {
			want[k] = v
		}
	}
	// One-off closure that no rule produces.
	want["2025-01-09"] = "closed"

	got := map[string]string{}
	for k, v := range usMarketCalendar {
		if v.EarlyClose {
			got[k] = "early"
		} else {
			got[k] = "closed"
		}
	}
	keys := func(m map[string]string) []string {
		s := make([]string, 0, len(m))
		for k := range m {
			s = append(s, k)
		}
		sort.Strings(s)
		return s
	}
	for _, k := range keys(want) {
		if got[k] != want[k] {
			t.Errorf("%s: table=%q rules=%q", k, got[k], want[k])
		}
	}
	for _, k := range keys(got) {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: in table (%q) but not produced by rules", k, got[k])
		}
	}
}

// TestUSMarketCalendarPublishedDates pins the dates published by NYSE (checked
// against nyse.com holiday/early-close schedules) independently of the rules.
func TestUSMarketCalendarPublishedDates(t *testing.T) {
	closed := map[int][]string{
		2025: {"2025-01-01", "2025-01-09", "2025-01-20", "2025-02-17", "2025-04-18", "2025-05-26", "2025-06-19", "2025-07-04", "2025-09-01", "2025-11-27", "2025-12-25"},
		2026: {"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25"},
		2027: {"2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18", "2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24"},
	}
	early := map[int][]string{
		2025: {"2025-07-03", "2025-11-28", "2025-12-24"},
		2026: {"2026-11-27", "2026-12-24"},
		2027: {"2027-11-26"},
	}
	count := map[int]int{}
	for y, days := range closed {
		for _, d := range days {
			day, ok := usMarketCalendar[d]
			if !ok || day.EarlyClose {
				t.Errorf("%d: %s should be a full closure, got %+v ok=%v", y, d, day, ok)
			}
			count[y]++
		}
	}
	for y, days := range early {
		for _, d := range days {
			day, ok := usMarketCalendar[d]
			if !ok || !day.EarlyClose {
				t.Errorf("%d: %s should be an early close, got %+v ok=%v", y, d, day, ok)
			}
			count[y]++
		}
	}
	perYear := map[int]int{}
	for k := range usMarketCalendar {
		y := int(k[0]-'0')*1000 + int(k[1]-'0')*100 + int(k[2]-'0')*10 + int(k[3]-'0')
		perYear[y]++
	}
	for y, n := range count {
		if perYear[y] != n {
			t.Errorf("%d: table has %d entries, expected %d", y, perYear[y], n)
		}
	}
}

func TestUSMarketCalendarSanity(t *testing.T) {
	for k, day := range usMarketCalendar {
		d, err := time.ParseInLocation("2006-01-02", k, newYorkLocation())
		if err != nil {
			t.Fatalf("bad key %q: %v", k, err)
		}
		if !isWeekday(d) {
			t.Errorf("%s (%s) falls on a weekend", k, day.Name)
		}
		if day.Name == "" {
			t.Errorf("%s has no name", k)
		}
		if d.Year() < usMarketCalendarFirstYear || d.Year() > usMarketCalendarLastYear {
			t.Errorf("%s outside the declared calendar range", k)
		}
	}
	// 2028-01-01 is a Saturday: NYSE stays open on Friday 2027-12-31.
	if _, _, ok := USMarketHoliday(time.Date(2027, 12, 31, 12, 0, 0, 0, newYorkLocation())); ok {
		t.Error("2027-12-31 must not be a holiday (New Year's Day 2028 is a Saturday)")
	}
	// 2026-07-02 and 2027-07-02 are full days (no early close before an observed Jul 4).
	for _, d := range []time.Time{
		time.Date(2026, 7, 2, 12, 0, 0, 0, newYorkLocation()),
		time.Date(2027, 7, 2, 12, 0, 0, 0, newYorkLocation()),
	} {
		if _, _, ok := USMarketHoliday(d); ok {
			t.Errorf("%s must be a normal day", d.Format("2006-01-02"))
		}
	}
}

func TestUSMarketHoliday(t *testing.T) {
	ny := newYorkLocation()
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, ny) }
	tests := []struct {
		name  string
		t     time.Time
		want  string
		early bool
		ok    bool
	}{
		{"2025 new year", at(2025, 1, 1, 10, 0), "New Year's Day", false, true},
		{"2025 mourning day", at(2025, 1, 9, 10, 0), "National Day of Mourning", false, true},
		{"2025 good friday", at(2025, 4, 18, 10, 0), "Good Friday", false, true},
		{"2025 jul 3 early", at(2025, 7, 3, 10, 0), "Day before Independence Day", true, true},
		{"2025 thanksgiving", at(2025, 11, 27, 10, 0), "Thanksgiving", false, true},
		{"2025 day after", at(2025, 11, 28, 10, 0), "Day after Thanksgiving", true, true},
		{"2025 christmas eve", at(2025, 12, 24, 10, 0), "Christmas Eve", true, true},
		{"2026 jul 3 observed", at(2026, 7, 3, 10, 0), "Independence Day (observed)", false, true},
		{"2026 jul 4 saturday itself", at(2026, 7, 4, 10, 0), "", false, false},
		{"2026 thanksgiving", at(2026, 11, 26, 10, 0), "Thanksgiving", false, true},
		{"2026 christmas eve", at(2026, 12, 24, 10, 0), "Christmas Eve", true, true},
		{"2026 normal wednesday", at(2026, 1, 7, 10, 0), "", false, false},
		{"2027 juneteenth observed", at(2027, 6, 18, 10, 0), "Juneteenth (observed)", false, true},
		{"2027 jun 19 saturday itself", at(2027, 6, 19, 10, 0), "", false, false},
		{"2027 independence observed", at(2027, 7, 5, 10, 0), "Independence Day (observed)", false, true},
		{"2027 christmas observed", at(2027, 12, 24, 10, 0), "Christmas (observed)", false, true},
		{"2027 dec 25 saturday itself", at(2027, 12, 25, 10, 0), "", false, false},
		{"2027 day after thanksgiving", at(2027, 11, 26, 10, 0), "Day after Thanksgiving", true, true},
		{"2028 out of range", at(2028, 7, 4, 10, 0), "", false, false},
		{"2024 out of range", at(2024, 12, 25, 10, 0), "", false, false},
	}
	for _, tt := range tests {
		name, early, ok := USMarketHoliday(tt.t)
		if name != tt.want || early != tt.early || ok != tt.ok {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", tt.name, name, early, ok, tt.want, tt.early, tt.ok)
		}
	}
}

func TestUSMarketHolidayUsesNewYorkDate(t *testing.T) {
	ny := newYorkLocation()
	// Thanksgiving 2026 (Thu Nov 26): 20:00 ET is already Nov 27 01:00 UTC but still Thanksgiving.
	evening := time.Date(2026, 11, 26, 20, 0, 0, 0, ny).UTC()
	if evening.Day() != 27 {
		t.Fatalf("test setup: expected UTC day 27, got %v", evening)
	}
	if name, early, ok := USMarketHoliday(evening); !ok || early || name != "Thanksgiving" {
		t.Errorf("Thanksgiving 20:00 ET (as UTC): got (%q, %v, %v)", name, early, ok)
	}
	// 04:59 UTC on Nov 26 is still Wednesday 23:59 ET: not a holiday.
	if _, _, ok := USMarketHoliday(time.Date(2026, 11, 26, 4, 59, 0, 0, time.UTC)); ok {
		t.Error("Wed 23:59 ET must not be Thanksgiving")
	}
	// 05:00 UTC is Thursday 00:00 ET: holiday starts.
	if _, _, ok := USMarketHoliday(time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)); !ok {
		t.Error("Thu 00:00 ET must be Thanksgiving")
	}
}

func TestUSMarketCalendarCovers(t *testing.T) {
	ny := newYorkLocation()
	cases := []struct {
		t    time.Time
		want bool
	}{
		{time.Date(2024, 12, 31, 23, 59, 0, 0, ny), false},
		{time.Date(2025, 1, 1, 0, 0, 0, 0, ny), true},
		{time.Date(2027, 12, 31, 23, 59, 0, 0, ny), true},
		{time.Date(2028, 1, 1, 0, 0, 0, 0, ny), false},
		// 2028-01-01 03:00 UTC is still 2027-12-31 22:00 ET.
		{time.Date(2028, 1, 1, 3, 0, 0, 0, time.UTC), true},
	}
	for _, c := range cases {
		if got := USMarketCalendarCovers(c.t); got != c.want {
			t.Errorf("Covers(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// ---- MarketSession behaviour on holidays / early closes ----

func TestMarketSessionHolidays(t *testing.T) {
	ny := newYorkLocation()
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, ny) }
	tests := []struct {
		name      string
		class     string
		t         time.Time
		reg       bool
		wantNotes []string // all must appear in the note
		notNotes  []string // none may appear in the note
	}{
		// ---- 2025 ----
		{"2025 thanksgiving 10:00", "equity", at(2025, 11, 27, 10, 0), false,
			[]string{"US cash market closed (Thanksgiving)", "perp liquidity may be thin and price may gap at reopen"}, []string{"not modelled"}},
		{"2025 good friday 10:00", "equity", at(2025, 4, 18, 10, 0), false, []string{"US cash market closed (Good Friday)"}, nil},
		{"2025 jul 3 before 13:00", "equity", at(2025, 7, 3, 12, 59), true, []string{"US early close 13:00 ET", "Day before Independence Day"}, nil},
		{"2025 jul 3 at 13:00", "equity", at(2025, 7, 3, 13, 0), false, []string{"US early close 13:00 ET", "may gap at reopen"}, nil},
		{"2025 jul 4 independence", "equity", at(2025, 7, 4, 11, 0), false, []string{"US cash market closed (Independence Day)"}, nil},
		{"2025 day after thanksgiving 09:30", "equity", at(2025, 11, 28, 9, 30), true, []string{"US early close 13:00 ET"}, nil},
		{"2025 day after thanksgiving 09:29", "equity", at(2025, 11, 28, 9, 29), false, []string{"outside US regular hours", "US early close 13:00 ET"}, nil},
		{"2025 day after thanksgiving 14:00", "equity", at(2025, 11, 28, 14, 0), false, []string{"US early close 13:00 ET", "may gap at reopen"}, nil},
		{"2025 christmas eve 15:00", "equity", at(2025, 12, 24, 15, 0), false, []string{"US early close 13:00 ET", "Christmas Eve"}, nil},
		{"2025 christmas", "equity", at(2025, 12, 25, 10, 0), false, []string{"US cash market closed (Christmas)"}, nil},
		{"2025 normal tuesday 10:00", "equity", at(2025, 11, 25, 10, 0), true, []string{"US regular hours (09:30-16:00 ET)"}, []string{"not modelled", "early close", "closed ("}},
		// ---- 2026 ----
		{"2026 new year", "equity", at(2026, 1, 1, 10, 0), false, []string{"US cash market closed (New Year's Day)"}, nil},
		{"2026 mlk", "equity", at(2026, 1, 19, 10, 0), false, []string{"US cash market closed (Martin Luther King Jr. Day)"}, nil},
		{"2026 presidents", "equity", at(2026, 2, 16, 10, 0), false, []string{"US cash market closed (Presidents Day)"}, nil},
		{"2026 good friday", "equity", at(2026, 4, 3, 10, 0), false, []string{"US cash market closed (Good Friday)"}, nil},
		{"2026 memorial", "equity", at(2026, 5, 25, 10, 0), false, []string{"US cash market closed (Memorial Day)"}, nil},
		{"2026 juneteenth", "equity", at(2026, 6, 19, 10, 0), false, []string{"US cash market closed (Juneteenth)"}, nil},
		{"2026 jul 3 observed", "equity", at(2026, 7, 3, 10, 0), false, []string{"US cash market closed (Independence Day (observed))"}, nil},
		{"2026 jul 2 normal", "equity", at(2026, 7, 2, 10, 0), true, []string{"US regular hours (09:30-16:00 ET)"}, []string{"early close"}},
		{"2026 jul 2 14:00 normal still regular", "equity", at(2026, 7, 2, 14, 0), true, nil, []string{"early close"}},
		{"2026 labor", "equity", at(2026, 9, 7, 10, 0), false, []string{"US cash market closed (Labor Day)"}, nil},
		{"2026 thanksgiving 10:00", "equity", at(2026, 11, 26, 10, 0), false, []string{"US cash market closed (Thanksgiving)"}, nil},
		{"2026 thanksgiving 20:00 overnight", "equity", at(2026, 11, 26, 20, 0), false, []string{"US cash market closed (Thanksgiving)"}, nil},
		{"2026 day after 12:30", "equity", at(2026, 11, 27, 12, 30), true, []string{"US early close 13:00 ET"}, nil},
		{"2026 day after 13:00", "equity", at(2026, 11, 27, 13, 0), false, []string{"US early close 13:00 ET"}, nil},
		{"2026 christmas eve 12:59", "equity", at(2026, 12, 24, 12, 59), true, []string{"US early close 13:00 ET"}, nil},
		{"2026 christmas eve 13:01", "equity", at(2026, 12, 24, 13, 1), false, []string{"US early close 13:00 ET"}, nil},
		{"2026 christmas", "equity", at(2026, 12, 25, 10, 0), false, []string{"US cash market closed (Christmas)"}, nil},
		// ---- 2027 ----
		{"2027 new year (friday)", "equity", at(2027, 1, 1, 10, 0), false, []string{"US cash market closed (New Year's Day)"}, nil},
		{"2027 mlk", "equity", at(2027, 1, 18, 10, 0), false, []string{"US cash market closed (Martin Luther King Jr. Day)"}, nil},
		{"2027 presidents", "equity", at(2027, 2, 15, 10, 0), false, []string{"US cash market closed (Presidents Day)"}, nil},
		{"2027 good friday", "equity", at(2027, 3, 26, 10, 0), false, []string{"US cash market closed (Good Friday)"}, nil},
		{"2027 memorial", "equity", at(2027, 5, 31, 10, 0), false, []string{"US cash market closed (Memorial Day)"}, nil},
		{"2027 juneteenth observed", "equity", at(2027, 6, 18, 10, 0), false, []string{"US cash market closed (Juneteenth (observed))"}, nil},
		{"2027 jul 5 observed", "equity", at(2027, 7, 5, 10, 0), false, []string{"US cash market closed (Independence Day (observed))"}, nil},
		{"2027 jul 2 normal", "equity", at(2027, 7, 2, 14, 0), true, nil, []string{"early close"}},
		{"2027 labor", "equity", at(2027, 9, 6, 10, 0), false, []string{"US cash market closed (Labor Day)"}, nil},
		{"2027 thanksgiving", "equity", at(2027, 11, 25, 10, 0), false, []string{"US cash market closed (Thanksgiving)"}, nil},
		{"2027 day after thanksgiving 11:00", "equity", at(2027, 11, 26, 11, 0), true, []string{"US early close 13:00 ET"}, nil},
		{"2027 day after thanksgiving 13:30", "equity", at(2027, 11, 26, 13, 30), false, []string{"US early close 13:00 ET"}, nil},
		{"2027 christmas observed (friday)", "equity", at(2027, 12, 24, 10, 0), false, []string{"US cash market closed (Christmas (observed))"}, []string{"early close"}},
		{"2027 dec 31 normal", "equity", at(2027, 12, 31, 10, 0), true, []string{"US regular hours (09:30-16:00 ET)"}, []string{"not modelled"}},
		// ---- commodity / fx follow the same cash-market calendar ----
		{"2026 thanksgiving commodity", "commodity", at(2026, 11, 26, 10, 0), false, []string{"US market holiday (Thanksgiving)", "may gap at reopen"}, nil},
		{"2026 christmas fx", "fx", at(2026, 12, 25, 10, 0), false, []string{"US market holiday (Christmas)"}, nil},
		{"2026 day after fx 12:00", "fx", at(2026, 11, 27, 12, 0), true, []string{"US early close 13:00 ET"}, nil},
		{"2026 day after commodity 13:00", "commodity", at(2026, 11, 27, 13, 0), false, []string{"US early close 13:00 ET"}, nil},
		{"2026 normal commodity", "commodity", at(2026, 11, 25, 10, 0), true, []string{"open 5x24"}, []string{"holiday", "early close", "US holidays not modelled"}},
	}
	for _, tt := range tests {
		open, reg, note := MarketSession(tt.class, tt.t)
		if !open {
			t.Errorf("%s: perp must stay open on weekdays (holidays never block trading), got open=false (%s)", tt.name, note)
		}
		if reg != tt.reg {
			t.Errorf("%s: regularHours=%v, want %v (%s)", tt.name, reg, tt.reg, note)
		}
		for _, w := range tt.wantNotes {
			if !strings.Contains(note, w) {
				t.Errorf("%s: note %q missing %q", tt.name, note, w)
			}
		}
		for _, w := range tt.notNotes {
			if strings.Contains(note, w) {
				t.Errorf("%s: note %q must not contain %q", tt.name, note, w)
			}
		}
	}
}

func TestMarketSessionHolidayNeverBlocksOrAffectsCrypto(t *testing.T) {
	ny := newYorkLocation()
	for _, d := range []time.Time{
		time.Date(2026, 11, 26, 10, 0, 0, 0, ny), // Thanksgiving
		time.Date(2026, 11, 27, 14, 0, 0, 0, ny), // after the early close
		time.Date(2027, 12, 24, 10, 0, 0, 0, ny), // observed Christmas
	} {
		if open, reg, note := MarketSession("crypto", d); !open || !reg || note != "crypto: open 24/7" {
			t.Errorf("crypto %v: got (%v, %v, %q)", d, open, reg, note)
		}
		if open, reg, note := MarketSession("", d); !open || !reg || note != "crypto: open 24/7" {
			t.Errorf("empty class %v: got (%v, %v, %q)", d, open, reg, note)
		}
		for _, cls := range []string{"equity", "commodity", "fx"} {
			if open, _, note := MarketSession(cls, d); !open {
				t.Errorf("%s %v must stay open (holidays are annotation only), note=%q", cls, d, note)
			}
		}
	}
}

func TestMarketSessionOutsideCalendarRange(t *testing.T) {
	ny := newYorkLocation()
	// 2028-01-03 Monday and 2024-12-24 Tuesday: no holiday data, so treated as normal days
	// and the note must say holidays are not modelled.
	for _, d := range []time.Time{
		time.Date(2028, 1, 3, 10, 0, 0, 0, ny),
		time.Date(2024, 12, 24, 10, 0, 0, 0, ny),
		time.Date(2028, 7, 4, 10, 0, 0, 0, ny),
	} {
		for _, cls := range []string{"equity", "commodity", "fx"} {
			open, reg, note := MarketSession(cls, d)
			if !open || !reg {
				t.Errorf("%s %v: got open=%v regular=%v, want true/true (%s)", cls, d, open, reg, note)
			}
			if !strings.Contains(note, "US holidays not modelled for") || !strings.Contains(note, "2025-2027") {
				t.Errorf("%s %v: note %q should say holidays are not modelled outside 2025-2027", cls, d, note)
			}
		}
		// outside the regular session too
		if _, reg, note := MarketSession("equity", d.Add(8*time.Hour)); reg || !strings.Contains(note, "not modelled") {
			t.Errorf("equity %v+8h: reg=%v note=%q", d, reg, note)
		}
	}
}

// ---- DST boundaries: ET offset changes but 09:30/13:00/16:00 stay wall-clock ET ----

func TestMarketSessionDSTBoundaries(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }
	tests := []struct {
		name  string
		t     time.Time
		reg   bool
		notes []string
	}{
		// Spring forward 2026-03-08 (Sun): Fri Mar 6 is EST (UTC-5), Mon Mar 9 is EDT (UTC-4).
		{"fri before spring-forward 09:30 EST", utc(2026, 3, 6, 14, 30), true, nil},
		{"fri before spring-forward 09:29 EST", utc(2026, 3, 6, 14, 29), false, nil},
		{"mon after spring-forward 09:30 EDT", utc(2026, 3, 9, 13, 30), true, nil},
		{"mon after spring-forward 13:29 UTC is 09:29 EDT", utc(2026, 3, 9, 13, 29), false, nil},
		{"mon after spring-forward 15:59 EDT", utc(2026, 3, 9, 19, 59), true, nil},
		{"mon after spring-forward 16:00 EDT", utc(2026, 3, 9, 20, 0), false, nil},
		// Fall back 2026-11-01 (Sun): Fri Oct 30 is EDT, Mon Nov 2 is EST.
		{"fri before fall-back 15:59 EDT", utc(2026, 10, 30, 19, 59), true, nil},
		{"fri before fall-back 16:00 EDT", utc(2026, 10, 30, 20, 0), false, nil},
		{"mon after fall-back 09:30 EST", utc(2026, 11, 2, 14, 30), true, nil},
		{"mon after fall-back 14:29 UTC is 09:29 EST", utc(2026, 11, 2, 14, 29), false, nil},
		// 2026-11-27 early close is in EST: 13:00 ET == 18:00 UTC.
		{"day after thanksgiving 17:59 UTC = 12:59 EST", utc(2026, 11, 27, 17, 59), true, []string{"US early close 13:00 ET"}},
		{"day after thanksgiving 18:00 UTC = 13:00 EST", utc(2026, 11, 27, 18, 0), false, []string{"US early close 13:00 ET"}},
		// Good Friday 2026-04-03 and Juneteenth 2026-06-19 are in EDT: 10:00 ET == 14:00 UTC.
		{"good friday 14:00 UTC = 10:00 EDT", utc(2026, 4, 3, 14, 0), false, []string{"US cash market closed (Good Friday)"}},
		{"juneteenth 14:00 UTC = 10:00 EDT", utc(2026, 6, 19, 14, 0), false, []string{"US cash market closed (Juneteenth)"}},
		// 2025-07-03 early close in EDT: 13:00 ET == 17:00 UTC.
		{"jul 3 2025 16:59 UTC = 12:59 EDT", utc(2025, 7, 3, 16, 59), true, []string{"US early close 13:00 ET"}},
		{"jul 3 2025 17:00 UTC = 13:00 EDT", utc(2025, 7, 3, 17, 0), false, []string{"US early close 13:00 ET"}},
		// 2027 DST: spring forward Mar 14 (Good Friday Mar 26 is EDT), fall back Nov 7
		// (Thanksgiving Nov 25 and day after are EST).
		{"good friday 2027 14:00 UTC = 10:00 EDT", utc(2027, 3, 26, 14, 0), false, []string{"US cash market closed (Good Friday)"}},
		{"day after thanksgiving 2027 17:59 UTC = 12:59 EST", utc(2027, 11, 26, 17, 59), true, []string{"US early close 13:00 ET"}},
		{"day after thanksgiving 2027 18:00 UTC = 13:00 EST", utc(2027, 11, 26, 18, 0), false, []string{"US early close 13:00 ET"}},
		// 2025 DST: fall back Nov 2; Thanksgiving Nov 27 (EST), Dec 24 early close (EST).
		{"christmas eve 2025 17:59 UTC = 12:59 EST", utc(2025, 12, 24, 17, 59), true, []string{"US early close 13:00 ET"}},
		{"christmas eve 2025 18:00 UTC = 13:00 EST", utc(2025, 12, 24, 18, 0), false, []string{"US early close 13:00 ET"}},
	}
	for _, tt := range tests {
		open, reg, note := MarketSession("equity", tt.t)
		if !open {
			t.Errorf("%s: open=false (%s)", tt.name, note)
		}
		if reg != tt.reg {
			t.Errorf("%s: regularHours=%v, want %v (%s)", tt.name, reg, tt.reg, note)
		}
		for _, w := range tt.notes {
			if !strings.Contains(note, w) {
				t.Errorf("%s: note %q missing %q", tt.name, note, w)
			}
		}
	}
}
