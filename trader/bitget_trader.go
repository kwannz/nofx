package trader

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"nofx/logger"
	"nofx/store"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bitget API endpoints (V2)
const (
	bitgetDefaultBaseURL   = "https://api.bitget.com"
	bitgetProductType      = "USDT-FUTURES"
	bitgetAccountPath      = "/api/v2/mix/account/accounts"
	bitgetPositionPath     = "/api/v2/mix/position/all-position"
	bitgetOrderPath        = "/api/v2/mix/order/place-order"
	bitgetLeveragePath     = "/api/v2/mix/account/set-leverage"
	bitgetTickerPath       = "/api/v2/mix/market/ticker"
	bitgetContractsPath    = "/api/v2/mix/market/contracts"
	bitgetCancelOrderPath  = "/api/v2/mix/order/cancel-order"
	bitgetPendingPath      = "/api/v2/mix/order/orders-pending"
	bitgetHistoryPath      = "/api/v2/mix/order/orders-history"
	bitgetMarginModePath   = "/api/v2/mix/account/set-margin-mode"
	bitgetPositionModePath = "/api/v2/mix/account/set-position-mode"
	// bitgetAccountDetailPath returns one margin-coin account incl. posMode (hedge_mode /
	// one_way_mode). The list endpoint (bitgetAccountPath) does NOT report the position mode.
	bitgetAccountDetailPath = "/api/v2/mix/account/account"
	bitgetPlaceTPSLPath     = "/api/v2/mix/order/place-tpsl-order"
	bitgetPlanPendingPath   = "/api/v2/mix/order/orders-plan-pending"
	bitgetCancelPlanPath    = "/api/v2/mix/order/cancel-plan-order"
	bitgetOrderDetailPath   = "/api/v2/mix/order/detail"
	bitgetHistoryPosPath    = "/api/v2/mix/position/history-position"
	// bitgetFillsPath lists trade fills. NOTE: /api/v2/mix/order/fill-history does NOT exist
	// (verified on Bitget Demo: code 40404 "Request URL NOT FOUND"); /fills is the real endpoint.
	bitgetFillsPath = "/api/v2/mix/order/fills"
)

// BitgetTrader Bitget futures trader
type BitgetTrader struct {
	apiKey     string
	secretKey  string
	passphrase string

	// baseURL is the REST endpoint (field so tests can point at httptest).
	baseURL string
	// demo enables Bitget demo trading: every request carries "paptrading: 1".
	demo bool

	// HTTP client
	httpClient *http.Client

	// Balance cache
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// Positions cache
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// Contract info cache (per-symbol fetch timestamp)
	contractsCache      map[string]*bitgetCachedContract
	contractsCacheMutex sync.RWMutex

	// Margin mode per symbol ("crossed" / "isolated"), defaults to crossed
	marginModes     map[string]string
	marginModeMutex sync.RWMutex

	// Position mode. targetPosMode is what the user configured (bitget_position_mode);
	// posMode is the mode the ACCOUNT is actually in (detected / switched / refreshed after a
	// 40774 rejection), "" while unknown. Both hold the API vocabulary (hedge_mode /
	// one_way_mode) and are guarded by posModeMu.
	posModeMu     sync.RWMutex
	targetPosMode string
	posMode       string

	// Order fill polling
	orderPollAttempts int
	orderPollInterval time.Duration

	// Cache duration
	cacheDuration time.Duration
}

type bitgetCachedContract struct {
	contract  *BitgetContract
	fetchedAt time.Time
}

const bitgetContractCacheTTL = 5 * time.Minute

// BitgetContract Bitget contract info
type BitgetContract struct {
	Symbol         string  // Symbol name
	BaseCoin       string  // Base coin
	QuoteCoin      string  // Quote coin
	MinTradeNum    float64 // Minimum trade amount (base units)
	MaxTradeNum    float64 // Maximum trade amount
	SizeMultiplier float64 // Order size step
	PricePlace     int     // Price decimal places
	PriceEndStep   float64 // Price step in units of 10^-PricePlace
	VolumePlace    int     // Volume decimal places
	MinTradeUSDT   float64 // Minimum notional in USDT
	MaxLever       int     // Maximum leverage
	SymbolStatus   string  // normal / maintain / ...
	IsRwa          bool    // Real-world asset (stocks/commodities/fx)
	TakerFeeRate   float64
	MakerFeeRate   float64
	FundInterval   int // funding interval in hours
}

// PriceTick returns the minimum price increment.
func (c *BitgetContract) PriceTick() float64 {
	step := c.PriceEndStep
	if step <= 0 {
		step = 1
	}
	return step * math.Pow(10, -float64(c.PricePlace))
}

// BitgetResponse Bitget API response
type BitgetResponse struct {
	Code        string          `json:"code"`
	Msg         string          `json:"msg"`
	Data        json.RawMessage `json:"data"`
	RequestTime int64           `json:"requestTime"`
}

// BitgetAPIError is returned when Bitget responds with a non-"00000" code.
type BitgetAPIError struct {
	Code string
	Msg  string
}

func (e *BitgetAPIError) Error() string {
	if hint := bitgetErrorHint(e.Code); hint != "" {
		return fmt.Sprintf("Bitget API error: code=%s, msg=%s (%s)", e.Code, e.Msg, hint)
	}
	return fmt.Sprintf("Bitget API error: code=%s, msg=%s", e.Code, e.Msg)
}

// bitgetErrorHint maps common Bitget error codes to clearer explanations.
func bitgetErrorHint(code string) string {
	switch code {
	case "40774":
		return "account position mode does not match the order (hedge mode needs tradeSide=open|close, one-way mode must not send tradeSide; the trader re-detects the mode and retries once)"
	case "45110":
		return "order amount is below the minimum order size/value"
	case "40762", "43012":
		return "insufficient balance"
	case "40808":
		return "size or price precision is invalid"
	case "40917", "45122":
		return "trigger price is on the wrong side of the current price"
	case "22002":
		return "no position to close"
	case "40099":
		return "API key environment mismatch: demo key requires Demo mode (testnet toggle) and live keys must not enable it"
	case "40404":
		return "endpoint not found: the Bitget API path does not exist (API change or unsupported environment)"
	}
	return ""
}

