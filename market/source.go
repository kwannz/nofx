package market

import (
	"fmt"
	"strings"

	"nofx/logger"
	"nofx/provider/bitget"
)

// Source selects which venue market data is fetched from.
type Source string

const (
	// SourceDefault is the legacy routing (CoinAnk/Binance for crypto, Hyperliquid for xyz assets).
	SourceDefault Source = ""
	// SourceBitget fetches klines/OI/funding from Bitget USDT-futures public API.
	SourceBitget Source = "bitget"
)

// GetWithSource is Get with an explicit data source.
func GetWithSource(symbol string, src Source) (*Data, error) {
	if src != SourceBitget {
		return Get(symbol)
	}
	return getFromBitget(symbol, []string{"3m", "4h"}, "3m", 100, true)
}

// GetWithTimeframesFromSource is GetWithTimeframes with an explicit data source.
func GetWithTimeframesFromSource(symbol string, timeframes []string, primaryTimeframe string, count int, src Source) (*Data, error) {
	if src != SourceBitget {
		return GetWithTimeframes(symbol, timeframes, primaryTimeframe, count)
	}
	return getFromBitget(symbol, timeframes, primaryTimeframe, count, false)
}

// SourceForExchange maps an exchange type to its market data source. Bitget (live/demo and the
// local paper exchange) uses Bitget public data so TradFi symbols such as NVDAUSDT / XAUUSDT
// resolve to the same venue that executes the orders; every other exchange keeps the legacy
// routing. It is the single place that knows which exchange types are Bitget.
func SourceForExchange(exchangeType string) Source {
	switch strings.ToLower(strings.TrimSpace(exchangeType)) {
	case "bitget", "bitget_paper":
		return SourceBitget
	}
	return SourceDefault
}

// BitgetKlines fetches klines from Bitget's public USDT-futures candles endpoint (ascending by
// time, CloseTime filled from the timeframe when it is a known one). The symbol is used as given:
// normalize it with NormalizeForSource(symbol, SourceBitget) first.
func BitgetKlines(symbol, tf string, limit int) ([]Kline, error) {
	candles, err := bitget.GetCandles(symbol, tf, limit)
	if err != nil {
		return nil, err
	}
	dur, derr := TFDuration(tf)
	klines := make([]Kline, len(candles))
	for i, c := range candles {
		k := Kline{
			OpenTime:    c.OpenTime,
			Open:        c.Open,
			High:        c.High,
			Low:         c.Low,
			Close:       c.Close,
			Volume:      c.Volume,
			QuoteVolume: c.QuoteVolume,
		}
		if derr == nil {
			k.CloseTime = c.OpenTime + dur.Milliseconds() - 1
		}
		klines[i] = k
	}
	return klines, nil
}

