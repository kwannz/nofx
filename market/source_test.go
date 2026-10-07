package market

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nofx/provider/bitget"
)

func bitgetMock(t *testing.T, withOIFunding bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/mix/market/candles", func(w http.ResponseWriter, r *http.Request) {
		sym := r.URL.Query().Get("symbol")
		if sym != "NVDAUSDT" {
			t.Errorf("symbol must stay NVDAUSDT (no xyz mapping), got %s", sym)
		}
		var sb strings.Builder
		sb.WriteString(`{"code":"00000","data":[`)
		for i := 0; i < 120; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			p := 100.0 + float64(i)*0.1 + float64(i%5)*0.05
			fmt.Fprintf(&sb, `["%d","%.2f","%.2f","%.2f","%.2f","10","1000"]`, 1700000000000+int64(i)*180000, p, p+0.3, p-0.3, p+0.1)
		}
		sb.WriteString(`]}`)
		fmt.Fprint(w, sb.String())
	})
	mux.HandleFunc("/api/v2/mix/market/open-interest", func(w http.ResponseWriter, r *http.Request) {
		if withOIFunding {
			fmt.Fprint(w, `{"code":"00000","data":{"openInterestList":[{"symbol":"NVDAUSDT","size":"1234.5"}]}}`)
		} else {
			fmt.Fprint(w, `{"code":"40309","msg":"nope"}`)
		}
	})
	mux.HandleFunc("/api/v2/mix/market/current-fund-rate", func(w http.ResponseWriter, r *http.Request) {
		if withOIFunding {
			fmt.Fprint(w, `{"code":"00000","data":[{"fundingRate":"0.0001"}]}`)
		} else {
			fmt.Fprint(w, `{"code":"40309","msg":"nope"}`)
		}
	})
	return httptest.NewServer(mux)
}

func TestGetWithSourceBitget(t *testing.T) {
	srv := bitgetMock(t, true)
	defer srv.Close()
	defer bitget.SetBaseURL(srv.URL)()

	for _, sym := range []string{"NVDAUSDT", "nvda", "xyz:NVDA"} {
		d, err := GetWithSource(sym, SourceBitget)
		if err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
		if d.Symbol != "NVDAUSDT" || d.CurrentPrice <= 0 || d.IntradaySeries == nil || d.LongerTermContext == nil {
			t.Fatalf("%s: bad data %+v", sym, d)
		}
		if d.OpenInterest == nil || d.OpenInterest.Latest != 1234.5 || d.FundingRate != 0.0001 || d.FundingRateUnavailable {
			t.Fatalf("OI/funding not populated: %+v", d)
		}
	}

	d, err := GetWithTimeframesFromSource("NVDAUSDT", []string{"3m", "4h"}, "3m", 30, SourceBitget)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.TimeframeData) != 2 || d.TimeframeData["3m"] == nil || d.OpenInterest == nil {
		t.Fatalf("bad multi-timeframe data: %+v", d)
	}
}

func TestGetWithSourceBitgetMissingOIFunding(t *testing.T) {
	srv := bitgetMock(t, false)
	defer srv.Close()
	defer bitget.SetBaseURL(srv.URL)()

	d, err := GetWithTimeframesFromSource("NVDAUSDT", []string{"3m"}, "3m", 30, SourceBitget)
	if err != nil {
		t.Fatal(err)
	}
	if d.OpenInterest != nil || !d.FundingRateUnavailable {
		t.Fatalf("expected OI nil and funding unavailable, got %+v", d)
	}
	out := Format(d) // must not panic or print misleading zeros
	if strings.Contains(out, "Funding Rate") || strings.Contains(out, "Open Interest") {
		t.Errorf("formatter printed missing data:\n%s", out)
	}
}

// Every timeframe nofx accepts must have a Bitget granularity (2h used to be missing).
func TestBitgetGranularityCoversSupportedTimeframes(t *testing.T) {
	for _, tf := range SupportedTimeframes() {
		if g, err := bitget.Granularity(tf); err != nil || g == "" {
			t.Errorf("supported timeframe %q has no Bitget granularity: %q, %v", tf, g, err)
		}
	}
	if g, _ := bitget.Granularity("2h"); g != "2H" {
		t.Errorf("2h -> %q, want 2H", g)
	}
}

