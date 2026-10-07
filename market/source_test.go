package market

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
