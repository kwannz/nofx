package bitget

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const contractsJSON = `{"code":"00000","msg":"success","data":[
{"symbol":"NVDAUSDT","baseCoin":"NVDA","minTradeNum":"0.01","sizeMultiplier":"0.01","volumePlace":"2","pricePlace":"2","priceEndStep":"1","minTradeUSDT":"5","maxLever":"100","minLever":"1","symbolStatus":"normal","fundInterval":"8","isRwa":"YES","takerFeeRate":"0.0006","makerFeeRate":"0.0002"},
{"symbol":"BTCUSDT","baseCoin":"BTC","minTradeNum":"0.0001","sizeMultiplier":"0.0001","volumePlace":"4","pricePlace":"1","priceEndStep":"1","minTradeUSDT":"5","maxLever":"125","minLever":"1","symbolStatus":"maintain","fundInterval":"8","isRwa":"NO","takerFeeRate":"0.0006","makerFeeRate":"0.0002"}]}`

func newServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/mix/market/contracts", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if r.URL.Query().Get("productType") != "USDT-FUTURES" {
			t.Errorf("missing productType")
		}
		fmt.Fprint(w, contractsJSON)
	})
	mux.HandleFunc("/api/v2/mix/market/candles", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("granularity") != "4H" || q.Get("symbol") != "NVDAUSDT" || q.Get("limit") != "3" {
			t.Errorf("unexpected query %v", q)
		}
		// deliberately out of order to test sorting
		fmt.Fprint(w, `{"code":"00000","data":[["2000","2","3","1","2.5","10","25"],["1000","1","2","0.5","1.5","20","30"]]}`)
	})
	mux.HandleFunc("/api/v2/mix/market/ticker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"00000","data":[{"symbol":"BTCUSDT","lastPr":"100.5","markPrice":"100.4","indexPrice":"100.3","fundingRate":"0.0001","bidPr":"100.4","askPr":"100.6","holdingAmount":"55.5","ts":"123"}]}`)
	})
	mux.HandleFunc("/api/v2/mix/market/current-fund-rate", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"00000","data":[{"symbol":"BTCUSDT","fundingRate":"-0.00025"}]}`)
	})
	mux.HandleFunc("/api/v2/mix/market/open-interest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"00000","data":{"openInterestList":[{"symbol":"BTCUSDT","size":"34668.5"}],"ts":"1"}}`)
	})
	return httptest.NewServer(mux)
}

func TestParsing(t *testing.T) {
	var hits int32
	srv := newServer(t, &hits)
	defer srv.Close()
	defer SetBaseURL(srv.URL)()

	c, err := GetContract("nvda")
	if err != nil {
		t.Fatal(err)
	}
	if c.Symbol != "NVDAUSDT" || c.MinTradeNum != 0.01 || c.SizeMultiplier != 0.01 || c.PricePlace != 2 ||
		c.MaxLever != 100 || !c.IsRwa || c.FundInterval != 8 || c.TakerFeeRate != 0.0006 || !c.Tradable() {
		t.Fatalf("bad contract: %+v", c)
	}
	if AssetClass(*c) != ClassEquity {
		t.Fatalf("class = %s", AssetClass(*c))
	}
	// cached: second call must not hit the network
	if _, err := GetContract("NVDAUSDT"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 contracts request, got %d", hits)
	}

	all, err := GetContracts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["BTCUSDT"].Tradable() {
		t.Fatalf("bad contracts map: %+v", all)
	}
	if _, err := GetContracts(); err != nil || hits != 2 {
		t.Fatalf("GetContracts should be cached after first load, hits=%d err=%v", hits, err)
	}

	candles, err := GetCandles("NVDAUSDT", "4h", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(candles) != 2 || candles[0].OpenTime != 1000 || candles[1].Close != 2.5 || candles[1].Volume != 10 || candles[1].QuoteVolume != 25 {
		t.Fatalf("bad candles: %+v", candles)
	}

	tk, err := GetTicker("BTCUSDT")
	if err != nil || tk.LastPrice != 100.5 || tk.MarkPrice != 100.4 || tk.IndexPrice != 100.3 || tk.FundingRate != 0.0001 || tk.HoldingAmount != 55.5 {
		t.Fatalf("bad ticker: %+v %v", tk, err)
	}
	fr, err := GetFundingRate("BTCUSDT")
	if err != nil || fr != -0.00025 {
		t.Fatalf("funding = %v %v", fr, err)
	}
	oi, err := GetOpenInterest("BTCUSDT")
	if err != nil || oi != 34668.5 {
		t.Fatalf("oi = %v %v", oi, err)
	}
}

func TestErrorsAndMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/mix/market/contracts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"00000","data":[]}`)
	})
	mux.HandleFunc("/api/v2/mix/market/open-interest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"40309","msg":"The symbol has been removed"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer SetBaseURL(srv.URL)()

	if _, err := GetContract("NOPEUSDT"); err == nil {
		t.Fatal("expected not found")
	}
	if _, err := GetOpenInterest("NOPEUSDT"); err == nil {
		t.Fatal("expected API error")
	}
	if _, err := GetCandles("BTCUSDT", "7m", 10); err == nil {
		t.Fatal("expected unsupported interval")
	}
}

func TestGranularity(t *testing.T) {
	for in, want := range map[string]string{"3m": "3m", "5m": "5m", "15m": "15m", "1h": "1H", "4h": "4H", "1d": "1D", "1H": "1H"} {
		got, err := Granularity(in)
		if err != nil || got != want {
			t.Errorf("Granularity(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestAssetClass(t *testing.T) {
	tests := []struct {
		symbol, base string
		rwa          bool
		want         string
	}{
		{"BTCUSDT", "BTC", false, ClassCrypto},
		{"PEPUSDT", "PEP", false, ClassCrypto}, // crypto even though PEP is also a stock ticker
		{"NVDAUSDT", "NVDA", true, ClassEquity},
		{"TSLAUSDT", "TSLA", true, ClassEquity},
		{"QQQUSDT", "QQQ", true, ClassEquity},
		{"SPYUSDT", "SPY", true, ClassEquity},
		{"XAUUSDT", "XAU", true, ClassCommodity},
		{"XAGUSDT", "XAG", true, ClassCommodity},
		{"CLUSDT", "CL", true, ClassCommodity},
		{"NATGASUSDT", "NATGAS", true, ClassCommodity},
		{"XAUTUSDT", "XAUT", true, ClassCommodity},
		{"PAXGUSDT", "PAXG", true, ClassCommodity},
		{"COPPERUSDT", "COPPER", true, ClassCommodity},
		{"EURUSDUSDT", "EURUSD", true, ClassFX},
		{"USDJPYUSDT", "USDJPY", true, ClassFX},
		{"GBPUSDUSDT", "GBPUSD", true, ClassFX},
		{"USDBRLUSDT", "USDBRL", true, ClassFX},
		{"XYZUSDT", "", true, ClassEquity}, // base derived from symbol
		{"XAUUSDT", "", true, ClassCommodity},
	}
	for _, tt := range tests {
		got := AssetClass(Contract{Symbol: tt.symbol, BaseCoin: tt.base, IsRwa: tt.rwa})
		if got != tt.want {
			t.Errorf("AssetClass(%s) = %s, want %s", tt.symbol, got, tt.want)
		}
	}
}
