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

// MarketSession reports the trading session state of an asset class at time t.
//
//	open         - whether Bitget's perpetual for this asset class is tradable
//	regularHours - whether the underlying US cash market is in its regular session
//	               (Mon-Fri 09:30-16:00 ET); only meaningful for equities
//	note         - short human-readable description (also usable in prompts)
//
// Bitget TradFi perps trade 5x24: Monday 00:00 ET to Saturday 00:00 ET.
// US market holidays are NOT modelled (mentioned in the note).
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
	regular := minutes >= 9*60+30 && minutes < 16*60
	switch strings.ToLower(assetClass) {
	case "equity":
		if regular {
			return true, true, "open, US regular hours (09:30-16:00 ET); US holidays not modelled"
		}
		return true, false, "open, outside US regular hours (extended/overnight session, thinner liquidity and wider spreads); US holidays not modelled"
	default: // commodity, fx
		return true, regular, "open 5x24 (Mon 00:00 - Sat 00:00 ET); holidays/maintenance not modelled"
	}
}