// bitgetErrMsgContains reports whether err is a Bitget API error whose raw msg contains any of subs.
func bitgetErrMsgContains(err error, subs ...string) bool {
	var apiErr *BitgetAPIError
	msg := ""
	if errors.As(err, &apiErr) {
		msg = apiErr.Msg
	} else if err != nil {
		msg = err.Error()
	}
	msg = strings.ToLower(msg)
	for _, s := range subs {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Position modes. The exchange setting (bitget_position_mode), AutoTraderConfig and the API use
// "hedge" / "one_way"; Bitget's own API vocabulary is "hedge_mode" / "one_way_mode".
const (
	// BitgetPositionModeHedge keeps independent LONG and SHORT positions per symbol (the
	// default, like Binance and OKX in nofx).
	BitgetPositionModeHedge = store.BitgetPositionModeHedge
	// BitgetPositionModeOneWay keeps one net position per symbol.
	BitgetPositionModeOneWay = store.BitgetPositionModeOneWay

	bitgetPosModeHedge  = "hedge_mode"
	bitgetPosModeOneWay = "one_way_mode"

	// bitgetModeProbeSymbol is the symbol used to read the account's posMode (the endpoint needs one).
	bitgetModeProbeSymbol = "BTCUSDT"
	// bitgetErrPosModeMismatch: the order format (tradeSide / reduceOnly) does not match the account mode.
	bitgetErrPosModeMismatch = "40774"
)

// NormalizeBitgetPositionMode maps a user-facing / API value to BitgetPositionModeHedge or
// BitgetPositionModeOneWay. Empty and unknown values mean the default, hedge.
func NormalizeBitgetPositionMode(s string) string { return store.NormalizeBitgetPositionMode(s) }

// IsValidBitgetPositionModeSetting reports whether s is an accepted value for the exchange
// setting (empty = default hedge).
func IsValidBitgetPositionModeSetting(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", BitgetPositionModeHedge, BitgetPositionModeOneWay:
		return true
	}
	return false
}

// bitgetAPIPosMode converts the setting vocabulary to Bitget's posMode value.
func bitgetAPIPosMode(setting string) string {
	if NormalizeBitgetPositionMode(setting) == BitgetPositionModeOneWay {
		return bitgetPosModeOneWay
	}
	return bitgetPosModeHedge
}

// bitgetSettingPosMode converts Bitget's posMode value to the setting vocabulary ("" if unknown).
func bitgetSettingPosMode(apiMode string) string {
	switch apiMode {
	case bitgetPosModeHedge:
		return BitgetPositionModeHedge
	case bitgetPosModeOneWay:
		return BitgetPositionModeOneWay
	}
	return ""
}

// BitgetOption customizes NewBitgetTraderWithOptions.
type BitgetOption func(*BitgetTrader)

// WithBitgetPositionMode sets the position mode the trader tries to put the account in
// (BitgetPositionModeHedge, the default, or BitgetPositionModeOneWay). When the account cannot
// be switched (open positions / orders) the trader operates in the mode it detects.
func WithBitgetPositionMode(mode string) BitgetOption {
	return func(t *BitgetTrader) { t.targetPosMode = bitgetAPIPosMode(mode) }
}

// NewBitgetTrader creates a Bitget (live) trader
func NewBitgetTrader(apiKey, secretKey, passphrase string) *BitgetTrader {
	return NewBitgetTraderWithOptions(apiKey, secretKey, passphrase, false)
}

// NewBitgetTraderWithOptions creates a Bitget trader; demo=true enables the demo (paptrading)
// environment. Options (e.g. WithBitgetPositionMode) are applied before the account's position
// mode is detected / switched.
func NewBitgetTraderWithOptions(apiKey, secretKey, passphrase string, demo bool, opts ...BitgetOption) *BitgetTrader {
	return newBitgetTraderAt(bitgetDefaultBaseURL, apiKey, secretKey, passphrase, demo, opts...)
}

// newBitgetTraderAt is NewBitgetTraderWithOptions against an explicit REST endpoint (tests
// point it at httptest, so the construction-time mode detection never reaches the network).
func newBitgetTraderAt(baseURL, apiKey, secretKey, passphrase string, demo bool, opts ...BitgetOption) *BitgetTrader {
	trader := newBitgetTraderCore(apiKey, secretKey, passphrase, demo)
	trader.baseURL = baseURL
	for _, opt := range opts {
		if opt != nil {
			opt(trader)
		}
	}

	// Detect the account's position mode and switch it to the configured one when possible
	trader.ensurePositionMode()

	if demo {
		logger.Infof("🟢 [Bitget] Trader initialized (DEMO / paptrading, position mode %s)", trader.PositionMode())
	} else {
		logger.Infof("🟢 [Bitget] Trader initialized (position mode %s)", trader.PositionMode())
	}

	return trader
}

// newBitgetTraderCore builds the struct without any network call.
func newBitgetTraderCore(apiKey, secretKey, passphrase string, demo bool) *BitgetTrader {
	return &BitgetTrader{
		apiKey:     apiKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		baseURL:    bitgetDefaultBaseURL,
		demo:       demo,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: http.DefaultTransport,
		},
		cacheDuration:     15 * time.Second,
		contractsCache:    make(map[string]*bitgetCachedContract),
		marginModes:       make(map[string]string),
		orderPollAttempts: 3,
		orderPollInterval: 300 * time.Millisecond,
		targetPosMode:     bitgetPosModeHedge,
	}
}

// PositionMode returns the position mode the account is operated in, BitgetPositionModeHedge or
// BitgetPositionModeOneWay, or "" while it is still unknown (detection failed; the next order
// then uses the configured mode and self-corrects on a 40774 rejection).
func (t *BitgetTrader) PositionMode() string {
	t.posModeMu.RLock()
	defer t.posModeMu.RUnlock()
	return bitgetSettingPosMode(t.posMode)
}

// TargetPositionMode returns the configured position mode (BitgetPositionModeHedge / OneWay).
func (t *BitgetTrader) TargetPositionMode() string {
	t.posModeMu.RLock()
	defer t.posModeMu.RUnlock()
	return bitgetSettingPosMode(t.targetPosMode)
}

// IsHedgeMode reports whether orders are currently sent in hedge mode.
func (t *BitgetTrader) IsHedgeMode() bool {
	return t.orderPosMode() == bitgetPosModeHedge
}

// orderPosMode is the API-vocabulary mode orders are built for: the account's mode when
// known, else the configured one.
func (t *BitgetTrader) orderPosMode() string {
	t.posModeMu.RLock()
	defer t.posModeMu.RUnlock()
	if t.posMode != "" {
		return t.posMode
	}
	if t.targetPosMode != "" {
		return t.targetPosMode
	}
	return bitgetPosModeHedge
}

func (t *BitgetTrader) setPosMode(apiMode string) {
	t.posModeMu.Lock()
	t.posMode = apiMode
	t.posModeMu.Unlock()
}

// detectPositionMode reads the account's current posMode (hedge_mode / one_way_mode).
func (t *BitgetTrader) detectPositionMode() (string, error) {
	data, err := t.doRequest("GET", bitgetAccountDetailPath, map[string]interface{}{
		"symbol":      bitgetModeProbeSymbol,
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
	})
	if err != nil {
		return "", fmt.Errorf("failed to detect position mode: %w", err)
	}
	var acc struct {
		PosMode string `json:"posMode"`
	}
	if err := json.Unmarshal(data, &acc); err != nil {
		return "", fmt.Errorf("failed to parse account info: %w", err)
	}
	if bitgetSettingPosMode(acc.PosMode) == "" {
		return "", fmt.Errorf("unexpected posMode %q in account info", acc.PosMode)
	}
	return acc.PosMode, nil
}

// switchPositionMode asks Bitget to put the account into apiMode. Bitget refuses while
// positions or orders (incl. TP/SL plan orders) exist. "Already in this mode" counts as success.
func (t *BitgetTrader) switchPositionMode(apiMode string) error {
	body := map[string]interface{}{
		"productType": bitgetProductType,
		"posMode":     apiMode,
	}
	if _, err := t.doRequest("POST", bitgetPositionModePath, body); err != nil {
		if bitgetErrMsgContains(err, "same", "already") {
			return nil
		}
		return err
	}
	return nil
}

// ensurePositionMode detects the account's position mode, switches it to the configured one
// when they differ and records the effective mode. If the switch is refused (positions or
// orders exist) it warns and operates in the detected mode; if the mode cannot be determined
// at all it stays unknown and the first order self-corrects on a 40774 rejection.
func (t *BitgetTrader) ensurePositionMode() {
	t.posModeMu.RLock()
	target := t.targetPosMode
	t.posModeMu.RUnlock()

	detected, derr := t.detectPositionMode()
	if derr == nil && detected == target {
		t.setPosMode(detected)
		logger.Infof("  ✓ Bitget account is already in %s position mode", detected)
		return
	}
	if derr != nil {
		logger.Warnf("⚠️ [Bitget] cannot detect the account position mode: %v (trying to switch to %s)", derr, target)
	}

	serr := t.switchPositionMode(target)
	if serr == nil {
		t.setPosMode(target)
		logger.Infof("  ✓ Bitget account switched to %s position mode", target)
		return
	}

	if derr != nil {
		detected, derr = t.detectPositionMode()
	}
	if derr != nil {
		logger.Warnf("⚠️ [Bitget] cannot switch to %s position mode (%v) and the current mode is unknown; orders will use %s and adapt if Bitget rejects them (code %s)",
			target, serr, target, bitgetErrPosModeMismatch)
		return
	}
	t.setPosMode(detected)
	logger.Warnf("⚠️ [Bitget] cannot switch the account to %s position mode (%v). Bitget only allows a switch while there are no open positions and no pending/TP-SL orders. "+
		"Operating in the detected %s mode; close all positions and orders and restart the trader to apply %s, or set the exchange's Bitget position mode to match.",
		target, serr, detected, target)
}

