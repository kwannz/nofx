package market

import (
	"fmt"
	"time"
)

// Static NYSE holiday / early-close calendar (2025-2027).
//
// Bitget TradFi perpetuals keep trading 5x24 through US market holidays, so
// the calendar never blocks trading: it only annotates the session (see
// MarketSession). Real halts are reported by the contract's SymbolStatus.
//
// Rules the table follows (NYSE Rule 7.2):
//   - A holiday that falls on Saturday is observed on the preceding Friday,
//     one that falls on Sunday on the following Monday. The exception is New
//     Year's Day on a Saturday, which is not observed on the previous Friday.
//   - Early close (13:00 ET): the day before Independence Day when both days
//     are weekdays, the day after Thanksgiving, and Christmas Eve when it is a
//     weekday and not itself the observed Christmas holiday.
//   - Good Friday is the Friday before Western Easter.
//   - 2025-01-09 was a one-off full closure (National Day of Mourning for
//     former President Carter).
//
// Years outside [usMarketCalendarFirstYear, usMarketCalendarLastYear] are not
// modelled; USMarketCalendarCovers lets callers say so.
const (
	usMarketCalendarFirstYear = 2025
	usMarketCalendarLastYear  = 2027

	// usEarlyCloseMinute is the early close time in minutes after midnight ET.
	usEarlyCloseMinute = 13 * 60
)

// usMarketDay describes one non-standard NYSE day.
type usMarketDay struct {
	Name       string
	EarlyClose bool // true: 13:00 ET early close; false: closed all day
}

func closedDay(name string) usMarketDay { return usMarketDay{Name: name} }
func halfDay(name string) usMarketDay   { return usMarketDay{Name: name, EarlyClose: true} }

// usMarketCalendar maps an ET calendar date (YYYY-MM-DD) to its special status.
var usMarketCalendar = map[string]usMarketDay{
	// 2025
	"2025-01-01": closedDay("New Year's Day"),
	"2025-01-09": closedDay("National Day of Mourning"),
	"2025-01-20": closedDay("Martin Luther King Jr. Day"),
	"2025-02-17": closedDay("Presidents Day"),
	"2025-04-18": closedDay("Good Friday"),
	"2025-05-26": closedDay("Memorial Day"),
	"2025-06-19": closedDay("Juneteenth"),
	"2025-07-03": halfDay("Day before Independence Day"),
	"2025-07-04": closedDay("Independence Day"),
	"2025-09-01": closedDay("Labor Day"),
	"2025-11-27": closedDay("Thanksgiving"),
	"2025-11-28": halfDay("Day after Thanksgiving"),
	"2025-12-24": halfDay("Christmas Eve"),
	"2025-12-25": closedDay("Christmas"),

	// 2026
	"2026-01-01": closedDay("New Year's Day"),
	"2026-01-19": closedDay("Martin Luther King Jr. Day"),
	"2026-02-16": closedDay("Presidents Day"),
	"2026-04-03": closedDay("Good Friday"),
	"2026-05-25": closedDay("Memorial Day"),
	"2026-06-19": closedDay("Juneteenth"),
	"2026-07-03": closedDay("Independence Day (observed)"), // Jul 4 is a Saturday
	"2026-09-07": closedDay("Labor Day"),
	"2026-11-26": closedDay("Thanksgiving"),
	"2026-11-27": halfDay("Day after Thanksgiving"),
	"2026-12-24": halfDay("Christmas Eve"),
	"2026-12-25": closedDay("Christmas"),

	// 2027 (2027-12-31 is NOT a holiday: New Year's Day 2028 falls on a Saturday)
	"2027-01-01": closedDay("New Year's Day"),
	"2027-01-18": closedDay("Martin Luther King Jr. Day"),
	"2027-02-15": closedDay("Presidents Day"),
	"2027-03-26": closedDay("Good Friday"),
	"2027-05-31": closedDay("Memorial Day"),
	"2027-06-18": closedDay("Juneteenth (observed)"),       // Jun 19 is a Saturday
	"2027-07-05": closedDay("Independence Day (observed)"), // Jul 4 is a Sunday
	"2027-09-06": closedDay("Labor Day"),
	"2027-11-25": closedDay("Thanksgiving"),
	"2027-11-26": halfDay("Day after Thanksgiving"),
	"2027-12-24": closedDay("Christmas (observed)"), // Dec 25 is a Saturday
}

// usMarketDateKey returns the calendar key for the ET date of t.
func usMarketDateKey(t time.Time) string {
	et := t.In(newYorkLocation())
	return fmt.Sprintf("%04d-%02d-%02d", et.Year(), int(et.Month()), et.Day())
}

// USMarketHoliday reports whether the New York calendar date of t is an NYSE
// holiday or early-close day.
//
//	name       - holiday name, e.g. "Thanksgiving" or "Day after Thanksgiving"
//	earlyClose - true when the cash market closes at 13:00 ET instead of being
//	             closed all day
//	ok         - false for a normal trading day, a weekend, or a date outside
//	             the modelled years (check USMarketCalendarCovers to tell the
//	             last two apart from "normal")
func USMarketHoliday(t time.Time) (name string, earlyClose bool, ok bool) {
	day, found := usMarketCalendar[usMarketDateKey(t)]
	if !found {
		return "", false, false
	}
	return day.Name, day.EarlyClose, true
}

// USMarketCalendarCovers reports whether the ET year of t is inside the static
// holiday table. Outside it, USMarketHoliday always returns ok=false because
// holidays are not modelled, not because the day is a normal one.
func USMarketCalendarCovers(t time.Time) bool {
	y := t.In(newYorkLocation()).Year()
	return y >= usMarketCalendarFirstYear && y <= usMarketCalendarLastYear
}
