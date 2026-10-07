package store

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
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

// oldExchangesTable is the exchanges schema before the bitget_position_mode column existed.
const oldExchangesTable = `CREATE TABLE exchanges (
		id text PRIMARY KEY, exchange_type text NOT NULL DEFAULT '', account_name text NOT NULL DEFAULT '',
		user_id text NOT NULL DEFAULT 'default', name text NOT NULL, type text NOT NULL, enabled numeric DEFAULT false,
		api_key text DEFAULT '', secret_key text DEFAULT '', passphrase text DEFAULT '', testnet numeric DEFAULT false,
		hyperliquid_wallet_addr text DEFAULT '', aster_user text DEFAULT '', aster_signer text DEFAULT '',
		aster_private_key text DEFAULT '', lighter_wallet_addr text DEFAULT '', lighter_private_key text DEFAULT '',
		lighter_api_key_private_key text DEFAULT '', lighter_api_key_index integer DEFAULT 0,
		created_at datetime, updated_at datetime)`

// An exchanges table created before the bitget_position_mode column existed must be upgraded by
// initTables. The existing rows were created while nofx forced one-way mode, so they must come out
// as one_way (their behaviour does not change), while accounts created afterwards default to hedge.
func TestExchangeBitgetPositionModeMigrationKeepsExistingAccountsOneWay(t *testing.T) {
	gdb, err := InitGorm(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(oldExchangesTable).Error; err != nil {
		t.Fatal(err)
	}
	old := map[string]string{"old-bitget": "bitget", "old-paper": "bitget_paper", "old-binance": "binance"}
	for id, typ := range old {
		if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES (?, ?, 'Default', 'u1', 'x', 'cex')`, id, typ).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := NewExchangeStore(gdb)
	if err := s.initTables(); err != nil {
		t.Fatalf("initTables on the old schema: %v", err)
	}
	for id := range old {
		if got := bitgetModeOf(t, s, id); got != "one_way" {
			t.Errorf("existing row %s: mode %q, want one_way", id, got)
		}
	}
	ex, err := s.GetByID("u1", "old-paper")
	if err != nil || ex.BitgetPositionMode != BitgetPositionModeOneWay {
		t.Errorf("model of the existing paper account: %+v, err %v", ex, err)
	}

	// an account created after the migration defaults to hedge (empty / unknown request value too)
	for name, mode := range map[string]string{"empty": "", "unknown": "bogus", "explicit": "hedge"} {
		id, err := s.Create("u1", "bitget", "new-"+name, true, "k", "s", "p", false, "", "", "", "", "", "", "", 0, mode)
		if err != nil {
			t.Fatal(err)
		}
		if got := bitgetModeOf(t, s, id); got != "hedge" {
			t.Errorf("new %s account: mode %q, want hedge", name, got)
		}
	}
	// ...as does one inserted without naming the column (the column default is hedge)
	if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES ('raw-new', 'bitget', 'raw', 'u1', 'x', 'cex')`).Error; err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "raw-new"); got != "hedge" {
		t.Errorf("a row inserted without the column: mode %q, want hedge", got)
	}
	// the legacy create path defaults to hedge as well
	if err := s.CreateLegacy("u1", "legacy-uuid", "Legacy", "cex", true, "k", "s", false, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "legacy-uuid"); got != "hedge" {
		t.Errorf("CreateLegacy: mode %q, want hedge", got)
	}

	// the one-time backfill must not run again: a later start keeps what the user chose since
	if err := s.Update("u1", "old-bitget", true, "", "", "", false, "", "", "", "", "", "", "", 0, "hedge"); err != nil {
		t.Fatal(err)
	}
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "old-bitget"); got != "hedge" {
		t.Errorf("second initTables must not reset the mode: old-bitget is %q, want hedge (chosen in the UI)", got)
	}
	if got := bitgetModeOf(t, s, "old-paper"); got != "one_way" {
		t.Errorf("second initTables changed old-paper to %q", got)
	}
}