// isBitgetPosModeMismatch reports whether err is Bitget's "order does not match the account's
// position mode" rejection (40774).
func isBitgetPosModeMismatch(err error) bool {
	var apiErr *BitgetAPIError
	return errors.As(err, &apiErr) && apiErr.Code == bitgetErrPosModeMismatch
}

// isBitgetHoldSideMismatch reports whether err is place-tpsl-order's rejection of a holdSide in the
// wrong vocabulary for the account's position mode. Verified on Bitget Demo: a hedge account
// answers holdSide=buy|sell with 43011 "... delegateType is error", a one-way account answers
// holdSide=long|short with 43011 "... holdSide error" (NOT 40774, which only order placement uses).
func isBitgetHoldSideMismatch(err error) bool {
	var apiErr *BitgetAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "43011" {
		return false
	}
	msg := strings.ToLower(apiErr.Msg)
	return strings.Contains(msg, "holdside") || strings.Contains(msg, "delegatetype")
}

// refreshPositionModeAfterMismatch is called after an order (or TP/SL) built for usedMode was rejected
// as a position-mode mismatch (40774 / holdSide 43011): it re-detects the account's mode (if that fails, the other mode must be the
// right one) and records it. It returns the mode to retry with, or ok=false when the mode
// did not change (the rejection has another cause).
func (t *BitgetTrader) refreshPositionModeAfterMismatch(usedMode string) (mode string, ok bool) {
	mode, err := t.detectPositionMode()
	if err != nil {
		mode = bitgetPosModeHedge
		if usedMode == bitgetPosModeHedge {
			mode = bitgetPosModeOneWay
		}
		logger.Warnf("⚠️ [Bitget] order rejected with %s but the account position mode cannot be re-detected (%v): assuming %s",
			bitgetErrPosModeMismatch, err, mode)
	}
	t.setPosMode(mode)
	if mode == usedMode {
		return mode, false
	}
	logger.Warnf("⚠️ [Bitget] account position mode is %s, not %s: refreshed and retrying once", mode, usedMode)
	return mode, true
}

// sign generates Bitget API signature
func (t *BitgetTrader) sign(timestamp, method, requestPath, body string) string {
	// Signature = BASE64(HMAC_SHA256(timestamp + method + requestPath + body, secretKey))
	preHash := timestamp + method + requestPath + body
	h := hmac.New(sha256.New, []byte(t.secretKey))
	h.Write([]byte(preHash))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// doRequest executes HTTP request
func (t *BitgetTrader) doRequest(method, path string, body interface{}) ([]byte, error) {
	var bodyBytes []byte
	var err error

	if body != nil {
		if method == "GET" {
			// For GET requests, body is query parameters (sorted for determinism)
			if params, ok := body.(map[string]interface{}); ok && len(params) > 0 {
				keys := make([]string, 0, len(params))
				for k := range params {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(fmt.Sprintf("%v", params[k])))
				}
				path = path + "?" + strings.Join(parts, "&")
			}
		} else {
			bodyBytes, err = json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize request body: %w", err)
			}
		}
	}

	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())

	// Signature includes body for POST, nothing for GET (query is in path)
	signBody := ""
	if method != "GET" && bodyBytes != nil {
		signBody = string(bodyBytes)
	}
	signature := t.sign(timestamp, method, path, signBody)

	req, err := http.NewRequest(method, t.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("ACCESS-KEY", t.apiKey)
	req.Header.Set("ACCESS-SIGN", signature)
	req.Header.Set("ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("ACCESS-PASSPHRASE", t.passphrase)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "en-US")
	if t.demo {
		req.Header.Set("paptrading", "1")
	}
	// Channel code only for order endpoints
	if strings.Contains(path, "/order/") {
		req.Header.Set("X-CHANNEL-API-CODE", "7fygt")
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var bitgetResp BitgetResponse
	if err := json.Unmarshal(respBody, &bitgetResp); err != nil {
		return nil, fmt.Errorf("failed to parse response (http %d): %w, body: %s", resp.StatusCode, err, string(respBody))
	}

	if bitgetResp.Code != "00000" {
		return nil, &BitgetAPIError{Code: bitgetResp.Code, Msg: bitgetResp.Msg}
	}

	return bitgetResp.Data, nil
}

// convertSymbol converts generic symbol to Bitget format
// e.g., BTCUSDT -> BTCUSDT
func (t *BitgetTrader) convertSymbol(symbol string) string {
	// Bitget uses same format as input, just ensure uppercase
	return strings.ToUpper(symbol)
}

// GetBalance gets account balance
func (t *BitgetTrader) GetBalance() (map[string]interface{}, error) {
	// Check cache
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		t.balanceCacheMutex.RUnlock()
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
	}

	data, err := t.doRequest("GET", bitgetAccountPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get account balance: %w", err)
	}

	var accounts []struct {
		MarginCoin    string `json:"marginCoin"`
		Available     string `json:"available"`     // Available balance
		AccountEquity string `json:"accountEquity"` // Total equity
		UsdtEquity    string `json:"usdtEquity"`    // USDT equity
		UnrealizedPL  string `json:"unrealizedPL"`  // Unrealized P&L
	}

	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("failed to parse balance data: %w, raw: %s", err, string(data))
	}

	var totalEquity, availableBalance, unrealizedPnL float64
	for _, acc := range accounts {
		if acc.MarginCoin == "USDT" {
			totalEquity, _ = strconv.ParseFloat(acc.AccountEquity, 64)
			availableBalance, _ = strconv.ParseFloat(acc.Available, 64)
			unrealizedPnL, _ = strconv.ParseFloat(acc.UnrealizedPL, 64)
			logger.Infof("✓ [Bitget] Balance: equity=%.2f, available=%.2f", totalEquity, availableBalance)
			break
		}
	}

	result := map[string]interface{}{
		"totalWalletBalance":    totalEquity - unrealizedPnL,
		"availableBalance":      availableBalance,
		"totalUnrealizedProfit": unrealizedPnL,
		"total_equity":          totalEquity,
		"totalEquity":           totalEquity,
	}

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// sanitizeBitgetLiquidationPrice parses Bitget's liquidationPrice and maps "not applicable"
// values to 0 (the convention used by the prompt schema: 0 = no liquidation risk).
// Low-leverage cross positions report a huge negative price (verified on Demo:
// "-279781303.08" for a 3x cross long), meaning the position cannot be liquidated by price;
// such a number must never reach the AI prompt or a risk check. Empty, NaN, Inf and
// non-positive values all become 0.
func sanitizeBitgetLiquidationPrice(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	return v
}

// bitgetLiqMaxMarkRatio bounds a believable liquidation price: Bitget reports absurd numbers for
// positions that cannot be liquidated by price (see sanitizeBitgetPositionLiquidation).
const bitgetLiqMaxMarkRatio = 100.0

// bitgetMarginModeIsolated is the marginMode of an isolated position ("crossed" otherwise).
const bitgetMarginModeIsolated = "isolated"

// sanitizeBitgetPositionLiquidation is sanitizeBitgetLiquidationPrice plus plausibility checks
// against the mark price (0 = not applicable = no liquidation risk, the prompt-schema convention).
//
// Every position: nothing beyond 100x or below 1/100 of the mark is actionable. Verified on Bitget
// Demo: with a long and a short of the same symbol open in hedge mode on a cross account BOTH legs
// report liquidationPrice "60549679693.55" (about 725000 x the mark) - their risks offset, so there
// is no price liquidation.
//
// Isolated positions only: a long can only be liquidated BELOW the mark and a short ABOVE it, so
// the other side is bogus. Cross positions are NOT direction-checked: their liquidation price is
// the account-wide one, so on a net-long cross account the short leg legitimately reports the same
// below-mark price as the long leg (and the mirror image for a net-short account), and both legs
// must show it.
func sanitizeBitgetPositionLiquidation(holdSide, marginMode string, mark float64, raw string) float64 {
	liq := sanitizeBitgetLiquidationPrice(raw)
	if liq <= 0 || mark <= 0 {
		return liq
	}
	if liq > mark*bitgetLiqMaxMarkRatio || liq < mark/bitgetLiqMaxMarkRatio {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(marginMode), bitgetMarginModeIsolated) {
		if strings.EqualFold(holdSide, "short") {
			if liq <= mark {
				return 0
			}
		} else if liq >= mark {
			return 0
		}
	}
	return liq
}

// GetPositions gets all positions
func (t *BitgetTrader) GetPositions() ([]map[string]interface{}, error) {
	// Check cache
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		t.positionsCacheMutex.RUnlock()
		return t.cachedPositions, nil
	}
	t.positionsCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
	}

	data, err := t.doRequest("GET", bitgetPositionPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var positions []struct {
		Symbol           string `json:"symbol"`
		HoldSide         string `json:"holdSide"`         // long, short
		OpenPriceAvg     string `json:"openPriceAvg"`     // Average entry price
		MarkPrice        string `json:"markPrice"`        // Mark price
		Total            string `json:"total"`            // Total position size
		Available        string `json:"available"`        // Available to close
		UnrealizedPL     string `json:"unrealizedPL"`     // Unrealized P&L
		Leverage         string `json:"leverage"`         // Leverage
		LiquidationPrice string `json:"liquidationPrice"` // Liquidation price
		MarginMode       string `json:"marginMode"`       // crossed, isolated
		MarginSize       string `json:"marginSize"`       // Position margin
		CTime            string `json:"cTime"`            // Create time
		UTime            string `json:"uTime"`            // Update time
	}

	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("failed to parse position data: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		total, _ := strconv.ParseFloat(pos.Total, 64)
		if total == 0 {
			continue
		}

		entryPrice, _ := strconv.ParseFloat(pos.OpenPriceAvg, 64)
		markPrice, _ := strconv.ParseFloat(pos.MarkPrice, 64)
		unrealizedPnL, _ := strconv.ParseFloat(pos.UnrealizedPL, 64)
		leverage, _ := strconv.ParseFloat(pos.Leverage, 64)
		liqPrice := sanitizeBitgetPositionLiquidation(pos.HoldSide, pos.MarginMode, markPrice, pos.LiquidationPrice)
		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)

		// Normalize side
		side := "long"
		if pos.HoldSide == "short" {
			side = "short"
		}

		posMap := map[string]interface{}{
			"symbol":           pos.Symbol,
			"positionAmt":      total,
			"entryPrice":       entryPrice,
			"markPrice":        markPrice,
			"unRealizedProfit": unrealizedPnL,
			"leverage":         leverage,
			"liquidationPrice": liqPrice,
			"side":             side,
			"createdTime":      cTime,
			"updatedTime":      uTime,
		}
		result = append(result, posMap)
	}

	// Update cache
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return result, nil
}

