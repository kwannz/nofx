package store

import "testing"

func TestGetExchangeNameAndType_BitgetPaper(t *testing.T) {
	name, typ := getExchangeNameAndType("bitget_paper")
	if name != "Bitget Paper" || typ != "cex" {
		t.Fatalf("got %q/%q", name, typ)
	}
	if name, _ := getExchangeNameAndType("bitget"); name != "Bitget Futures" {
		t.Fatalf("bitget display name changed: %q", name)
	}
}
