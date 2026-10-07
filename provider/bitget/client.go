// Package bitget wraps Bitget's PUBLIC (unauthenticated) USDT-futures market
// data endpoints: contract metadata, candles, tickers, funding and open interest.
//
// It intentionally has no dependency on the market package so that market can
// import it without an import cycle; callers convert Candle to their own types.
package bitget

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"nofx/hook"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the Bitget public API host.
	DefaultBaseURL = "https://api.bitget.com"
	// ProductType is the only product type used by nofx.
	ProductType = "USDT-FUTURES"

	contractCacheTTL = 10 * time.Minute
	// contractMissTTL is how long "symbol not found" is remembered, so an unknown or delisted
	// symbol proposed every cycle does not cost an HTTP request each time.
	contractMissTTL = 60 * time.Second
	// contractErrTTL is how long a failed bulk load is remembered (fail fast for the other
	// symbols of the same cycle) while keeping outages short: the next attempt after it retries.
	contractErrTTL = 5 * time.Second
)

var (
	baseURL = DefaultBaseURL

	httpMu     sync.Mutex
	httpClient *http.Client

	contractMu    sync.Mutex
	contractCache = map[string]cachedContract{}
	// allFetchedAt is when the full contract list was last loaded.
	allFetchedAt time.Time
	// missCache holds symbols known to be absent (symbol -> expiry of the negative entry).
	missCache = map[string]time.Time{}
	// bulkErr / bulkErrAt remember the last failed full-list load for contractErrTTL.
	bulkErr   error
	bulkErrAt time.Time
	// refreshMu serialises full-list loads so concurrent lookups share a single request.
	refreshMu sync.Mutex

	// nowFn is the clock (a seam for tests).
	nowFn = time.Now
)

// ContractNotFoundError is returned by GetContract when the symbol is not a Bitget USDT-futures
// contract (never listed, or delisted). Use IsContractNotFound to tell it from network errors.
type ContractNotFoundError struct{ Symbol string }

func (e *ContractNotFoundError) Error() string {
	return fmt.Sprintf("bitget: contract %s not found", e.Symbol)
}

// IsContractNotFound reports whether err says the symbol does not exist (as opposed to a lookup failure).
func IsContractNotFound(err error) bool {
	var nf *ContractNotFoundError
	return errors.As(err, &nf)
}

type cachedContract struct {
	c         Contract
	fetchedAt time.Time
}

// SetBaseURL overrides the API host (tests). Returns a restore func. It also
// clears the contract cache.
func SetBaseURL(u string) (restore func()) {
	old := baseURL
	baseURL = strings.TrimRight(u, "/")
	ResetCache()
	return func() {
		baseURL = old
		ResetCache()
	}
}

// ResetCache drops all cached contract metadata.
func ResetCache() {
	contractMu.Lock()
	contractCache = map[string]cachedContract{}
	allFetchedAt = time.Time{}
	missCache = map[string]time.Time{}
	bulkErr, bulkErrAt = nil, time.Time{}
	contractMu.Unlock()
}

func getHTTPClient() *http.Client {
	httpMu.Lock()
	defer httpMu.Unlock()
	if httpClient != nil {
		return httpClient
	}
	c := &http.Client{Timeout: 20 * time.Second}
	// Same convention as market.NewAPIClient: let the hook inject a proxy client.
	if res := hook.HookExec[hook.SetHttpClientResult](hook.SET_HTTP_CLIENT, c); res != nil && res.Error() == nil {
		c = res.GetResult()
	}
	httpClient = c
	return c
}

type envelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// get performs a public GET and returns the raw `data` payload.
func get(path string, q url.Values) (json.RawMessage, error) {
	u := baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := getHTTPClient().Get(u)
	if err != nil {
		return nil, fmt.Errorf("bitget GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bitget read %s: %w", path, err)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("bitget %s: HTTP %d, invalid JSON: %w", path, resp.StatusCode, err)
	}
	if env.Code != "00000" {
		return nil, fmt.Errorf("bitget %s: code=%s msg=%s", path, env.Code, env.Msg)
	}
	return env.Data, nil
}

func marketQuery(symbol string) url.Values {
	q := url.Values{}
	q.Set("productType", ProductType)
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	return q
}

func pf(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// normSymbol upper-cases and appends USDT when missing.
func normSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s != "" && !strings.HasSuffix(s, "USDT") {
		s += "USDT"
	}
	return s
}

// ---------------------------------------------------------------- contracts

