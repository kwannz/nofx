package api

import "testing"

func TestNormalizeSymbolForExchange(t *testing.T) {
	cases := []struct{ symbol, exchange, want string }{
		{"nvda", "bitget_paper", "NVDAUSDT"},
		{"NVDAUSDT", "bitget", "NVDAUSDT"},
		{"xyz:NVDA", "bitget_paper", "NVDAUSDT"},
		{"btc", "bitget_paper", "BTCUSDT"},
		{"BTCUSDT", "binance", "BTCUSDT"},
		{"NVDA", "hyperliquid", "xyz:NVDA"}, // legacy mapping stays for other exchanges
	}
	for _, c := range cases {
		if got := normalizeSymbolForExchange(c.symbol, c.exchange); got != c.want {
			t.Errorf("normalizeSymbolForExchange(%q, %q) = %q, want %q", c.symbol, c.exchange, got, c.want)
		}
	}
}