// getContract returns cached contract info (per-symbol TTL), fetching on miss.
func (t *BitgetTrader) getContract(symbol string) (*BitgetContract, error) {
	symbol = t.convertSymbol(symbol)

	t.contractsCacheMutex.RLock()
	if c, ok := t.contractsCache[symbol]; ok && time.Since(c.fetchedAt) < bitgetContractCacheTTL {
		t.contractsCacheMutex.RUnlock()
		return c.contract, nil
	}
	t.contractsCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"symbol":      symbol,
	}

	data, err := t.doRequest("GET", bitgetContractsPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get contract info for %s: %w", symbol, err)
	}

	var contracts []struct {
		Symbol         string `json:"symbol"`
		BaseCoin       string `json:"baseCoin"`
		QuoteCoin      string `json:"quoteCoin"`
		MinTradeNum    string `json:"minTradeNum"`
		MaxOrderQty    string `json:"maxOrderQty"`
		SizeMultiplier string `json:"sizeMultiplier"`
		PricePlace     string `json:"pricePlace"`
		PriceEndStep   string `json:"priceEndStep"`
		VolumePlace    string `json:"volumePlace"`
		MinTradeUSDT   string `json:"minTradeUSDT"`
		MaxLever       string `json:"maxLever"`
		SymbolStatus   string `json:"symbolStatus"`
		IsRwa          string `json:"isRwa"`
		TakerFeeRate   string `json:"takerFeeRate"`
		MakerFeeRate   string `json:"makerFeeRate"`
		FundInterval   string `json:"fundInterval"`
	}

	if err := json.Unmarshal(data, &contracts); err != nil {
		return nil, fmt.Errorf("failed to parse contract info: %w", err)
	}

	for _, c := range contracts {
		if !strings.EqualFold(c.Symbol, symbol) {
			continue
		}
		contract := &BitgetContract{
			Symbol:       c.Symbol,
			BaseCoin:     c.BaseCoin,
			QuoteCoin:    c.QuoteCoin,
			SymbolStatus: c.SymbolStatus,
			IsRwa:        strings.EqualFold(c.IsRwa, "YES"),
		}
		contract.MinTradeNum, _ = strconv.ParseFloat(c.MinTradeNum, 64)
		contract.MaxTradeNum, _ = strconv.ParseFloat(c.MaxOrderQty, 64)
		contract.SizeMultiplier, _ = strconv.ParseFloat(c.SizeMultiplier, 64)
		contract.PricePlace, _ = strconv.Atoi(c.PricePlace)
		contract.PriceEndStep, _ = strconv.ParseFloat(c.PriceEndStep, 64)
		contract.VolumePlace, _ = strconv.Atoi(c.VolumePlace)
		contract.MinTradeUSDT, _ = strconv.ParseFloat(c.MinTradeUSDT, 64)
		contract.MaxLever, _ = strconv.Atoi(c.MaxLever)
		contract.TakerFeeRate, _ = strconv.ParseFloat(c.TakerFeeRate, 64)
		contract.MakerFeeRate, _ = strconv.ParseFloat(c.MakerFeeRate, 64)
		contract.FundInterval, _ = strconv.Atoi(c.FundInterval)
		if contract.SizeMultiplier <= 0 {
			contract.SizeMultiplier = math.Pow(10, -float64(contract.VolumePlace))
		}

		t.contractsCacheMutex.Lock()
		t.contractsCache[symbol] = &bitgetCachedContract{contract: contract, fetchedAt: time.Now()}
		t.contractsCacheMutex.Unlock()

		return contract, nil
	}

	return nil, fmt.Errorf("contract info not found: %s", symbol)
}

// GetContractInfo returns Bitget contract metadata (precision, min size, max leverage, RWA flag...).
func (t *BitgetTrader) GetContractInfo(symbol string) (*BitgetContract, error) {
	return t.getContract(symbol)
}

// marginModeFor returns the margin mode recorded for symbol (default crossed).
func (t *BitgetTrader) marginModeFor(symbol string) string {
	t.marginModeMutex.RLock()
	defer t.marginModeMutex.RUnlock()
	if m, ok := t.marginModes[symbol]; ok {
		return m
	}
	return "crossed"
}

// SetMarginMode sets margin mode
func (t *BitgetTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	symbol = t.convertSymbol(symbol)

	marginMode := "isolated"
	if isCrossMargin {
		marginMode = "crossed"
	}

	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
		"marginMode":  marginMode,
	}

	_, err := t.doRequest("POST", bitgetMarginModePath, body)
	if err != nil {
		if bitgetErrMsgContains(err, "same", "already") {
			t.setMarginModeLocal(symbol, marginMode)
			return nil
		}
		if bitgetErrMsgContains(err, "position") {
			logger.Infof("  ⚠️ %s has positions, cannot change margin mode (keeping %s)", symbol, t.marginModeFor(symbol))
			return nil
		}
		return err
	}

	t.setMarginModeLocal(symbol, marginMode)
	logger.Infof("  ✓ %s margin mode set to %s", symbol, marginMode)
	return nil
}

func (t *BitgetTrader) setMarginModeLocal(symbol, mode string) {
	t.marginModeMutex.Lock()
	t.marginModes[symbol] = mode
	t.marginModeMutex.Unlock()
}

