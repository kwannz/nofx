package market

import (
	"testing"
	"time"
)

func TestMarketSession(t *testing.T) {
	ny := newYorkLocation()
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, ny) }
	// 2026-01-05 is a Monday, 01-03 Saturday, 01-04 Sunday, 01-07 Wednesday (all EST).
	tests := []struct {
		name        string
		class       string
		t           time.Time
		open, reg   bool
	}{
		{"saturday noon equity", "equity", at(2026, 1, 3, 12, 0), false, false},
		{"monday 10:00 equity", "equity", at(2026, 1, 5, 10, 0), true, true},
		{"sunday 20:00 equity", "equity", at(2026, 1, 4, 20, 0), false, false},
		{"wednesday 20:00 equity", "equity", at(2026, 1, 7, 20, 0), true, false},
		{"monday 00:00 equity opens", "equity", at(2026, 1, 5, 0, 0), true, false},
		{"saturday 00:00 equity closed", "equity", at(2026, 1, 3, 0, 0), false, false},
		{"friday 23:59 equity open", "equity", at(2026, 1, 9, 23, 59), true, false},
		{"09:29 not regular", "equity", at(2026, 1, 7, 9, 29), true, false},
		{"09:30 regular", "equity", at(2026, 1, 7, 9, 30), true, true},
		{"16:00 not regular", "equity", at(2026, 1, 7, 16, 0), true, false},
		{"saturday commodity closed", "commodity", at(2026, 1, 3, 12, 0), false, false},
		{"wednesday night commodity open", "commodity", at(2026, 1, 7, 22, 0), true, false},
		{"monday fx open", "fx", at(2026, 1, 5, 10, 0), true, true},
		{"saturday crypto open", "crypto", at(2026, 1, 3, 12, 0), true, true},
		{"empty class is crypto", "", at(2026, 1, 4, 3, 0), true, true},
	}
	for _, tt := range tests {
		open, reg, note := MarketSession(tt.class, tt.t)
		if open != tt.open || reg != tt.reg {
			t.Errorf("%s: got open=%v regular=%v (%s), want open=%v regular=%v", tt.name, open, reg, note, tt.open, tt.reg)
		}
		if note == "" {
			t.Errorf("%s: empty note", tt.name)
		}
	}
}

func TestMarketSessionUsesNewYorkNotUTC(t *testing.T) {
	// Sat 2026-01-03 03:00 UTC is still Friday 22:00 ET -> equity open.
	open, _, _ := MarketSession("equity", time.Date(2026, 1, 3, 3, 0, 0, 0, time.UTC))
	if !open {
		t.Error("Fri 22:00 ET should be open")
	}
	// Mon 2026-01-05 03:00 UTC is Sun 22:00 ET -> closed.
	open, _, _ = MarketSession("equity", time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC))
	if open {
		t.Error("Sun 22:00 ET should be closed")
	}
}
