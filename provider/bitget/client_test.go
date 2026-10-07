package bitget

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	// GetContract("nvda") already loaded the whole list with one request: GetContracts reuses it
	if _, err := GetContracts(); err != nil || hits != 1 {
		t.Fatalf("GetContracts should reuse the list loaded by GetContract, hits=%d err=%v", hits, err)
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
	for in, want := range map[string]string{
		"1m": "1m", "3m": "3m", "5m": "5m", "15m": "15m", "30m": "30m",
		"1h": "1H", "1H": "1H", "2h": "2H", "2H": "2H", "4h": "4H", "6h": "6H", "12h": "12H",
		"1d": "1D", "3d": "3D", "1w": "1W", " 4H ": "4H",
	} {
		got, err := Granularity(in)
		if err != nil || got != want {
			t.Errorf("Granularity(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// no Bitget bar for these (or ambiguous once lower-cased)
	for _, in := range []string{"", "7m", "8h", "2d", "1M2", "abc"} {
		if got, err := Granularity(in); err == nil {
			t.Errorf("Granularity(%q) = %q, want an error", in, got)
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

// ---------------------------------------------------------------- contract cache

type contractServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	bulk     int // requests without a symbol
	single   int // requests with a symbol
	failNext int // number of upcoming requests that return a server error
	extra    map[string]bool
}

func newContractServer(t *testing.T) *contractServer {
	t.Helper()
	cs := &contractServer{extra: map[string]bool{}}
	row := func(sym string) string {
		return fmt.Sprintf(`{"symbol":"%s","baseCoin":"%s","minTradeNum":"1","sizeMultiplier":"1","volumePlace":"0","pricePlace":"2","priceEndStep":"1","minTradeUSDT":"5","maxLever":"50","minLever":"1","symbolStatus":"normal","fundInterval":"8","isRwa":"NO","takerFeeRate":"0.0006","makerFeeRate":"0.0002"}`, sym, strings.TrimSuffix(sym, "USDT"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/mix/market/contracts", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		sym := r.URL.Query().Get("symbol")
		if sym == "" {
			cs.bulk++
		} else {
			cs.single++
		}
		if cs.failNext > 0 {
			cs.failNext--
			cs.mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>bad gateway</html>")
			return
		}
		listed := []string{"BTCUSDT", "ETHUSDT", "NVDAUSDT"}
		for s := range cs.extra {
			listed = append(listed, s)
		}
		cs.mu.Unlock()
		var rows []string
		for _, s := range listed {
			if sym == "" || sym == s {
				rows = append(rows, row(s))
			}
		}
		fmt.Fprintf(w, `{"code":"00000","data":[%s]}`, strings.Join(rows, ","))
	})
	cs.srv = httptest.NewServer(mux)
	t.Cleanup(cs.srv.Close)
	restore := SetBaseURL(cs.srv.URL)
	t.Cleanup(restore)
	return cs
}

func (cs *contractServer) counts() (bulk, single int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.bulk, cs.single
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func withTestClock(t *testing.T) *testClock {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	old := nowFn
	nowFn = clk.now
	t.Cleanup(func() { nowFn = old })
	return clk
}

func TestContractColdCacheUsesOneBulkRequest(t *testing.T) {
	cs := newContractServer(t)
	withTestClock(t)
	for _, sym := range []string{"BTCUSDT", "eth", "NVDAUSDT", "BTCUSDT", "ETHUSDT"} {
		if _, err := GetContract(sym); err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
	}
	if bulk, single := cs.counts(); bulk != 1 || single != 0 {
		t.Fatalf("requests: bulk=%d single=%d, want 1 and 0", bulk, single)
	}
}

func TestContractColdCacheConcurrentLookupsShareOneRequest(t *testing.T) {
	cs := newContractServer(t)
	withTestClock(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sym := []string{"BTCUSDT", "ETHUSDT", "NVDAUSDT"}[i%3]
			if _, err := GetContract(sym); err != nil {
				t.Errorf("%s: %v", sym, err)
			}
		}(i)
	}
	wg.Wait()
	if bulk, single := cs.counts(); bulk != 1 || single != 0 {
		t.Fatalf("requests: bulk=%d single=%d, want 1 and 0", bulk, single)
	}
}

func TestContractNegativeCache(t *testing.T) {
	cs := newContractServer(t)
	clk := withTestClock(t)

	if _, err := GetContract("BTCUSDT"); err != nil { // warm the list
		t.Fatal(err)
	}
	_, err := GetContract("FOOUSDT")
	if !IsContractNotFound(err) {
		t.Fatalf("want not-found error, got %v", err)
	}
	if bulk, single := cs.counts(); bulk != 1 || single != 1 {
		t.Fatalf("first miss: bulk=%d single=%d, want 1 and 1 (one confirm request)", bulk, single)
	}
	for i := 0; i < 5; i++ {
		if _, err := GetContract("fooUSDT"); !IsContractNotFound(err) {
			t.Fatalf("want not-found, got %v", err)
		}
	}
	if _, single := cs.counts(); single != 1 {
		t.Fatalf("misses inside the negative window must not hit the network, single=%d", single)
	}

	// the verdict expires after ~60s and the symbol is looked up again
	cs.mu.Lock()
	cs.extra["FOOUSDT"] = true // listed in the meantime
	cs.mu.Unlock()
	clk.advance(contractMissTTL + time.Second)
	c, err := GetContract("FOOUSDT")
	if err != nil || c.Symbol != "FOOUSDT" {
		t.Fatalf("after the negative TTL the new listing must be found: %v %v", c, err)
	}
	if _, single := cs.counts(); single != 2 {
		t.Fatalf("single=%d, want 2", single)
	}
	if _, err := GetContract("FOOUSDT"); err != nil {
		t.Fatal(err)
	}
	if _, single := cs.counts(); single != 2 {
		t.Fatalf("found symbol must be cached, single=%d", single)
	}
}

func TestContractNotFoundAfterColdBulkNeedsNoConfirmRequest(t *testing.T) {
	cs := newContractServer(t)
	withTestClock(t)
	if _, err := GetContract("FOOUSDT"); !IsContractNotFound(err) {
		t.Fatalf("want not-found, got %v", err)
	}
	if bulk, single := cs.counts(); bulk != 1 || single != 0 {
		t.Fatalf("bulk=%d single=%d, want 1 and 0 (the list just loaded is authoritative)", bulk, single)
	}
	if _, err := GetContract("FOOUSDT"); !IsContractNotFound(err) {
		t.Fatal("want cached not-found")
	}
	if bulk, single := cs.counts(); bulk != 1 || single != 0 {
		t.Fatalf("negative entry must absorb the repeat: bulk=%d single=%d", bulk, single)
	}
}

func TestContractNetworkErrorsAreNotCachedLong(t *testing.T) {
	cs := newContractServer(t)
	clk := withTestClock(t)
	cs.mu.Lock()
	cs.failNext = 1
	cs.mu.Unlock()

	_, err := GetContract("BTCUSDT")
	if err == nil || IsContractNotFound(err) {
		t.Fatalf("an outage must surface as a lookup error, not as not-found: %v", err)
	}
	// fail fast inside the short error window ...
	if _, err2 := GetContract("ETHUSDT"); err2 == nil || IsContractNotFound(err2) {
		t.Fatalf("expected the remembered error, got %v", err2)
	}
	if bulk, _ := cs.counts(); bulk != 1 {
		t.Fatalf("second lookup inside the error window must not hit the network, bulk=%d", bulk)
	}
	// ... and recover afterwards; the symbol was never marked as missing
	clk.advance(contractErrTTL + time.Second)
	if c, err := GetContract("BTCUSDT"); err != nil || c.Symbol != "BTCUSDT" {
		t.Fatalf("must recover after the outage: %v %v", c, err)
	}
	if bulk, _ := cs.counts(); bulk != 2 {
		t.Fatalf("bulk=%d, want 2", bulk)
	}

	// single-symbol confirm errors are not cached at all
	cs.mu.Lock()
	cs.failNext = 1
	cs.mu.Unlock()
	if _, err := GetContract("BARUSDT"); err == nil || IsContractNotFound(err) {
		t.Fatalf("confirm request failure must be a lookup error: %v", err)
	}
	cs.mu.Lock()
	cs.extra["BARUSDT"] = true
	cs.mu.Unlock()
	if c, err := GetContract("BARUSDT"); err != nil || c.Symbol != "BARUSDT" {
		t.Fatalf("a failed confirm request must not leave a negative entry: %v %v", c, err)
	}
}

func TestContractStaleCacheReloadsOnce(t *testing.T) {
	cs := newContractServer(t)
	clk := withTestClock(t)
	if _, err := GetContract("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	clk.advance(contractCacheTTL + time.Minute)
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "NVDAUSDT"} {
		if _, err := GetContract(sym); err != nil {
			t.Fatal(err)
		}
	}
	if bulk, single := cs.counts(); bulk != 2 || single != 0 {
		t.Fatalf("bulk=%d single=%d, want 2 and 0", bulk, single)
	}
}