// SetLeverage sets leverage
func (t *BitgetTrader) SetLeverage(symbol string, leverage int) error {
	symbol = t.convertSymbol(symbol)

	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
		"leverage":    fmt.Sprintf("%d", leverage),
	}

	_, err := t.doRequest("POST", bitgetLeveragePath, body)
	if err != nil {
		if bitgetErrMsgContains(err, "same") {
			return nil
		}
		logger.Infof("  ⚠️ Failed to set %s leverage: %v", symbol, err)
		return fmt.Errorf("failed to set leverage for %s: %w", symbol, err)
	}

	logger.Infof("  ✓ %s leverage set to %dx", symbol, leverage)
	return nil
}

// bitgetOrderSide returns the "side" of a place-order request for a position direction
// ("long"/"short") in the given mode.
//
// Hedge mode: side is the POSITION direction and tradeSide says open|close (open long =
// buy+open, close long = buy+close, open short = sell+open, close short = sell+close; verified
// on Bitget Demo). One-way mode: side is the real order direction (open long = buy, close long =
// sell, open short = sell, close short = buy) and closes are reduceOnly.
func bitgetOrderSide(mode, direction string, closing bool) string {
	long := direction == "long"
	if mode != bitgetPosModeHedge && closing {
		long = !long
	}
	if long {
		return "buy"
	}
	return "sell"
}

// marketOrderBody builds the place-order body of a market order for the given account mode.
//
// Every close is reduceOnly, in both modes. In one-way mode it is what makes the order a close.
// In hedge mode Bitget accepts and ignores reduceOnly (verified on Demo: the position is closed
// by tradeSide=close on the position `side`), so it is a pure fail-safe: if the trader's belief
// about the mode is wrong, the mis-routed close (a one-way account rejects tradeSide with 40774; if
// it did not, side=<position direction> would be an order that adds to the position) is rejected
// as a reduce-only violation instead of adding exposure.
func (t *BitgetTrader) marketOrderBody(symbol, direction string, closing bool, qtyStr, mode string) map[string]interface{} {
	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginMode":  t.marginModeFor(symbol),
		"marginCoin":  "USDT",
		"side":        bitgetOrderSide(mode, direction, closing),
		"orderType":   "market",
		"size":        qtyStr,
		"clientOid":   genBitgetClientOid(),
	}
	if mode == bitgetPosModeHedge {
		// hedge mode: tradeSide is mandatory (open|close) and ties the order to the position of `side`
		if closing {
			body["tradeSide"] = "close"
		} else {
			body["tradeSide"] = "open"
		}
	}
	// one-way mode must NOT send tradeSide (40774); a close is a reduce-only order there
	if closing {
		body["reduceOnly"] = "YES"
	}
	return body
}

// placeMarketOrder places a market order for the position direction ("long"/"short") and
// confirms the fill via order/detail. closing=false opens/adds, closing=true reduces. The
// request format depends on the account's position mode (see bitgetOrderSide); if Bitget
// rejects it with 40774 (mode mismatch) the mode is re-detected and the order is retried once.
//
// The retry cannot double-submit: it happens only after a definitive 40774 rejection (the order was
// refused by validation and never reached the book), doRequest itself never retries, and any other
// error (timeout, 5xx, insufficient balance, ...) is returned without a second attempt. The retry
// body is rebuilt for the re-detected mode (new clientOid; the rejected one never created an order).
func (t *BitgetTrader) placeMarketOrder(symbol, direction string, closing bool, quantity float64, label string) (map[string]interface{}, error) {
	var qtyStr string
	var err error
	if closing {
		// Closing must never be blocked by local min-size/notional checks: a residual
		// position below the minimum would otherwise become impossible to close.
		qtyStr, err = t.formatQuantityStep(symbol, quantity)
	} else {
		qtyStr, err = t.FormatQuantity(symbol, quantity)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}

	mode := t.orderPosMode()
	var data []byte
	for attempt := 0; ; attempt++ {
		body := t.marketOrderBody(symbol, direction, closing, qtyStr, mode)
		logger.Infof("  📊 Bitget %s: symbol=%s, qty=%s, mode=%s", label, symbol, qtyStr, mode)
		data, err = t.doRequest("POST", bitgetOrderPath, body)
		if err == nil {
			break
		}
		if attempt == 0 && isBitgetPosModeMismatch(err) {
			if newMode, ok := t.refreshPositionModeAfterMismatch(mode); ok {
				mode = newMode
				continue
			}
		}
		return nil, fmt.Errorf("failed to %s: %w", label, err)
	}

	var order struct {
		OrderId   string `json:"orderId"`
		ClientOid string `json:"clientOid"`
	}
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	t.clearCache()

	result := t.confirmOrder(symbol, order.OrderId)
	logger.Infof("✓ Bitget %s submitted: %s orderId=%s status=%v avg=%v", label, symbol, order.OrderId, result["status"], result["avgPrice"])
	return result, nil
}

// confirmOrder polls order/detail (default 3 x 300ms) until the order is filled or canceled.
// If detail is unavailable the order is reported as NEW.
func (t *BitgetTrader) confirmOrder(symbol, orderID string) map[string]interface{} {
	result := map[string]interface{}{
		"orderId":     orderID,
		"symbol":      symbol,
		"status":      "NEW",
		"avgPrice":    0.0,
		"executedQty": 0.0,
	}

	attempts := t.orderPollAttempts
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(t.orderPollInterval)
		}
		st, err := t.GetOrderStatus(symbol, orderID)
		if err != nil {
			logger.Infof("  ⚠️ Bitget order detail query failed (attempt %d/%d): %v", i+1, attempts, err)
			continue
		}
		for _, k := range []string{"status", "avgPrice", "executedQty", "commission"} {
			result[k] = st[k]
		}
		if s, _ := st["status"].(string); s == "FILLED" || s == "CANCELED" {
			break
		}
	}
	return result
}

// OpenLong opens long position
func (t *BitgetTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, fmt.Errorf("aborting open long: %w", err)
	}
	return t.placeMarketOrder(symbol, "long", false, quantity, "open long")
}

// OpenShort opens short position
func (t *BitgetTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, fmt.Errorf("aborting open short: %w", err)
	}
	return t.placeMarketOrder(symbol, "short", false, quantity, "open short")
}

// closeQuantity resolves quantity==0 to the current position size.
func (t *BitgetTrader) closeQuantity(symbol, side string, quantity float64) (float64, error) {
	if quantity < 0 {
		quantity = -quantity
	}
	if quantity != 0 {
		return quantity, nil
	}
	positions, err := t.GetPositions()
	if err != nil {
		return 0, err
	}
	for _, pos := range positions {
		if s, _ := pos["symbol"].(string); s == symbol {
			if ps, _ := pos["side"].(string); ps == side {
				if amt, ok := pos["positionAmt"].(float64); ok && amt != 0 {
					return math.Abs(amt), nil
				}
			}
		}
	}
	return 0, fmt.Errorf("%s position not found for %s", side, symbol)
}

// CloseLong closes long position
func (t *BitgetTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)
	qty, err := t.closeQuantity(symbol, "long", quantity)
	if err != nil {
		return nil, err
	}
	return t.placeMarketOrder(symbol, "long", true, qty, "close long")
}

// CloseShort closes short position
func (t *BitgetTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)
	qty, err := t.closeQuantity(symbol, "short", quantity)
	if err != nil {
		return nil, err
	}
	return t.placeMarketOrder(symbol, "short", true, qty, "close short")
}

// GetMarketPrice gets market price
func (t *BitgetTrader) GetMarketPrice(symbol string) (float64, error) {
	symbol = t.convertSymbol(symbol)

	params := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
	}

	data, err := t.doRequest("GET", bitgetTickerPath, params)
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	var tickers []struct {
		LastPr string `json:"lastPr"`
	}

	if err := json.Unmarshal(data, &tickers); err != nil {
		return 0, err
	}

	if len(tickers) == 0 {
		return 0, fmt.Errorf("no price data received")
	}

	price, err := strconv.ParseFloat(tickers[0].LastPr, 64)
	if err != nil {
		return 0, err
	}

	return price, nil
}

// bitgetPlanOrder is one entry of orders-plan-pending.entrustedList
type bitgetPlanOrder struct {
	OrderId      string `json:"orderId"`
	PlanType     string `json:"planType"`
	TriggerPrice string `json:"triggerPrice"`
	Side         string `json:"side"`
	PosSide      string `json:"posSide"`
	HoldSide     string `json:"holdSide"`
	Size         string `json:"size"`
	Symbol       string `json:"symbol"`
	CTime        string `json:"cTime"`
}