// Contract is the Bitget USDT-futures contract metadata (all numeric fields parsed).
type Contract struct {
	Symbol         string
	BaseCoin       string
	MinTradeNum    float64
	SizeMultiplier float64
	VolumePlace    int
	PricePlace     int
	PriceEndStep   float64
	MinTradeUSDT   float64
	MaxLever       float64
	MinLever       float64
	SymbolStatus   string
	FundInterval   int // hours
	IsRwa          bool
	TakerFeeRate   float64
	MakerFeeRate   float64
}

// Tradable reports whether the contract is currently open for trading.
func (c Contract) Tradable() bool {
	return strings.EqualFold(c.SymbolStatus, "normal")
}

// AssetClass classifies the contract. See AssetClass (package func).
func (c Contract) AssetClass() string { return AssetClass(c) }

type rawContract struct {
	Symbol         string `json:"symbol"`
	BaseCoin       string `json:"baseCoin"`
	MinTradeNum    string `json:"minTradeNum"`
	SizeMultiplier string `json:"sizeMultiplier"`
	VolumePlace    string `json:"volumePlace"`
	PricePlace     string `json:"pricePlace"`
	PriceEndStep   string `json:"priceEndStep"`
	MinTradeUSDT   string `json:"minTradeUSDT"`
	MaxLever       string `json:"maxLever"`
	MinLever       string `json:"minLever"`
	SymbolStatus   string `json:"symbolStatus"`
	FundInterval   string `json:"fundInterval"`
	IsRwa          string `json:"isRwa"`
	TakerFeeRate   string `json:"takerFeeRate"`
	MakerFeeRate   string `json:"makerFeeRate"`
}

func (r rawContract) convert() Contract {
	vp, _ := strconv.Atoi(r.VolumePlace)
	pp, _ := strconv.Atoi(r.PricePlace)
	fi, _ := strconv.Atoi(r.FundInterval)
	return Contract{
		Symbol:         strings.ToUpper(r.Symbol),
		BaseCoin:       strings.ToUpper(r.BaseCoin),
		MinTradeNum:    pf(r.MinTradeNum),
		SizeMultiplier: pf(r.SizeMultiplier),
		VolumePlace:    vp,
		PricePlace:     pp,
		PriceEndStep:   pf(r.PriceEndStep),
		MinTradeUSDT:   pf(r.MinTradeUSDT),
		MaxLever:       pf(r.MaxLever),
		MinLever:       pf(r.MinLever),
		SymbolStatus:   r.SymbolStatus,
		FundInterval:   fi,
		IsRwa:          strings.EqualFold(r.IsRwa, "YES"),
		TakerFeeRate:   pf(r.TakerFeeRate),
		MakerFeeRate:   pf(r.MakerFeeRate),
	}
}

func fetchContracts(symbol string) ([]Contract, error) {
	data, err := get("/api/v2/mix/market/contracts", marketQuery(symbol))
	if err != nil {
		return nil, err
	}
	var raws []rawContract
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("bitget contracts: parse: %w", err)
	}
	out := make([]Contract, 0, len(raws))
	for _, r := range raws {
		out = append(out, r.convert())
	}
	return out, nil
}

// contractsFreshLocked reports whether the full contract list is still within its TTL. contractMu must be held.
func contractsFreshLocked(now time.Time) bool {
	return !allFetchedAt.IsZero() && now.Sub(allFetchedAt) < contractCacheTTL
}

// refreshAllContracts loads the full contract list with ONE request and replaces the per-symbol
// cache with it. It is a no-op when the list is fresh (another goroutine may have just loaded
// it) and fails fast for contractErrTTL after a failed load. Network errors are never cached
// beyond that, so an outage recovers on the next attempt.
func refreshAllContracts() error {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	contractMu.Lock()
	now := nowFn()
	if contractsFreshLocked(now) {
		contractMu.Unlock()
		return nil
	}
	if bulkErr != nil && now.Sub(bulkErrAt) < contractErrTTL {
		err := bulkErr
		contractMu.Unlock()
		return err
	}
	contractMu.Unlock()

	list, err := fetchContracts("")

	contractMu.Lock()
	defer contractMu.Unlock()
	if err != nil {
		bulkErr, bulkErrAt = err, nowFn()
		return err
	}
	fetched := nowFn()
	contractCache = make(map[string]cachedContract, len(list))
	for _, c := range list {
		contractCache[c.Symbol] = cachedContract{c: c, fetchedAt: fetched}
	}
	// the new list is authoritative: forget old "not found" verdicts
	missCache = map[string]time.Time{}
	allFetchedAt = fetched
	bulkErr, bulkErrAt = nil, time.Time{}
	return nil
}