// A fresh database has no rows to protect: the table is created with the hedge default.
func TestExchangeBitgetPositionModeFreshInstallDefaultsToHedge(t *testing.T) {
	gdb, err := InitGorm(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewExchangeStore(gdb)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	id, err := s.Create("u1", "bitget_paper", "acc", true, "", "", "", false, "", "", "", "", "", "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, id); got != "hedge" {
		t.Errorf("fresh install: new account mode %q, want hedge", got)
	}
}

// A failing backfill must not leave the column behind (it would then never be backfilled).
func TestExchangeBitgetPositionModeMigrationIsAtomic(t *testing.T) {
	gdb, err := InitGorm(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(oldExchangesTable).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES ('old-bitget', 'bitget', 'Default', 'u1', 'x', 'cex')`).Error; err != nil {
		t.Fatal(err)
	}
	// make the backfill UPDATE fail (AutoMigrate may rebuild the table, so a trigger would not survive)
	if err := gdb.Callback().Raw().Before("gorm:raw").Register("test:fail_backfill", func(db *gorm.DB) {
		if strings.Contains(db.Statement.SQL.String(), "SET bitget_position_mode") {
			_ = db.AddError(errors.New("boom"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	s := NewExchangeStore(gdb)
	err = s.initTables()
	if err == nil || !strings.Contains(err.Error(), "one-way Bitget position mode") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the wrapped backfill error, got %v", err)
	}
	if gdb.Migrator().HasColumn(&Exchange{}, "bitget_position_mode") {
		t.Fatal("the column must have been rolled back with the failed backfill")
	}

	// once the cause is gone the next start migrates correctly
	if err := gdb.Callback().Raw().Remove("test:fail_backfill"); err != nil {
		t.Fatal(err)
	}
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "old-bitget"); got != "one_way" {
		t.Errorf("retry: mode %q, want one_way", got)
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

// The PostgreSQL branch (the table already exists, AutoMigrate is skipped) needs a real server, so
// it only runs when NOFX_TEST_POSTGRES_ADDR=host:port points at one that accepts user "postgres"
// (trust authentication; the password is ignored); every run uses its own throwaway database.
func TestExchangeBitgetPositionModeMigrationPostgres(t *testing.T) {
	addr := os.Getenv("NOFX_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("NOFX_TEST_POSTGRES_ADDR not set")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	admin, err := InitGormPostgres(host, port, "postgres", "x", "postgres", "disable")
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("nofx_exchange_mig_%d", time.Now().UnixNano())
	if err := admin.Exec(`CREATE DATABASE ` + dbName).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`) })

	connect := func() *gorm.DB {
		t.Helper()
		g, err := InitGormPostgres(host, port, "postgres", "x", dbName, "disable")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	gdb := connect()
	old := `CREATE TABLE exchanges (
		id text PRIMARY KEY, exchange_type text NOT NULL DEFAULT '', account_name text NOT NULL DEFAULT '',
		user_id text NOT NULL DEFAULT 'default', name text NOT NULL, type text NOT NULL, enabled boolean DEFAULT false,
		api_key text DEFAULT '', secret_key text DEFAULT '', passphrase text DEFAULT '', testnet boolean DEFAULT false,
		hyperliquid_wallet_addr text DEFAULT '', aster_user text DEFAULT '', aster_signer text DEFAULT '',
		aster_private_key text DEFAULT '', lighter_wallet_addr text DEFAULT '', lighter_private_key text DEFAULT '',
		lighter_api_key_private_key text DEFAULT '', lighter_api_key_index integer DEFAULT 0,
		created_at timestamptz, updated_at timestamptz)`
	if err := gdb.Exec(old).Error; err != nil {
		t.Fatal(err)
	}
	for id, typ := range map[string]string{"old-bitget": "bitget", "old-paper": "bitget_paper"} {
		if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES (?, ?, 'Default', 'u1', 'x', 'cex')`, id, typ).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := NewExchangeStore(gdb)

	// a failing second statement rolls back the new column as well (PostgreSQL DDL is transactional)
	if err := gdb.Callback().Raw().Before("gorm:raw").Register("test:fail_default", func(db *gorm.DB) {
		if strings.Contains(db.Statement.SQL.String(), "SET DEFAULT") {
			_ = db.AddError(errors.New("boom"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.initTables(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the wrapped error, got %v", err)
	}
	if gdb.Migrator().HasColumn(&Exchange{}, "bitget_position_mode") {
		t.Fatal("the column must have been rolled back with the failed default change")
	}
	if err := gdb.Callback().Raw().Remove("test:fail_default"); err != nil {
		t.Fatal(err)
	}

	if err := s.initTables(); err != nil {
		t.Fatalf("initTables on the old PostgreSQL schema: %v", err)
	}
	for _, id := range []string{"old-bitget", "old-paper"} {
		if got := bitgetModeOf(t, s, id); got != "one_way" {
			t.Errorf("existing row %s: mode %q, want one_way", id, got)
		}
	}
	var colDefault string
	if err := gdb.Raw(`SELECT column_default FROM information_schema.columns WHERE table_name = 'exchanges' AND column_name = 'bitget_position_mode'`).Scan(&colDefault).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colDefault, "hedge") {
		t.Errorf("column default = %q, want 'hedge' for new rows", colDefault)
	}
	if err := gdb.Exec(`INSERT INTO exchanges (id, exchange_type, account_name, user_id, name, type) VALUES ('raw-new', 'bitget', 'raw', 'u1', 'x', 'cex')`).Error; err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "raw-new"); got != "hedge" {
		t.Errorf("a row inserted without the column: mode %q, want hedge", got)
	}
	id, err := s.Create("u1", "bitget", "acc", true, "k", "s", "p", false, "", "", "", "", "", "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, id); got != "hedge" {
		t.Errorf("new account: mode %q, want hedge", got)
	}

	// restart: the backfill is one-time, what the user chose since stays
	if err := s.Update("u1", "old-bitget", true, "", "", "", false, "", "", "", "", "", "", "", 0, "hedge"); err != nil {
		t.Fatal(err)
	}
	if err := NewExchangeStore(connect()).initTables(); err != nil {
		t.Fatal(err)
	}
	if got := bitgetModeOf(t, s, "old-bitget"); got != "hedge" {
		t.Errorf("a later start reset old-bitget to %q", got)
	}
	if got := bitgetModeOf(t, s, "old-paper"); got != "one_way" {
		t.Errorf("a later start changed old-paper to %q", got)
	}
}