type bitgetTPSLKind int

const (
	bitgetKindSL bitgetTPSLKind = 1 << iota
	bitgetKindTP
	bitgetKindAll = bitgetKindSL | bitgetKindTP
)

func bitgetPlanKind(planType string) bitgetTPSLKind {
	switch planType {
	case "pos_loss", "loss_plan":
		return bitgetKindSL
	case "pos_profit", "profit_plan":
		return bitgetKindTP
	}
	return 0
}

// bitgetDirection normalizes buy/long -> "long", sell/short -> "short", otherwise "".
func bitgetDirection(s string) string {
	switch strings.ToLower(s) {
	case "buy", "long":
		return "long"
	case "sell", "short":
		return "short"
	}
	return ""
}

// planOrderDirection returns the direction ("long"/"short") of the POSITION a TP/SL plan order
// protects, or "" when it cannot be determined.
//
// Hedge mode reports posSide=long|short, which names the position directly. One-way mode
// reports posSide="net" and side = the CLOSING side of the plan (verified on Bitget Demo:
// a long's pos_loss/pos_profit has side "sell", a short's has side "buy"), so the position
// direction is the opposite of side. Legacy holdSide (the place-tpsl-order request vocabulary:
// buy = long position, sell = short position) is honored when present.
func planOrderDirection(o bitgetPlanOrder) string {
	switch strings.ToLower(o.PosSide) {
	case "long":
		return "long"
	case "short":
		return "short"
	}
	if d := bitgetDirection(o.HoldSide); d != "" {
		return d
	}
	// one-way ("net" or missing posSide): side is the closing side
	switch strings.ToLower(o.Side) {
	case "sell":
		return "long"
	case "buy":
		return "short"
	}
	return ""
}

// listPlanOrders queries pending TP/SL (profit_loss) plan orders.
func (t *BitgetTrader) listPlanOrders(symbol string) ([]bitgetPlanOrder, error) {
	params := map[string]interface{}{
		"productType": bitgetProductType,
		"planType":    "profit_loss",
	}
	if symbol != "" {
		params["symbol"] = symbol
	}
	data, err := t.doRequest("GET", bitgetPlanPendingPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to list plan orders: %w", err)
	}
	var resp struct {
		EntrustedList []bitgetPlanOrder `json:"entrustedList"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse plan orders: %w", err)
	}
	return resp.EntrustedList, nil
}

// cancelPlanOrders cancels TP/SL plan orders and reports any failure.
//
// cancel-plan-order must be called with the plan order's OWN planType (pos_loss / pos_profit /
// loss_plan / profit_plan), not with the listing's umbrella type "profit_loss": verified on
// Bitget Demo (one-way and hedge), planType "profit_loss" + orderIdList answers
// {"successList":[],"failureList":[]} (code 00000) and cancels NOTHING, while planType
// "pos_loss" + the same orderIdList cancels the stop loss. The orders are therefore grouped
// by planType and every id must come back in successList; a silent no-op is reported as an error.
func (t *BitgetTrader) cancelPlanOrders(symbol string, orders []bitgetPlanOrder) error {
	if len(orders) == 0 {
		return nil
	}
	byType := map[string][]string{}
	var types []string
	for _, o := range orders {
		if _, ok := byType[o.PlanType]; !ok {
			types = append(types, o.PlanType)
		}
		byType[o.PlanType] = append(byType[o.PlanType], o.OrderId)
	}
	sort.Strings(types)

	var errs []error
	for _, planType := range types {
		ids := byType[planType]
		list := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			list = append(list, map[string]string{"orderId": id})
		}
		body := map[string]interface{}{
			"productType": bitgetProductType,
			"marginCoin":  "USDT",
			"symbol":      symbol,
			"planType":    planType,
			"orderIdList": list,
		}
		data, err := t.doRequest("POST", bitgetCancelPlanPath, body)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to cancel %s plan orders %v: %w", planType, ids, err))
			continue
		}
		var resp struct {
			SuccessList []struct {
				OrderId string `json:"orderId"`
			} `json:"successList"`
			FailureList []struct {
				OrderId  string `json:"orderId"`
				ErrorMsg string `json:"errorMsg"`
			} `json:"failureList"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			continue // unparseable detail: the call itself succeeded
		}
		if len(resp.FailureList) > 0 {
			var msgs []string
			for _, f := range resp.FailureList {
				msgs = append(msgs, fmt.Sprintf("%s: %s", f.OrderId, f.ErrorMsg))
			}
			errs = append(errs, fmt.Errorf("failed to cancel plan orders: %s", strings.Join(msgs, "; ")))
			continue
		}
		done := make(map[string]bool, len(resp.SuccessList))
		for _, s := range resp.SuccessList {
			done[s.OrderId] = true
		}
		var missing []string
		for _, id := range ids {
			if !done[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("cancel %s plan orders %v: not confirmed by the exchange (successList %d of %d)", planType, missing, len(resp.SuccessList), len(ids)))
		}
	}
	return errors.Join(errs...)
}

// matchingPlanOrders returns the pending TP/SL plan orders of the given kinds for symbol.
// direction ("long"/"short"/"") restricts the result to one position side when the
// exchange reports it.
func (t *BitgetTrader) matchingPlanOrders(symbol string, kinds bitgetTPSLKind, direction string) ([]bitgetPlanOrder, error) {
	orders, err := t.listPlanOrders(symbol)
	if err != nil {
		return nil, err
	}
	var out []bitgetPlanOrder
	for _, o := range orders {
		if bitgetPlanKind(o.PlanType)&kinds == 0 {
			continue
		}
		if o.Symbol != "" && !strings.EqualFold(o.Symbol, symbol) {
			continue
		}
		if d := planOrderDirection(o); direction != "" && d != "" && d != direction {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// cancelTPSL cancels pending TP/SL orders of the given kinds. direction ("long"/"short"/"")
// restricts the cancel to one position side when the exchange reports it.
func (t *BitgetTrader) cancelTPSL(symbol string, kinds bitgetTPSLKind, direction string) error {
	symbol = t.convertSymbol(symbol)
	orders, err := t.matchingPlanOrders(symbol, kinds, direction)
	if err != nil {
		return err
	}
	return t.cancelPlanOrders(symbol, orders)
}

// roundBitgetPrice rounds to pricePlace decimals and to a multiple of priceEndStep.
func roundBitgetPrice(c *BitgetContract, price float64) string {
	tick := c.PriceTick()
	v := math.Round(price/tick) * tick
	return strconv.FormatFloat(v, 'f', c.PricePlace, 64)
}

// bitgetHoldSide is the holdSide vocabulary of place-tpsl-order for a position direction
// ("long"/"short"): hedge mode names the position (long|short), one-way mode uses the
// order-side vocabulary (buy = long position, sell = short position).
func bitgetHoldSide(mode, direction string) string {
	if mode == bitgetPosModeHedge {
		return direction
	}
	if direction == "short" {
		return "sell"
	}
	return "buy"
}

// placeTPSL places a whole-position stop loss / take profit via place-tpsl-order.
//
// place-tpsl-order is an UPSERT for pos_loss / pos_profit (verified on Bitget Demo, one-way and
// hedge): placing one for a position (symbol + holdSide) that already has one of the same type
// replaces it in place (same orderId, new trigger), and a rejected placement (e.g. 45122
// "stop loss price must be above mark price") leaves the existing order untouched. In hedge
// mode the long and the short position of a symbol are protected independently. Cancelling
// first would only open an unprotected window, so this deliberately does NOT cancel anything:
// it places the new trigger and propagates the exchange error as-is. Use the Cancel* methods
// to remove orders.
func (t *BitgetTrader) placeTPSL(symbol, positionSide string, price float64, planType, label string) error {
	symbol = t.convertSymbol(symbol)

	direction := "long"
	if strings.ToUpper(positionSide) == "SHORT" {
		direction = "short"
	}

	contract, err := t.getContract(symbol)
	if err != nil {
		return fmt.Errorf("failed to set %s: %w", label, err)
	}
	trigger := roundBitgetPrice(contract, price)

	mode := t.orderPosMode()
	for attempt := 0; ; attempt++ {
		body := map[string]interface{}{
			"marginCoin":   "USDT",
			"productType":  bitgetProductType,
			"symbol":       symbol,
			"planType":     planType,
			"triggerPrice": trigger,
			"triggerType":  "mark_price",
			"holdSide":     bitgetHoldSide(mode, direction),
			"clientOid":    genBitgetClientOid(),
		}
		_, err = t.doRequest("POST", bitgetPlaceTPSLPath, body)
		if err == nil {
			break
		}
		if attempt == 0 && (isBitgetPosModeMismatch(err) || isBitgetHoldSideMismatch(err)) {
			if newMode, ok := t.refreshPositionModeAfterMismatch(mode); ok {
				mode = newMode
				continue
			}
		}
		return fmt.Errorf("failed to set %s: %w", label, err)
	}
	logger.Infof("  ✓ [Bitget] %s set: %s %s @ %s", label, symbol, direction, trigger)
	return nil
}

// SetStopLoss sets a position stop loss (size is the whole position; quantity is unused)
func (t *BitgetTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return t.placeTPSL(symbol, positionSide, stopPrice, "pos_loss", "stop loss")
}

// SetTakeProfit sets a position take profit (size is the whole position; quantity is unused)
func (t *BitgetTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	return t.placeTPSL(symbol, positionSide, takeProfitPrice, "pos_profit", "take profit")
}

// CancelStopLossOrders cancels the stop loss orders of symbol (both position sides in hedge
// mode; use CancelStopLossOrdersForSide to cancel one side only)
func (t *BitgetTrader) CancelStopLossOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindSL, "")
}

// CancelTakeProfitOrders cancels the take profit orders of symbol (both position sides in
// hedge mode; use CancelTakeProfitOrdersForSide to cancel one side only)
func (t *BitgetTrader) CancelTakeProfitOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindTP, "")
}

