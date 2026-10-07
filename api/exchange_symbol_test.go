package api

import (
	"testing"

	"nofx/market"
)

// The handlers normalize user-supplied symbols with market.NormalizeForSource(symbol,
// market.SourceForExchange(exchange)); this pins the resulting behaviour per exchange type.
func TestNormalizeSymbolForExchange(t *testing.T) {
	cases := []struct{ symbol, exchange, want string }{
		{"nvda", "bitget_paper", "NVDAUSDT"},
		{"NVDAUSDT", "bitget", "NVDAUSDT"},
		{"xyz:NVDA", "bitget_paper", "NVDAUSDT"},
		{"btc", "bitget_paper", "BTCUSDT"},
		{"nvda", "BITGET", "NVDAUSDT"},
		{"BTCUSDT", "binance", "BTCUSDT"},
		{"NVDA", "hyperliquid", "xyz:NVDA"}, // legacy mapping stays for other exchanges
	}
	for _, c := range cases {
		if got := market.NormalizeForSource(c.symbol, market.SourceForExchange(c.exchange)); got != c.want {
			t.Errorf("normalize(%q, %q) = %q, want %q", c.symbol, c.exchange, got, c.want)
		}
	}
}
