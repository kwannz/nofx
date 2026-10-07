package store

import (
	"path/filepath"
	"testing"
)

func TestGetExchangeNameAndType_BitgetPaper(t *testing.T) {
	name, typ := getExchangeNameAndType("bitget_paper")
	if name != "Bitget Paper" || typ != "cex" {
		t.Fatalf("got %q/%q", name, typ)
	}
	if name, _ := getExchangeNameAndType("bitget"); name != "Bitget Futures" {
		t.Fatalf("bitget display name changed: %q", name)
	}
}

func TestNormalizeBitgetPositionMode(t *testing.T) {
	for in, want := range map[string]string{
		"": BitgetPositionModeHedge, "hedge": BitgetPositionModeHedge, "HEDGE": BitgetPositionModeHedge,
		"hedge_mode": BitgetPositionModeHedge, "unknown": BitgetPositionModeHedge,
		"one_way": BitgetPositionModeOneWay, "One-Way": BitgetPositionModeOneWay, "one_way_mode": BitgetPositionModeOneWay,
	} {
		if got := NormalizeBitgetPositionMode(in); got != want {
			t.Errorf("NormalizeBitgetPositionMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// bitgetModeOf reads the raw column (bypassing the model) so the test sees what is stored.
func bitgetModeOf(t *testing.T, s *ExchangeStore, id string) string {
	t.Helper()
	var mode string
	if err := s.db.Raw("SELECT bitget_position_mode FROM exchanges WHERE id = ?", id).Scan(&mode).Error; err != nil {
		t.Fatal(err)
	}
	return mode
}

// An exchanges table created before the bitget_position_mode column existed must be upgraded by
// initTables, and the existing rows must come out as hedge (the default).
func TestExchangeBitgetPositionModeMigrationDefaultsToHedge(t *testing.T) {
	gdb, err := InitGorm(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := `CREATE TABLE exchanges (
		id text PRIMARY KEY, exchange_type text NOT NULL DEFAULT '', account_name text NOT NULL DEFAULT '',
		user_id text NOT NULL DEFAULT 'default', name text NOT NULL, type text NOT NULL, enabled numeric DEFAULT false,
		api_key text DEFAULT '', secret_key text DEFAULT '', passphrase text DEFAULT '', testnet numeric DEFAULT false,
		hyperliquid_wallet_addr text DEFAULT '', aster_user text DEFAULT '', aster_signer text DEFAULT '',
		aster_private_key text DEFAULT '', lighter_wallet_addr text DEFAULT '', lighter_private_key text DEFAULT '',
		lighter_api_key_private_key text DEFAULT '', lighter_api_key_index integer DEFAULT 0,
		created_at datetime, updated_at datetime)`
	if err := gdb.Exec(old).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-bitget", "old-paper"} {
		if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES (?, ?, 'Default', 'u1', 'x', 'cex')`, id, "bitget").Error; err != nil {
			t.Fatal(err)
		}
	}
	s := NewExchangeStore(gdb)
	if err := s.initTables(); err != nil {
		t.Fatalf("initTables on the old schema: %v", err)
	}
	for _, id := range []string{"old-bitget", "old-paper"} {
		if got := bitgetModeOf(t, s, id); got != "hedge" {
			t.Errorf("existing row %s: mode %q, want hedge", id, got)
		}
	}
	// running the migration again is harmless
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
}

func TestExchangeCreateAndUpdateBitgetPositionMode(t *testing.T) {
	gdb, err := InitGorm(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewExchangeStore(gdb)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	create := func(mode string) string {
		t.Helper()
		id, err := s.Create("u1", "bitget", "acc", true, "k", "s", "p", false, "", "", "", "", "", "", "", 0, mode)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if got := bitgetModeOf(t, s, create("")); got != "hedge" {
		t.Errorf("empty create mode stored as %q, want hedge", got)
	}
	idOneWay := create("one_way")
	if got := bitgetModeOf(t, s, idOneWay); got != "one_way" {
		t.Errorf("one_way create stored as %q", got)
	}
	if got := bitgetModeOf(t, s, create("bogus")); got != "hedge" {
		t.Errorf("unknown create mode stored as %q, want hedge", got)
	}

	update := func(id, mode string) {
		t.Helper()
		if err := s.Update("u1", id, true, "", "", "", false, "", "", "", "", "", "", "", 0, mode); err != nil {
			t.Fatal(err)
		}
	}
	update(idOneWay, "") // older clients do not send the field: the stored value stays
	if got := bitgetModeOf(t, s, idOneWay); got != "one_way" {
		t.Errorf("an empty update must keep one_way, got %q", got)
	}
	update(idOneWay, "hedge")
	if got := bitgetModeOf(t, s, idOneWay); got != "hedge" {
		t.Errorf("update to hedge stored %q", got)
	}
	update(idOneWay, "one_way")
	ex, err := s.GetByID("u1", idOneWay)
	if err != nil {
		t.Fatal(err)
	}
	if ex.BitgetPositionMode != "one_way" {
		t.Errorf("model field = %q", ex.BitgetPositionMode)
	}
}