// CancelStopOrders cancels stop loss and take profit orders of symbol (both position sides in
// hedge mode; use CancelStopOrdersForSide to cancel one side only)
func (t *BitgetTrader) CancelStopOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindAll, "")
}

// bitgetPositionSideArg normalizes a "LONG"/"SHORT"/"long"/"short" argument to the lower-case
// direction used by the plan-order filters ("" = both sides).
func bitgetPositionSideArg(positionSide string) string {
	return bitgetDirection(positionSide)
}

// CancelStopLossOrdersForSide cancels only the stop loss of one position side ("LONG"/"SHORT";
// "" = both). The other side's protection stays untouched.
func (t *BitgetTrader) CancelStopLossOrdersForSide(symbol, positionSide string) error {
	return t.cancelTPSL(symbol, bitgetKindSL, bitgetPositionSideArg(positionSide))
}

// CancelTakeProfitOrdersForSide cancels only the take profit of one position side.
func (t *BitgetTrader) CancelTakeProfitOrdersForSide(symbol, positionSide string) error {
	return t.cancelTPSL(symbol, bitgetKindTP, bitgetPositionSideArg(positionSide))
}

// CancelStopOrdersForSide cancels the stop loss and take profit of one position side.
func (t *BitgetTrader) CancelStopOrdersForSide(symbol, positionSide string) error {
	return t.cancelTPSL(symbol, bitgetKindAll, bitgetPositionSideArg(positionSide))
}

// CancelAllOrders cancels all pending orders (regular + TP/SL plan orders)
func (t *BitgetTrader) CancelAllOrders(symbol string) error {
	symbol = t.convertSymbol(symbol)
	var errs []error

	data, err := t.doRequest("GET", bitgetPendingPath, map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to list pending orders: %w", err))
	} else {
		var orders struct {
			EntrustedList []struct {
				OrderId string `json:"orderId"`
			} `json:"entrustedList"`
		}
		if err := json.Unmarshal(data, &orders); err != nil {
			errs = append(errs, fmt.Errorf("failed to parse pending orders: %w", err))
		}
		for _, order := range orders.EntrustedList {
			body := map[string]interface{}{
				"symbol":      symbol,
				"productType": bitgetProductType,
				"marginCoin":  "USDT",
				"orderId":     order.OrderId,
			}
			if _, err := t.doRequest("POST", bitgetCancelOrderPath, body); err != nil {
				logger.Infof("  ⚠️ Failed to cancel order %s: %v", order.OrderId, err)
				errs = append(errs, fmt.Errorf("cancel order %s: %w", order.OrderId, err))
			}
		}
	}

	if err := t.cancelTPSL(symbol, bitgetKindAll, ""); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// bitgetFloorQuantity floors quantity to a whole multiple of step (the contract's
// sizeMultiplier). The small epsilon absorbs float noise such as 0.3/0.1 = 2.9999999999999996.
// Shared by the live Bitget trader and the local paper trader.
func bitgetFloorQuantity(quantity, step float64) float64 {
	if step <= 0 {
		return quantity
	}
	return math.Floor(quantity/step+1e-9) * step
}

// formatQuantityStep floors quantity to the contract's size step without min-size checks.
func (t *BitgetTrader) formatQuantityStep(symbol string, quantity float64) (string, error) {
	contract, err := t.getContract(t.convertSymbol(symbol))
	if err != nil {
		return "", err
	}
	qty := bitgetFloorQuantity(quantity, contract.SizeMultiplier)
	if qty <= 0 {
		return "", fmt.Errorf("quantity %.8f for %s rounds to zero (step %v)", quantity, symbol, contract.SizeMultiplier)
	}
	return strconv.FormatFloat(qty, 'f', contract.VolumePlace, 64), nil
}

// FormatQuantity floors quantity to the contract's size step and formats it with volumePlace.
// It errors when the contract is unknown or the order is below the minimum size / notional.
func (t *BitgetTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	symbol = t.convertSymbol(symbol)
	contract, err := t.getContract(symbol)
	if err != nil {
		return "", err
	}

	mult := contract.SizeMultiplier
	qty := bitgetFloorQuantity(quantity, mult)
	if qty < contract.MinTradeNum-1e-12 {
		return "", fmt.Errorf("quantity %.8f for %s is below minimum trade size %v (step %v)", quantity, symbol, contract.MinTradeNum, mult)
	}

	if contract.MinTradeUSDT > 0 {
		if price, perr := t.GetMarketPrice(symbol); perr == nil && price > 0 {
			if qty*price < contract.MinTradeUSDT {
				return "", fmt.Errorf("order value %.4f USDT for %s is below minimum %v USDT", qty*price, symbol, contract.MinTradeUSDT)
			}
		} else if perr != nil {
			logger.Infof("  ⚠️ Bitget min-notional check skipped for %s: %v", symbol, perr)
		}
	}

	return strconv.FormatFloat(qty, 'f', contract.VolumePlace, 64), nil
}

// normalizeBitgetStatus maps Bitget order state to the uppercase convention used by other traders.
func normalizeBitgetStatus(state string) string {
	switch strings.ToLower(state) {
	case "filled", "full_fill", "full-fill":
		return "FILLED"
	case "live", "new", "init":
		return "NEW"
	case "partially_filled", "partially-filled", "partial_fill":
		return "PARTIALLY_FILLED"
	case "canceled", "cancelled":
		return "CANCELED"
	}
	return strings.ToUpper(state)
}

// GetOrderStatus gets order status
func (t *BitgetTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	params := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"orderId":     orderID,
	}

	data, err := t.doRequest("GET", bitgetOrderDetailPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get order status: %w", err)
	}

	var order struct {
		OrderId    string `json:"orderId"`
		State      string `json:"state"`      // live, partially_filled, filled, canceled
		PriceAvg   string `json:"priceAvg"`   // Average fill price
		BaseVolume string `json:"baseVolume"` // Filled quantity
		Fee        string `json:"fee"`        // Fee (negative)
		Side       string `json:"side"`
		OrderType  string `json:"orderType"`
		CTime      string `json:"cTime"`
		UTime      string `json:"uTime"`
	}

	if err := json.Unmarshal(data, &order); err != nil {
		return nil, fmt.Errorf("failed to parse order detail: %w", err)
	}

	avgPrice, _ := strconv.ParseFloat(order.PriceAvg, 64)
	fillQty, _ := strconv.ParseFloat(order.BaseVolume, 64)
	fee, _ := strconv.ParseFloat(order.Fee, 64)
	cTime, _ := strconv.ParseInt(order.CTime, 10, 64)
	uTime, _ := strconv.ParseInt(order.UTime, 10, 64)

	return map[string]interface{}{
		"orderId":     order.OrderId,
		"symbol":      symbol,
		"status":      normalizeBitgetStatus(order.State),
		"avgPrice":    avgPrice,
		"executedQty": fillQty,
		"side":        order.Side,
		"type":        order.OrderType,
		"time":        cTime,
		"updateTime":  uTime,
		"commission":  math.Abs(fee),
	}, nil
}