// GetContracts returns all USDT-futures contracts keyed by symbol. The result is
// cached for ~10 minutes.
func GetContracts() (map[string]Contract, error) {
	if err := refreshAllContracts(); err != nil {
		return nil, err
	}
	contractMu.Lock()
	defer contractMu.Unlock()
	out := make(map[string]Contract, len(contractCache))
	for k, v := range contractCache {
		out[k] = v.c
	}
	return out, nil
}

// GetContract returns metadata for one symbol.
//
// Lookups are served from a per-symbol cache (~10 min). When the cache is cold or stale it is
// repopulated from a single full-list request instead of one request per symbol. A symbol that
// is not found is remembered for a minute (negative cache) so repeated lookups of an unknown or
// delisted symbol stay local; network errors are not cached (see refreshAllContracts).
func GetContract(symbol string) (*Contract, error) {
	symbol = normSymbol(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("bitget: empty symbol")
	}

	contractMu.Lock()
	now := nowFn()
	if e, ok := contractCache[symbol]; ok && now.Sub(e.fetchedAt) < contractCacheTTL {
		c := e.c
		contractMu.Unlock()
		return &c, nil
	}
	if until, ok := missCache[symbol]; ok {
		if now.Before(until) {
			contractMu.Unlock()
			return nil, &ContractNotFoundError{Symbol: symbol}
		}
		delete(missCache, symbol)
	}
	fresh := contractsFreshLocked(now)
	contractMu.Unlock()

	if !fresh {
		// Cold or stale: one request fills the cache for every symbol.
		if err := refreshAllContracts(); err != nil {
			return nil, err
		}
		contractMu.Lock()
		e, ok := contractCache[symbol]
		contractMu.Unlock()
		if ok {
			c := e.c
			return &c, nil
		}
		// absent from the list we just loaded: definitive
		return nil, rememberMissing(symbol)
	}

	// The list is fresh but does not hold the symbol (e.g. listed after the last full load):
	// confirm with a single-symbol request, at most once per negative-cache window.
	list, err := fetchContracts(symbol)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.Symbol == symbol {
			contractMu.Lock()
			contractCache[symbol] = cachedContract{c: c, fetchedAt: nowFn()}
			delete(missCache, symbol)
			contractMu.Unlock()
			cc := c
			return &cc, nil
		}
	}
	return nil, rememberMissing(symbol)
}

// rememberMissing records a negative cache entry and returns the not-found error.
func rememberMissing(symbol string) error {
	contractMu.Lock()
	missCache[symbol] = nowFn().Add(contractMissTTL)
	contractMu.Unlock()
	return &ContractNotFoundError{Symbol: symbol}
}

// ----------------------------------------------------------- asset class

var (
	commodityBases = map[string]bool{
		"XAU": true, "XAG": true, "XPT": true, "XPD": true, "CL": true, "BZ": true,
		"NATGAS": true, "COPPER": true, "COP": true, "XAUT": true, "PAXG": true,
	}
	fxBases = map[string]bool{
		"EURUSD": true, "USDJPY": true, "GBPUSD": true, "USDBRL": true,
	}
)

// Asset class names.
const (
	ClassCrypto    = "crypto"
	ClassEquity    = "equity"
	ClassCommodity = "commodity"
	ClassFX        = "fx"
)

// AssetClass returns "crypto" | "equity" | "commodity" | "fx".
func AssetClass(c Contract) string {
	if !c.IsRwa {
		return ClassCrypto
	}
	base := strings.ToUpper(c.BaseCoin)
	if base == "" {
		base = strings.TrimSuffix(strings.ToUpper(c.Symbol), "USDT")
	}
	if commodityBases[base] {
		return ClassCommodity
	}
	if fxBases[base] {
		return ClassFX
	}
	return ClassEquity
}

// ---------------------------------------------------------------- candles

// Candle is one OHLCV bar. Times are in milliseconds.
type Candle struct {
	OpenTime    int64
	Open        float64
	High        float64
	Low         float64
	Close       float64
	Volume      float64 // base volume
	QuoteVolume float64
}