// A timeframe Bitget has no candles for is skipped when secondary and is a clear error when primary.
func TestGetFromBitgetUnsupportedTimeframes(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/mix/market/candles", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Query().Get("granularity")]++
		mu.Unlock()
		var sb strings.Builder
		sb.WriteString(`{"code":"00000","data":[`)
		for i := 0; i < 60; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			p := 100.0 + float64(i)*0.1
			fmt.Fprintf(&sb, `["%d","%.2f","%.2f","%.2f","%.2f","10","1000"]`, 1700000000000+int64(i)*180000, p, p+0.3, p-0.3, p+0.1)
		}
		sb.WriteString(`]}`)
		fmt.Fprint(w, sb.String())
	})
	mux.HandleFunc("/api/v2/mix/market/open-interest", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"code":"40309","msg":"nope"}`) })
	mux.HandleFunc("/api/v2/mix/market/current-fund-rate", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"code":"40309","msg":"nope"}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer bitget.SetBaseURL(srv.URL)()

	t.Run("non-primary is skipped", func(t *testing.T) {
		d, err := GetWithTimeframesFromSource("BTCUSDT", []string{"3m", "8h", "2h"}, "3m", 20, SourceBitget)
		if err != nil {
			t.Fatal(err)
		}
		if d.TimeframeData["3m"] == nil || d.TimeframeData["2h"] == nil {
			t.Errorf("3m and 2h must be present: %v", d.TimeframeData)
		}
		if _, ok := d.TimeframeData["8h"]; ok {
			t.Error("unsupported 8h must be skipped")
		}
		mu.Lock()
		defer mu.Unlock()
		if seen["2H"] == 0 {
			t.Errorf("2h must be requested as 2H, saw %v", seen)
		}
		for g := range seen {
			if strings.EqualFold(g, "8h") {
				t.Errorf("no request may be sent for an unsupported timeframe, saw %v", seen)
			}
		}
	})

	t.Run("primary is an error", func(t *testing.T) {
		_, err := GetWithTimeframesFromSource("BTCUSDT", []string{"3m", "8h"}, "8h", 20, SourceBitget)
		if err == nil || !strings.Contains(err.Error(), "primary timeframe") || !strings.Contains(err.Error(), "not supported by Bitget") {
			t.Fatalf("want a clear primary-timeframe error, got %v", err)
		}
	})
}

func TestSourceForExchange(t *testing.T) {
	cases := map[string]Source{
		"bitget": SourceBitget, "bitget_paper": SourceBitget, "BITGET": SourceBitget, " Bitget_Paper ": SourceBitget,
		"binance": SourceDefault, "hyperliquid": SourceDefault, "okx": SourceDefault, "": SourceDefault,
		"bitget_future": SourceDefault,
	}
	for ex, want := range cases {
		if got := SourceForExchange(ex); got != want {
			t.Errorf("SourceForExchange(%q) = %q, want %q", ex, got, want)
		}
	}
}

func TestBitgetKlinesExported(t *testing.T) {
	srv := bitgetMock(t, false)
	defer srv.Close()
	defer bitget.SetBaseURL(srv.URL)()

	ks, err := BitgetKlines("NVDAUSDT", "3m", 50)
	if err != nil || len(ks) != 120 {
		t.Fatalf("BitgetKlines: %d klines, err=%v", len(ks), err)
	}
	if ks[0].CloseTime != ks[0].OpenTime+3*60*1000-1 || ks[0].Open <= 0 || ks[0].QuoteVolume != 1000 {
		t.Fatalf("kline not converted: %+v", ks[0])
	}
	if _, err := BitgetKlines("NVDAUSDT", "7m", 50); err == nil {
		t.Fatal("unsupported timeframe must fail")
	}
}

func TestNormalizeForSource(t *testing.T) {
	cases := map[string]string{"NVDAUSDT": "NVDAUSDT", "nvda": "NVDAUSDT", "xyz:NVDA": "NVDAUSDT", "btc": "BTCUSDT", "XAUUSDT": "XAUUSDT"}
	for in, want := range cases {
		if got := NormalizeForSource(in, SourceBitget); got != want {
			t.Errorf("NormalizeForSource(%q, bitget) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeForSource("NVDAUSDT", SourceDefault); got != Normalize("NVDAUSDT") {
		t.Errorf("default source must equal Normalize, got %q", got)
	}
}

// TestLiveBitgetSource uses the real Bitget API. Skipped with -short.
func TestLiveBitgetSource(t *testing.T) {
	if testing.Short() {
		t.Skip("live network test")
	}
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "NVDAUSDT", "XAUUSDT"} {
		d, err := GetWithTimeframesFromSource(sym, []string{"3m", "15m", "4h"}, "3m", 30, SourceBitget)
		if err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
		if d.Symbol != sym || d.CurrentPrice <= 0 || len(d.TimeframeData) != 3 {
			t.Fatalf("%s: bad data", sym)
		}
		t.Logf("%s price=%.4f oi=%v funding=%.6f (unavailable=%v) 1h=%+.2f%%", sym, d.CurrentPrice, d.OpenInterest, d.FundingRate, d.FundingRateUnavailable, d.PriceChange1h)
		legacy, err := GetWithSource(sym, SourceBitget)
		if err != nil || legacy.IntradaySeries == nil {
			t.Fatalf("%s legacy: %v", sym, err)
		}
	}
}
