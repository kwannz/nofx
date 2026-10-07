package market

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // embed tz database so containers without tzdata still work
)

var (
	nyLocation     *time.Location
	nyLocationOnce bool
)

// newYorkLocation returns America/New_York, falling back to a fixed UTC-5 zone.
func newYorkLocation() *time.Location {
	if !nyLocationOnce {
		loc, err := time.LoadLocation("America/New_York")
		if err != nil || loc == nil {
			loc = time.FixedZone("ET", -5*3600)
		}
		nyLocation = loc
		nyLocationOnce = true
	}
	return nyLocation
}

func init() { newYorkLocation() }

// US cash-market regular session, minutes after midnight ET.
const (
	usRegularOpenMinute  = 9*60 + 30
	usRegularCloseMinute = 16 * 60
)

// MarketSession reports the trading session state of an asset class at time t.
//
//	open         - whether Bitget's perpetual for this asset class is tradable
//	regularHours - whether the underlying US cash market is in its regular session
//	               (Mon-Fri 09:30-16:00 ET, never on an NYSE holiday, until 13:00 ET
//	               on an NYSE early-close day); only meaningful for equities
//	note         - short human-readable description (also usable in prompts)
//
// Bitget TradFi perps trade 5x24: Monday 00:00 ET to Saturday 00:00 ET.
// NYSE holidays and early closes (2025-2027, see USMarketHoliday) do NOT close
// the perp: open stays true and only regularHours/note reflect the closed or
// shortened cash session. Real halts are handled by the contract SymbolStatus.
// Outside the modelled years the note says holidays are not modelled.
// Crypto is always open.
func MarketSession(assetClass string, t time.Time) (open bool, regularHours bool, note string) {
	switch strings.ToLower(assetClass) {
	case "", "crypto":
		return true, true, "crypto: open 24/7"
	}
	et := t.In(newYorkLocation())
	wd := et.Weekday()
	isOpen := wd >= time.Monday && wd <= time.Friday // Mon 00:00 -> Sat 00:00
	if !isOpen {
		return false, false, fmt.Sprintf("closed for the weekend (Sat 00:00 - Mon 00:00 ET); now %s ET. Opening trades is not allowed", et.Format("Mon 15:04"))
	}
	minutes := et.Hour()*60 + et.Minute()

	holiday, early, special := USMarketHoliday(et)
	closeMinute := usRegularCloseMinute
	switch {
	case special && early:
		closeMinute = usEarlyCloseMinute
	case special:
		closeMinute = 0 // cash market closed all day
	}
	regular := minutes >= usRegularOpenMinute && minutes < closeMinute

	suffix := ""
	if !USMarketCalendarCovers(et) {
		suffix = fmt.Sprintf("; US holidays not modelled for %d (calendar covers %d-%d)",
			et.Year(), usMarketCalendarFirstYear, usMarketCalendarLastYear)
	}
	const thinLiquidity = "perp liquidity may be thin and price may gap at reopen"
	earlyNote := fmt.Sprintf("US early close 13:00 ET (%s)", holiday)

	switch strings.ToLower(assetClass) {
	case "equity":
		switch {
		case special && !early:
			return true, false, fmt.Sprintf("open 5x24, US cash market closed (%s); %s", holiday, thinLiquidity)
		case special && regular:
			return true, true, fmt.Sprintf("open, US regular hours (09:30-13:00 ET); %s", earlyNote)
		case special && minutes >= usEarlyCloseMinute:
			return true, false, fmt.Sprintf("open, US cash market closed after %s; %s", earlyNote, thinLiquidity)
		case special:
			return true, false, fmt.Sprintf("open, outside US regular hours (extended/overnight session, thinner liquidity and wider spreads); %s", earlyNote)
		case regular:
			return true, true, "open, US regular hours (09:30-16:00 ET)" + suffix
		}
		return true, false, "open, outside US regular hours (extended/overnight session, thinner liquidity and wider spreads)" + suffix
	default: // commodity, fx
		switch {
		case special && !early:
			return true, false, fmt.Sprintf("open 5x24, US market holiday (%s): US cash market closed; %s", holiday, thinLiquidity)
		case special:
			return true, regular, fmt.Sprintf("open 5x24 (Mon 00:00 - Sat 00:00 ET); %s", earlyNote)
		}
		return true, regular, "open 5x24 (Mon 00:00 - Sat 00:00 ET); maintenance not modelled" + suffix
	}
}