// granularity maps lower-cased nofx timeframes to Bitget candle granularities. It covers every
// timeframe market.SupportedTimeframes() accepts (1m..1d, including 2h: Bitget accepts "2H",
// verified live) plus 3d and 1w. Bitget's monthly "1M" cannot be keyed here because the lookup
// lower-cases its input and "1m" is the minute bar. Bitget has no 8H bar, so "8h" stays unsupported.
var granularity = map[string]string{
	"1m": "1m", "3m": "3m", "5m": "5m", "15m": "15m", "30m": "30m",
	"1h": "1H", "2h": "2H", "4h": "4H", "6h": "6H", "12h": "12H",
	"1d": "1D", "3d": "3D", "1w": "1W",
}

// Granularity maps a nofx timeframe ("5m", "1h", "4h", "1d") to Bitget's value.
func Granularity(interval string) (string, error) {
	g, ok := granularity[strings.ToLower(strings.TrimSpace(interval))]
	if !ok {
		return "", fmt.Errorf("bitget: unsupported interval %q", interval)
	}
	return g, nil
}

// GetCandles returns up to limit candles (max 1000), ascending by time.
func GetCandles(symbol, interval string, limit int) ([]Candle, error) {
	g, err := Granularity(interval)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := marketQuery(normSymbol(symbol))
	q.Set("granularity", g)
	q.Set("limit", strconv.Itoa(limit))
	data, err := get("/api/v2/mix/market/candles", q)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("bitget candles: parse: %w", err)
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		ts, err := strconv.ParseInt(r[0], 10, 64)
		if err != nil {
			continue
		}
		c := Candle{OpenTime: ts, Open: pf(r[1]), High: pf(r[2]), Low: pf(r[3]), Close: pf(r[4]), Volume: pf(r[5])}
		if len(r) > 6 {
			c.QuoteVolume = pf(r[6])
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OpenTime < out[j].OpenTime })
	return out, nil
}

// ----------------------------------------------------------------- ticker

// Ticker is a market snapshot.
type Ticker struct {
	Symbol      string
	LastPrice   float64
	MarkPrice   float64
	IndexPrice  float64
	FundingRate float64
	BidPrice    float64
	AskPrice    float64
	// HoldingAmount is open interest in base units.
	HoldingAmount float64
	Timestamp     int64
}

// GetTicker returns the ticker for one symbol.
func GetTicker(symbol string) (*Ticker, error) {
	symbol = normSymbol(symbol)
	data, err := get("/api/v2/mix/market/ticker", marketQuery(symbol))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Symbol        string `json:"symbol"`
		LastPr        string `json:"lastPr"`
		MarkPrice     string `json:"markPrice"`
		IndexPrice    string `json:"indexPrice"`
		FundingRate   string `json:"fundingRate"`
		BidPr         string `json:"bidPr"`
		AskPr         string `json:"askPr"`
		HoldingAmount string `json:"holdingAmount"`
		Ts            string `json:"ts"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("bitget ticker: parse: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("bitget ticker: no data for %s", symbol)
	}
	r := rows[0]
	ts, _ := strconv.ParseInt(r.Ts, 10, 64)
	return &Ticker{
		Symbol: r.Symbol, LastPrice: pf(r.LastPr), MarkPrice: pf(r.MarkPrice), IndexPrice: pf(r.IndexPrice),
		FundingRate: pf(r.FundingRate), BidPrice: pf(r.BidPr), AskPrice: pf(r.AskPr),
		HoldingAmount: pf(r.HoldingAmount), Timestamp: ts,
	}, nil
}

// GetFundingRate returns the current funding rate.
func GetFundingRate(symbol string) (float64, error) {
	data, err := get("/api/v2/mix/market/current-fund-rate", marketQuery(normSymbol(symbol)))
	if err != nil {
		return 0, err
	}
	var rows []struct {
		FundingRate string `json:"fundingRate"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return 0, fmt.Errorf("bitget funding: parse: %w", err)
	}
	if len(rows) == 0 || rows[0].FundingRate == "" {
		return 0, fmt.Errorf("bitget funding: no data for %s", symbol)
	}
	return pf(rows[0].FundingRate), nil
}

// GetOpenInterest returns open interest in base units.
func GetOpenInterest(symbol string) (float64, error) {
	data, err := get("/api/v2/mix/market/open-interest", marketQuery(normSymbol(symbol)))
	if err != nil {
		return 0, err
	}
	var d struct {
		OpenInterestList []struct {
			Size string `json:"size"`
		} `json:"openInterestList"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return 0, fmt.Errorf("bitget open-interest: parse: %w", err)
	}
	if len(d.OpenInterestList) == 0 || d.OpenInterestList[0].Size == "" {
		return 0, fmt.Errorf("bitget open-interest: no data for %s", symbol)
	}
	return pf(d.OpenInterestList[0].Size), nil
}