// getFromBitget builds a Data snapshot entirely from Bitget public data.
// legacy=true mimics Get() (intraday + longer-term series); otherwise it mimics
// GetWithTimeframes() (TimeframeData). Symbols are never mapped to xyz: names.
func getFromBitget(symbol string, timeframes []string, primaryTimeframe string, count int, legacy bool) (*Data, error) {
	symbol = NormalizeForSource(symbol, SourceBitget)
	if len(timeframes) == 0 {
		return nil, fmt.Errorf("at least one timeframe is required")
	}
	if primaryTimeframe == "" {
		primaryTimeframe = timeframes[0]
	}

	var data *Data
	if legacy {
		k3m, err := BitgetKlines(symbol, "3m", 100)
		if err != nil {
			return nil, fmt.Errorf("failed to get 3-minute K-line from Bitget: %w", err)
		}
		if len(k3m) == 0 {
			return nil, fmt.Errorf("3-minute K-line data is empty")
		}
		if isStaleData(k3m, symbol) {
			return nil, fmt.Errorf("%s data is stale, possible cache failure", symbol)
		}
		k4h, err := BitgetKlines(symbol, "4h", 100)
		if err != nil {
			return nil, fmt.Errorf("failed to get 4-hour K-line from Bitget: %w", err)
		}
		if len(k4h) == 0 {
			return nil, fmt.Errorf("4-hour K-line data is empty")
		}
		d, err := buildLegacyData(symbol, k3m, k4h)
		if err != nil {
			return nil, err
		}
		data = d
	} else {
		hasPrimary := false
		for _, tf := range timeframes {
			if tf == primaryTimeframe {
				hasPrimary = true
				break
			}
		}
		if !hasPrimary {
			timeframes = append([]string{primaryTimeframe}, timeframes...)
		}
		tfData := make(map[string]*TimeframeSeriesData)
		var primary []Kline
		for _, tf := range timeframes {
			// Fail fast on a timeframe Bitget has no candles for instead of sending a request that
			// is bound to fail: a missing secondary timeframe only costs that series, but the
			// primary timeframe drives every indicator and cannot be skipped.
			if _, gerr := bitget.Granularity(tf); gerr != nil {
				if tf == primaryTimeframe {
					return nil, fmt.Errorf("primary timeframe %q is not supported by Bitget: %w", tf, gerr)
				}
				logger.Warnf("⚠️ Skipping %s timeframe %q: not supported by Bitget (%v)", symbol, tf, gerr)
				continue
			}
			klines, err := BitgetKlines(symbol, tf, 200)
			if err != nil {
				logger.Infof("⚠️ Failed to get %s %s K-line from Bitget: %v", symbol, tf, err)
				continue
			}
			if len(klines) == 0 {
				logger.Infof("⚠️ %s %s K-line data is empty", symbol, tf)
				continue
			}
			if tf == primaryTimeframe {
				primary = klines
			}
			tfData[tf] = calculateTimeframeSeries(klines, tf, count)
		}
		if len(primary) == 0 {
			return nil, fmt.Errorf("Primary timeframe %s K-line data is empty", primaryTimeframe)
		}
		if isStaleData(primary, symbol) {
			return nil, fmt.Errorf("%s data is stale, possible cache failure", symbol)
		}
		data = &Data{
			Symbol:        symbol,
			CurrentPrice:  primary[len(primary)-1].Close,
			PriceChange1h: calculatePriceChangeByBars(primary, primaryTimeframe, 60),
			PriceChange4h: calculatePriceChangeByBars(primary, primaryTimeframe, 240),
			CurrentEMA20:  calculateEMA(primary, 20),
			CurrentMACD:   calculateMACD(primary),
			CurrentRSI7:   calculateRSI(primary, 7),
			TimeframeData: tfData,
		}
	}

	// OI (base units) and funding; failures leave them unset so formatters omit them.
	if oi, err := bitget.GetOpenInterest(symbol); err == nil && oi > 0 {
		data.OpenInterest = &OIData{Latest: oi, Average: oi * 0.999}
	}
	if fr, err := bitget.GetFundingRate(symbol); err == nil {
		data.FundingRate = fr
	} else {
		data.FundingRateUnavailable = true
	}
	return data, nil
}

// buildLegacyData mirrors the tail of Get() for pre-fetched 3m/4h series.
func buildLegacyData(symbol string, k3m, k4h []Kline) (*Data, error) {
	price := k3m[len(k3m)-1].Close
	d := &Data{
		Symbol:            symbol,
		CurrentPrice:      price,
		CurrentEMA20:      calculateEMA(k3m, 20),
		CurrentMACD:       calculateMACD(k3m),
		CurrentRSI7:       calculateRSI(k3m, 7),
		IntradaySeries:    calculateIntradaySeries(k3m),
		LongerTermContext: calculateLongerTermData(k4h),
	}
	if len(k3m) >= 21 {
		if p := k3m[len(k3m)-21].Close; p > 0 {
			d.PriceChange1h = (price - p) / p * 100
		}
	}
	if len(k4h) >= 2 {
		if p := k4h[len(k4h)-2].Close; p > 0 {
			d.PriceChange4h = (price - p) / p * 100
		}
	}
	return d, nil
}

// NormalizeForSource normalizes a symbol for the given data source. For
// SourceBitget the symbol stays an XXXUSDT contract name (no xyz: mapping); for
// every other source it is identical to Normalize.
func NormalizeForSource(symbol string, src Source) string {
	if src != SourceBitget {
		return Normalize(symbol)
	}
	s := strings.ToUpper(strings.TrimSpace(symbol))
	s = strings.TrimPrefix(s, "XYZ:")
	if s != "" && !strings.HasSuffix(s, "USDT") {
		s += "USDT"
	}
	return s
}
