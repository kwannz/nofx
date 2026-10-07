package bitget

import (
	"testing"

	"nofx/internal/testutil"
)

// TestLiveBitget hits the real Bitget public API. Skipped unless NOFX_LIVE_TESTS=1.
func TestLiveBitget(t *testing.T) {
	testutil.RequireLive(t)
	ResetCache()
	wantClass := map[string]string{
		"BTCUSDT": ClassCrypto, "ETHUSDT": ClassCrypto, "NVDAUSDT": ClassEquity, "XAUUSDT": ClassCommodity,
	}
	for sym, cls := range wantClass {
		c, err := GetContract(sym)
		if err != nil {
			t.Fatalf("%s contract: %v", sym, err)
		}
		if got := AssetClass(*c); got != cls {
			t.Errorf("%s class = %s, want %s", sym, got, cls)
		}
		if c.MaxLever <= 0 || c.MinTradeNum <= 0 || c.SizeMultiplier <= 0 {
			t.Errorf("%s suspicious contract: %+v", sym, c)
		}
		candles, err := GetCandles(sym, "5m", 50)
		if err != nil {
			t.Fatalf("%s candles: %v", sym, err)
		}
		if len(candles) < 10 {
			t.Fatalf("%s only %d candles", sym, len(candles))
		}
		for i := 1; i < len(candles); i++ {
			if candles[i].OpenTime <= candles[i-1].OpenTime {
				t.Fatalf("%s candles not ascending", sym)
			}
		}
		last := candles[len(candles)-1]
		if last.Close <= 0 || last.High < last.Low {
			t.Errorf("%s bad last candle %+v", sym, last)
		}
		t.Logf("%s: class=%s maxLever=%v status=%s candles=%d last=%.4f", sym, cls, c.MaxLever, c.SymbolStatus, len(candles), last.Close)
	}
	all, err := GetContracts()
	if err != nil || len(all) < 100 {
		t.Fatalf("GetContracts: %d, %v", len(all), err)
	}
	tk, err := GetTicker("BTCUSDT")
	if err != nil || tk.LastPrice <= 0 || tk.MarkPrice <= 0 {
		t.Fatalf("ticker: %+v %v", tk, err)
	}
	if _, err := GetFundingRate("BTCUSDT"); err != nil {
		t.Errorf("funding: %v", err)
	}
	if oi, err := GetOpenInterest("BTCUSDT"); err != nil || oi <= 0 {
		t.Errorf("oi: %v %v", oi, err)
	}
}