// inferBitgetCloseType derives the close type from an optional reason field.
func inferBitgetCloseType(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case r == "":
		return "unknown"
	case strings.Contains(r, "liquidat") || strings.Contains(r, "burst"):
		return "liquidation"
	case strings.Contains(r, "profit") || strings.Contains(r, "take"):
		return "take_profit"
	case strings.Contains(r, "loss") || strings.Contains(r, "stop"):
		return "stop_loss"
	}
	return "manual"
}

// GetClosedPnL retrieves closed position PnL records
func (t *BitgetTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"startTime":   fmt.Sprintf("%d", startTime.UnixMilli()),
		"endTime":     fmt.Sprintf("%d", time.Now().UnixMilli()),
		"limit":       fmt.Sprintf("%d", limit),
	}

	data, err := t.doRequest("GET", bitgetHistoryPosPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions history: %w", err)
	}

	var resp struct {
		List []struct {
			PositionId    string `json:"positionId"`
			Symbol        string `json:"symbol"`
			HoldSide      string `json:"holdSide"`
			OpenAvgPrice  string `json:"openAvgPrice"`
			CloseAvgPrice string `json:"closeAvgPrice"`
			OpenTotalPos  string `json:"openTotalPos"`
			CloseTotalPos string `json:"closeTotalPos"`
			Pnl           string `json:"pnl"`
			NetProfit     string `json:"netProfit"`
			TotalFunding  string `json:"totalFunding"`
			OpenFee       string `json:"openFee"`
			CloseFee      string `json:"closeFee"`
			CTime         string `json:"cTime"`
			UTime         string `json:"uTime"`
			CloseType     string `json:"closeType"`
		} `json:"list"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	records := make([]ClosedPnLRecord, 0, len(resp.List))
	for _, pos := range resp.List {
		record := ClosedPnLRecord{
			Symbol:     pos.Symbol,
			Side:       strings.ToLower(pos.HoldSide),
			ExchangeID: pos.PositionId,
		}

		record.EntryPrice, _ = strconv.ParseFloat(pos.OpenAvgPrice, 64)
		record.ExitPrice, _ = strconv.ParseFloat(pos.CloseAvgPrice, 64)
		record.Quantity, _ = strconv.ParseFloat(pos.CloseTotalPos, 64)
		if record.Quantity == 0 {
			record.Quantity, _ = strconv.ParseFloat(pos.OpenTotalPos, 64)
		}
		// pnl is the gross price PnL; fees are reported separately
		record.RealizedPnL, _ = strconv.ParseFloat(pos.Pnl, 64)
		openFee, _ := strconv.ParseFloat(pos.OpenFee, 64)
		closeFee, _ := strconv.ParseFloat(pos.CloseFee, 64)
		record.Fee = math.Abs(openFee) + math.Abs(closeFee)

		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)
		record.EntryTime = time.UnixMilli(cTime).UTC()
		record.ExitTime = time.UnixMilli(uTime).UTC()

		record.CloseType = inferBitgetCloseType(pos.CloseType)
		records = append(records, record)
	}

	return records, nil
}

// clearCache clears all caches
func (t *BitgetTrader) clearCache() {
	t.balanceCacheMutex.Lock()
	t.cachedBalance = nil
	t.balanceCacheMutex.Unlock()

	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheMutex.Unlock()
}

// genBitgetClientOid generates unique client order ID
func genBitgetClientOid() string {
	timestamp := time.Now().UnixNano() % 10000000000000
	rand := time.Now().Nanosecond() % 100000
	return fmt.Sprintf("nofx%d%05d", timestamp, rand)
}

// GetOpenOrders gets pending regular orders and TP/SL plan orders (symbol "" = all symbols)
func (t *BitgetTrader) GetOpenOrders(symbol string) ([]OpenOrder, error) {
	symbol = t.convertSymbol(symbol)
	result := []OpenOrder{}

	params := map[string]interface{}{"productType": bitgetProductType}
	if symbol != "" {
		params["symbol"] = symbol
	}
	data, err := t.doRequest("GET", bitgetPendingPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending orders: %w", err)
	}
	var pending struct {
		EntrustedList []struct {
			OrderId    string `json:"orderId"`
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			Side       string `json:"side"`
			PosSide    string `json:"posSide"`
			TradeSide  string `json:"tradeSide"`
			OrderType  string `json:"orderType"`
			Price      string `json:"price"`
			Size       string `json:"size"`
			ReduceOnly string `json:"reduceOnly"`
		} `json:"entrustedList"`
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("failed to parse pending orders: %w", err)
	}
	for _, o := range pending.EntrustedList {
		price, _ := strconv.ParseFloat(o.Price, 64)
		size, _ := strconv.ParseFloat(o.Size, 64)
		side := strings.ToLower(o.Side)
		posSide := strings.ToUpper(bitgetDirection(o.PosSide))
		if posSide != "" {
			// hedge mode: side names the POSITION direction, tradeSide says open|close. Report
			// the real order direction like every other exchange: closing a long is a SELL.
			if strings.EqualFold(o.TradeSide, "close") {
				side = map[string]string{"LONG": "sell", "SHORT": "buy"}[posSide]
			} else {
				side = map[string]string{"LONG": "buy", "SHORT": "sell"}[posSide]
			}
		} else {
			// one-way: reduceOnly sell closes a long, reduceOnly buy closes a short
			reduce := strings.EqualFold(o.ReduceOnly, "YES") || strings.EqualFold(o.TradeSide, "close")
			switch {
			case side == "buy" && !reduce, side == "sell" && reduce:
				posSide = "LONG"
			default:
				posSide = "SHORT"
			}
		}
		status := normalizeBitgetStatus(o.Status)
		result = append(result, OpenOrder{
			OrderID:      o.OrderId,
			Symbol:       o.Symbol,
			Side:         strings.ToUpper(side),
			PositionSide: posSide,
			Type:         strings.ToUpper(o.OrderType),
			Price:        price,
			Quantity:     size,
			Status:       status,
		})
	}

	plans, err := t.listPlanOrders(symbol)
	if err != nil {
		return nil, err
	}
	for _, o := range plans {
		kind := bitgetPlanKind(o.PlanType)
		if kind == 0 {
			continue
		}
		trigger, _ := strconv.ParseFloat(o.TriggerPrice, 64)
		size, _ := strconv.ParseFloat(o.Size, 64)
		typ := "STOP_MARKET"
		if kind == bitgetKindTP {
			typ = "TAKE_PROFIT_MARKET"
		}
		// PositionSide is the position the plan protects; Side is the order side that closes it
		// (one-way: exactly the plan's own side, e.g. a long's SL is SELL / LONG).
		dir := planOrderDirection(o)
		posSide := strings.ToUpper(dir)
		closeSide := strings.ToUpper(o.Side)
		switch dir {
		case "long":
			closeSide = "SELL"
		case "short":
			closeSide = "BUY"
		}
		result = append(result, OpenOrder{
			OrderID:      o.OrderId,
			Symbol:       o.Symbol,
			Side:         closeSide,
			PositionSide: posSide,
			Type:         typ,
			StopPrice:    trigger,
			Quantity:     size,
			Status:       "NEW",
		})
	}

	return result, nil
}
