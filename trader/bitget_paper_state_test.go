package trader

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// ------------------------------------------------------------------ helpers

// resetPaperRegistryForTest drops every in-memory paper account (without touching snapshot
// files), which is what a process restart does to the registry.
func resetPaperRegistryForTest() {
	paperRegistryMu.Lock()
	old := paperRegistry
	paperRegistry = map[string]*BitgetPaperTrader{}
	paperRegistryMu.Unlock()
	for _, t := range old {
		t.Stop()
	}
}

// usePaperStateDir enables persistence in a fresh temp dir for the duration of the test and
// starts and ends with an empty registry.
func usePaperStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "paper")
	resetPaperRegistryForTest()
	SetPaperStateDir(dir)
	t.Cleanup(func() {
		resetPaperRegistryForTest()
		SetPaperStateDir("")
	})
	return dir
}

// acquirePaper acquires (or restores) the account of id on the fake price source.
func acquirePaper(id string, initial float64, src paperPriceSource, clk *fakeClock) *BitgetPaperTrader {
	return acquireBitgetPaperTrader(id, initial, func(b float64) *BitgetPaperTrader {
		return newBitgetPaperTrader(b, src, clk.Now)
	})
}

// canonicalSnapshot returns the persisted form of the account without the save timestamp.
func canonicalSnapshot(t *testing.T, p *BitgetPaperTrader) string {
	t.Helper()
	p.mu.Lock()
	s := p.snapshotLocked()
	p.mu.Unlock()
	s.SavedAt = time.Time{}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameClosedRecord(a, b ClosedPnLRecord) bool {
	ta, tb := a.EntryTime, a.ExitTime
	a.EntryTime, a.ExitTime = time.Time{}, time.Time{}
	ea, eb := b.EntryTime, b.ExitTime
	b.EntryTime, b.ExitTime = time.Time{}, time.Time{}
	return a == b && ta.Equal(ea) && tb.Equal(eb)
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// ------------------------------------------------------------------ tests

// TestPaperSnapshotRoundTrip: everything the account holds survives a restart (registry dropped,
// account acquired again from the snapshot file).
func TestPaperSnapshotRoundTrip(t *testing.T) {
	dir := usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC)}
	const id = "trader-roundtrip"

	p := acquirePaper(id, 10000, src, clk)
	if err := p.SetLeverage("BTCUSDT", 7); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMarginMode("BTCUSDT", true); err != nil {
		t.Fatal(err)
	}
	openBTC, err := p.OpenLong("BTCUSDT", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("NVDAUSDT", 5, 10); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "LONG", 10, 95); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "LONG", 10, 130); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("NVDAUSDT", "SHORT", 5, 110); err != nil {
		t.Fatal(err)
	}
	// a closed trade (partial close of NVDA then a full re-open + close of BTC is not needed)
	clZ, err := p.CloseShort("NVDAUSDT", 2)
	if err != nil {
		t.Fatal(err)
	}
	// funding settles at 08:00 and is bookkept in the position
	src.setRate("BTCUSDT", 0.0001)
	src.setRate("NVDAUSDT", 0.0002)
	clk.Set(time.Date(2026, 10, 5, 8, 0, 5, 0, time.UTC))
	p.Tick()

	wantBal := balanceOf(t, p)
	wantPos := mustPositions(t, p)
	wantOrders, _ := p.GetOpenOrders("")
	wantClosed, _ := p.GetClosedPnL(time.Time{}, 100)
	wantSnapshot := canonicalSnapshot(t, p)
	if len(wantPos) != 2 || len(wantOrders) != 3 || len(wantClosed) != 1 {
		t.Fatalf("setup: positions %d orders %d closed %d", len(wantPos), len(wantOrders), len(wantClosed))
	}
	p.mu.Lock()
	seqBefore, slotBefore := p.seq, p.lastFundingSlot
	p.mu.Unlock()
	if slotBefore == 0 {
		t.Fatal("funding settlement must be recorded in lastFundingSlot")
	}

	// "restart": the registry entry is gone, only the file is left
	resetPaperRegistryForTest()
	if got := dirFiles(t, dir); len(got) != 1 || got[0] != id+".json" {
		t.Fatalf("state dir content = %v, want exactly %s.json", got, id)
	}

	p2 := acquirePaper(id, 777, src, clk) // initialBalance must be ignored: a snapshot exists
	if p2 == p {
		t.Fatal("registry was reset: expected a new instance")
	}
	if got := canonicalSnapshot(t, p2); got != wantSnapshot {
		t.Fatalf("restored state differs\n got: %s\nwant: %s", got, wantSnapshot)
	}
	b2 := balanceOf(t, p2)
	for _, k := range []string{"totalWalletBalance", "availableBalance", "totalEquity", "totalUnrealizedProfit"} {
		near(t, k, mustFloat(t, b2, k), mustFloat(t, wantBal, k))
	}
	pos2 := mustPositions(t, p2)
	if len(pos2) != len(wantPos) {
		t.Fatalf("positions after restart: %d, want %d", len(pos2), len(wantPos))
	}
	for i := range pos2 {
		for _, k := range []string{"entryPrice", "positionAmt", "liquidationPrice", "leverage", "unRealizedProfit"} {
			near(t, pos2[i]["symbol"].(string)+" "+k, mustFloat(t, pos2[i], k), mustFloat(t, wantPos[i], k))
		}
		if pos2[i]["symbol"] != wantPos[i]["symbol"] || pos2[i]["side"] != wantPos[i]["side"] || pos2[i]["createdTime"] != wantPos[i]["createdTime"] {
			t.Fatalf("position %d identity differs: %v vs %v", i, pos2[i], wantPos[i])
		}
	}
	orders2, _ := p2.GetOpenOrders("")
	if len(orders2) != len(wantOrders) {
		t.Fatalf("SL/TP orders after restart: %d, want %d", len(orders2), len(wantOrders))
	}
	for i := range orders2 {
		if orders2[i] != wantOrders[i] {
			t.Fatalf("order %d: %+v vs %+v", i, orders2[i], wantOrders[i])
		}
	}
	closed2, _ := p2.GetClosedPnL(time.Time{}, 100)
	if len(closed2) != 1 || !sameClosedRecord(closed2[0], wantClosed[0]) {
		t.Fatalf("closed trades after restart: %+v vs %+v", closed2, wantClosed)
	}
	// fill index: both an old open and the close are still queryable
	for _, oid := range []string{openBTC["orderId"].(string), clZ["orderId"].(string)} {
		if st, err := p2.GetOrderStatus("BTCUSDT", oid); err != nil || st["status"] != "FILLED" {
			t.Fatalf("GetOrderStatus(%s) after restart: %v %v", oid, st, err)
		}
	}
	// counters and settings
	p2.mu.Lock()
	seqAfter, slotAfter := p2.seq, p2.lastFundingSlot
	lev, cross := p2.leverage["BTCUSDT"], p2.crossMode["BTCUSDT"]
	initial := p2.initial
	p2.mu.Unlock()
	if seqAfter != seqBefore || slotAfter != slotBefore {
		t.Fatalf("seq %d/%d or funding slot %d/%d not restored", seqAfter, seqBefore, slotAfter, slotBefore)
	}
	if lev != 10 || !cross { // lev was last set to 10 by the open order
		t.Fatalf("leverage/margin settings not restored: lev=%d cross=%v", lev, cross)
	}
	if initial != 10000 {
		t.Fatalf("initial balance = %v, want 10000 (snapshot wins over the argument)", initial)
	}

	// the restored SL/TP is live: it fires on the restored account and the new order id is unique
	src.setMark("BTCUSDT", 94) // below the stop loss, above the 10x liquidation price (~90.5)
	p2.Tick()
	if left := mustPositions(t, p2); len(left) != 1 || left[0]["symbol"] != "NVDAUSDT" {
		t.Fatalf("restored stop loss did not fire: %+v", left)
	}
	closed3, _ := p2.GetClosedPnL(time.Time{}, 100)
	if len(closed3) != 2 || closed3[0].CloseType != paperReasonStopLoss {
		t.Fatalf("closed trades after SL: %+v", closed3)
	}
	if closed3[0].OrderID == clZ["orderId"] || closed3[0].OrderID == openBTC["orderId"] {
		t.Fatal("order ids must stay unique across restarts")
	}
}

// TestPaperSnapshotOnlyAppliesInitialBalanceWithoutSnapshot: a fresh account gets initialBalance,
// and an account with a snapshot keeps its own.
func TestPaperSnapshotOnlyAppliesInitialBalanceWithoutSnapshot(t *testing.T) {
	usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}

	p := acquirePaper("t-initial", 5000, src, clk)
	near(t, "fresh", mustFloat(t, balanceOf(t, p), "totalEquity"), 5000)
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	eq := mustFloat(t, balanceOf(t, p), "totalEquity")
	resetPaperRegistryForTest()
	p2 := acquirePaper("t-initial", 9999, src, clk)
	near(t, "restored", mustFloat(t, balanceOf(t, p2), "totalEquity"), eq)
}

// TestPaperSnapshotMatcherMutationsPersist: SL/TP fills and funding done by the matcher are
// written without any call from the AutoTrader.
func TestPaperSnapshotMatcherMutationsPersist(t *testing.T) {
	usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := acquirePaper("t-matcher", 10000, src, clk)
	if _, err := p.OpenLong("BTCUSDT", 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "LONG", 2, 95); err != nil {
		t.Fatal(err)
	}
	src.setMark("BTCUSDT", 94) // below the stop loss, above the 10x liquidation price (~90.5)
	p.Tick()                   // SL fires, nothing else touches the account
	want := canonicalSnapshot(t, p)

	resetPaperRegistryForTest()
	p2 := acquirePaper("t-matcher", 10000, src, clk)
	if got := canonicalSnapshot(t, p2); got != want {
		t.Fatalf("SL fill not persisted\n got: %s\nwant: %s", got, want)
	}
	if len(mustPositions(t, p2)) != 0 {
		t.Fatal("position closed by the SL must stay closed after a restart")
	}
	if c, _ := p2.GetClosedPnL(time.Time{}, 10); len(c) != 1 || c[0].CloseType != paperReasonStopLoss {
		t.Fatalf("closed trades: %+v", c)
	}
}

// TestPaperSnapshotCancelAndReplaceTriggersPersist covers CancelStopOrders and trigger replacement.
func TestPaperSnapshotCancelAndReplaceTriggersPersist(t *testing.T) {
	usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := acquirePaper("t-cancel", 10000, src, clk)
	if _, err := p.OpenLong("BTCUSDT", 2, 10); err != nil {
		t.Fatal(err)
	}
	_ = p.SetStopLoss("BTCUSDT", "LONG", 2, 90)
	_ = p.SetStopLoss("BTCUSDT", "LONG", 2, 95) // replaces
	_ = p.SetTakeProfit("BTCUSDT", "LONG", 2, 130)
	resetPaperRegistryForTest()
	p2 := acquirePaper("t-cancel", 10000, src, clk)
	orders, _ := p2.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 || orders[0].StopPrice != 95 {
		t.Fatalf("replaced SL / TP not restored: %+v", orders)
	}
	if err := p2.CancelStopLossOrders("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	resetPaperRegistryForTest()
	p3 := acquirePaper("t-cancel", 10000, src, clk)
	orders, _ = p3.GetOpenOrders("BTCUSDT")
	if len(orders) != 1 || orders[0].Type != "TAKE_PROFIT_MARKET" {
		t.Fatalf("cancelled SL must stay cancelled: %+v", orders)
	}
}

// TestPaperSnapshotBadFilesAreIgnored: corrupted, truncated, structurally invalid and
// unknown-version snapshots never crash the load; the account starts fresh, the bad file is kept
// aside as .rejected and the next write produces a valid snapshot.
func TestPaperSnapshotBadFilesAreIgnored(t *testing.T) {
	valid := func(version int) string {
		s := paperSnapshot{
			Version: version, TraderID: "t-bad", Initial: 1234, Cash: 1000, Seq: 3,
			Positions: []paperPositionSnap{{ID: "p1", Symbol: "BTCUSDT", Side: "long", Qty: 1, Entry: 100, Leverage: 5, Margin: 20}},
		}
		b, _ := json.Marshal(s)
		return string(b)
	}
	cases := map[string]string{
		"garbage":         "this is not json {{{",
		"truncated":       valid(1)[:40],
		"empty":           "",
		"unknown version": valid(2),
		"version zero":    valid(0),
		"negative qty":    strings.Replace(valid(1), `"qty":1`, `"qty":-1`, 1),
		"bad side":        strings.Replace(valid(1), `"side":"long"`, `"side":"flat"`, 1),
		"other trader":    strings.Replace(valid(1), `"trader_id":"t-bad"`, `"trader_id":"somebody-else"`, 1),
		"negative cash":   strings.Replace(valid(1), `"cash":1000`, `"cash":-5`, 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := usePaperStateDir(t)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "t-bad.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			src := newFakePaperSource()
			clk := &fakeClock{t: paperMonday}
			p := acquirePaper("t-bad", 5000, src, clk)
			near(t, "fresh balance", mustFloat(t, balanceOf(t, p), "totalEquity"), 5000)
			if len(mustPositions(t, p)) != 0 {
				t.Fatal("no positions may be loaded from a rejected snapshot")
			}
			kept, err := os.ReadFile(path + paperRejectedSuffix)
			if err != nil || string(kept) != content {
				t.Fatalf("rejected file must be kept verbatim: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("bad snapshot must be moved away, stat err=%v", err)
			}
			// the account is usable and writes a valid snapshot again
			if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
				t.Fatal(err)
			}
			resetPaperRegistryForTest()
			p2 := acquirePaper("t-bad", 5000, src, clk)
			if len(mustPositions(t, p2)) != 1 {
				t.Fatal("fresh account must persist normally after a rejected snapshot")
			}
		})
	}
}

// TestPaperReleaseRemovesSnapshot: deleting the trader deletes its file (also when the account
// is not in the registry, e.g. right after a restart) and a later trader with the same ID starts fresh.
func TestPaperReleaseRemovesSnapshot(t *testing.T) {
	dir := usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}

	p := acquirePaper("t-release", 10000, src, clk)
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "t-release.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot must exist after a trade: %v", err)
	}
	ReleaseBitgetPaperTrader("t-release")
	if got := dirFiles(t, dir); len(got) != 0 {
		t.Fatalf("state dir after release = %v, want empty", got)
	}
	// a straggler still holding the released account must not resurrect the file
	if _, err := p.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	if got := dirFiles(t, dir); len(got) != 0 {
		t.Fatalf("released account wrote %v", got)
	}
	fresh := acquirePaper("t-release", 3000, src, clk)
	near(t, "fresh account after release", mustFloat(t, balanceOf(t, fresh), "totalEquity"), 3000)

	// release without a registry entry (process restarted, trader deleted afterwards)
	if _, err := fresh.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	resetPaperRegistryForTest()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot expected: %v", err)
	}
	ReleaseBitgetPaperTrader("t-release")
	if got := dirFiles(t, dir); len(got) != 0 {
		t.Fatalf("state dir after unregistered release = %v, want empty", got)
	}
}

// TestPaperSnapshotAtomicWriteLeavesNoTempFiles: after many mutations the directory holds only the
// snapshot, and a failing write cleans up its temp file and keeps the previous content.
func TestPaperSnapshotAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := acquirePaper("t-atomic", 10000, src, clk)
	for i := 0; i < 20; i++ {
		if _, err := p.OpenLong("BTCUSDT", 0.5, 5); err != nil {
			t.Fatal(err)
		}
		_ = p.SetStopLoss("BTCUSDT", "LONG", 1, 90)
		if _, err := p.CloseLong("BTCUSDT", 0.25); err != nil {
			t.Fatal(err)
		}
	}
	if got := dirFiles(t, dir); len(got) != 1 || got[0] != "t-atomic.json" {
		t.Fatalf("state dir = %v, want only t-atomic.json", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "t-atomic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap paperSnapshot
	if err := json.Unmarshal(data, &snap); err != nil || snap.Version != paperSnapshotVersion {
		t.Fatalf("snapshot must be valid JSON with version %d: %v", paperSnapshotVersion, err)
	}

	// failing rename (target is a non-empty directory): error returned, no temp file left behind
	bad := filepath.Join(dir, "blocked.json")
	if err := os.MkdirAll(filepath.Join(bad, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(bad, []byte("{}")); err == nil {
		t.Fatal("expected the write over a directory to fail")
	}
	for _, name := range dirFiles(t, dir) {
		if strings.Contains(name, ".tmp-") {
			t.Fatalf("temp file left behind: %s", name)
		}
	}
}

// TestPaperSnapshotHistoryIsCapped: only the recent fills / closed trades are persisted.
func TestPaperSnapshotHistoryIsCapped(t *testing.T) {
	p, _, _ := newTestPaper(t, 1e9)
	p.mu.Lock()
	for i := 0; i < paperPersistMaxFills+50; i++ {
		p.recordFillLocked(&PaperFill{OrderID: "o" + string(rune('A'+i%26)) + strings.Repeat("x", i/26), Symbol: "BTCUSDT"})
	}
	for i := 0; i < paperPersistMaxClosed+50; i++ {
		p.recordClosedLocked(ClosedPnLRecord{Symbol: "BTCUSDT", OrderID: strings.Repeat("c", i+1)})
	}
	s := p.snapshotLocked()
	p.mu.Unlock()
	if len(s.Fills) != paperPersistMaxFills || len(s.Closed) != paperPersistMaxClosed {
		t.Fatalf("persisted %d fills / %d closed, want %d / %d", len(s.Fills), len(s.Closed), paperPersistMaxFills, paperPersistMaxClosed)
	}
	if s.Closed[len(s.Closed)-1].OrderID != strings.Repeat("c", paperPersistMaxClosed+50) {
		t.Fatal("the newest closed trade must be kept")
	}
}

func TestPaperStateDisabledByDefault(t *testing.T) {
	resetPaperRegistryForTest()
	SetPaperStateDir("")
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := acquirePaper("t-nopersist", 10000, src, clk)
	defer resetPaperRegistryForTest()
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	path := p.statePath
	p.mu.Unlock()
	if path != "" {
		t.Fatalf("persistence must be off without a state dir, statePath=%q", path)
	}
}

func TestPaperStateFileNameAndDefaultDir(t *testing.T) {
	if got := paperStateFileName("abc-123_X.y"); got != "abc-123_X.y.json" {
		t.Fatalf("safe id must be kept: %s", got)
	}
	seen := map[string]string{}
	for _, id := range []string{"a/b", "a_b", "a\\b", "../../etc/passwd", "..", ".", "", "a b", "a:b", strings.Repeat("z", 300)} {
		name := paperStateFileName(id)
		if strings.ContainsAny(name, `/\`) || name == ".json" || strings.HasPrefix(name, "..") {
			t.Fatalf("unsafe file name %q for id %q", name, id)
		}
		if len(name) > 140 {
			t.Fatalf("file name too long (%d) for id %q", len(name), id)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("ids %q and %q share file %q", prev, id, name)
		}
		seen[name] = id
	}

	if got := DefaultPaperStateDir("sqlite", "data/data.db"); got != filepath.Join("data", "paper") {
		t.Fatalf("sqlite default = %s", got)
	}
	if got := DefaultPaperStateDir("sqlite", "/var/lib/nofx/prod.db"); got != "/var/lib/nofx/paper" {
		t.Fatalf("sqlite abs default = %s", got)
	}
	if got := DefaultPaperStateDir("postgres", "ignored.db"); got != filepath.Join("data", "paper") {
		t.Fatalf("postgres default = %s", got)
	}
}

// ------------------------------------------------------------------ reconciliation

func newReconcileAutoTrader(t *testing.T, id string, paper *BitgetPaperTrader) (*AutoTrader, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "paper.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &AutoTrader{
		id:                    id,
		name:                  "paper",
		exchange:              "bitget_paper",
		exchangeID:            "paper-exchange-1",
		store:                 st,
		trader:                paper,
		positionFirstSeenTime: map[string]int64{},
	}, st
}

func createDBPosition(t *testing.T, st *store.Store, traderID, symbol, side string, qty, entry, fees float64) *store.TraderPosition {
	t.Helper()
	nowMs := time.Now().UTC().UnixMilli()
	pos := &store.TraderPosition{
		TraderID: traderID, ExchangeID: "paper-exchange-1", ExchangeType: "bitget_paper",
		Symbol: symbol, Side: side, Quantity: qty, EntryPrice: entry, EntryOrderID: "e-" + symbol,
		EntryTime: nowMs, Leverage: 5, Fee: fees, CreatedAt: nowMs, UpdatedAt: nowMs,
	}
	if err := st.Position().Create(pos); err != nil {
		t.Fatal(err)
	}
	return pos
}

// TestPaperReconcileClosesOrphanedDBPositions: DB OPEN positions the paper account does not hold are
// closed as paper_state_lost at the mark, with no extra fee; matching positions, other traders'
// positions and already closed ones are left alone.
func TestPaperReconcileClosesOrphanedDBPositions(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	at, st := newReconcileAutoTrader(t, "paper-reconcile-1", p)

	// the paper account really holds NVDAUSDT long and BTCUSDT short
	if _, err := p.OpenLong("NVDAUSDT", 5, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	keepNVDA := createDBPosition(t, st, at.id, "NVDAUSDT", "LONG", 5, 100, 0.3)
	keepBTC := createDBPosition(t, st, at.id, "BTCUSDT", "SHORT", 1, 100, 0.1)
	// orphans: no such position in the paper account (wrong symbol, and the opposite side)
	orphanShort := createDBPosition(t, st, at.id, "BTCUSDT", "LONG", 2, 90, 1.5)
	orphanLong := createDBPosition(t, st, at.id, "NVDAUSDT", "SHORT", 3, 120, 0.7)
	other := createDBPosition(t, st, "some-other-trader", "BTCUSDT", "SHORT", 1, 100, 0)

	src.setMark("BTCUSDT", 100)
	src.setMark("NVDAUSDT", 100)
	at.reconcilePaperPositions(p)

	got := func(id int64) *store.TraderPosition {
		t.Helper()
		var x store.TraderPosition
		if err := st.GormDB().First(&x, id).Error; err != nil {
			t.Fatalf("position %d: %v", id, err)
		}
		return &x
	}

	for _, keep := range []*store.TraderPosition{keepNVDA, keepBTC, other} {
		if x := got(keep.ID); x.Status != "OPEN" || x.CloseReason != "" {
			t.Errorf("position #%d %s %s must stay OPEN: %+v", keep.ID, keep.Symbol, keep.Side, x)
		}
	}
	// orphan BTC long: entry 90 -> mark 100 = +20; orphan NVDA short: entry 120 -> mark 100 = +60
	for _, c := range []struct {
		pos     *store.TraderPosition
		wantPnL float64
	}{{orphanShort, 20}, {orphanLong, 60}} {
		x := got(c.pos.ID)
		if x.Status != "CLOSED" || x.CloseReason != "paper_state_lost" {
			t.Fatalf("orphan #%d not closed with paper_state_lost: %+v", c.pos.ID, x)
		}
		near(t, "exit price = mark", x.ExitPrice, 100)
		near(t, "realized pnl at mark", x.RealizedPnL, c.wantPnL)
		near(t, "no additional fee", x.Fee, c.pos.Fee)
		if x.ExitTime == 0 {
			t.Error("exit time must be set")
		}
	}

	// idempotent: a second pass changes nothing
	open, _ := st.Position().GetOpenPositions(at.id)
	at.reconcilePaperPositions(p)
	if again, _ := st.Position().GetOpenPositions(at.id); len(again) != len(open) || len(open) != 2 {
		t.Fatalf("open positions: %d then %d, want 2 and 2", len(open), len(again))
	}
}

// TestPaperReconcileWithoutMarkClosesAtEntry: a price outage must not leave the orphan open.
func TestPaperReconcileWithoutMarkClosesAtEntry(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	at, st := newReconcileAutoTrader(t, "paper-reconcile-2", p)
	orphan := createDBPosition(t, st, at.id, "BTCUSDT", "SHORT", 1, 111, 0.2)
	src.mu.Lock()
	src.markErr = errors.New("price feed down")
	src.mu.Unlock()
	at.reconcilePaperPositions(p)
	closed, _ := st.Position().GetClosedPositions(at.id, 10)
	if len(closed) != 1 || closed[0].ID != orphan.ID || closed[0].CloseReason != "paper_state_lost" {
		t.Fatalf("orphan must be closed even without a mark: %+v", closed)
	}
	near(t, "exit at entry", closed[0].ExitPrice, 111)
	near(t, "zero pnl", closed[0].RealizedPnL, 0)
}

func TestPaperReconcileNilStoreIsNoop(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	(&AutoTrader{id: "x", name: "x", exchange: "bitget_paper", trader: p}).reconcilePaperPositions(p)
}

// TestPaperRestartKeepsDBPositionOpen is the production scenario end to end: the AI opens a
// short (paper account + DB record), the backend restarts. With the snapshot the position is
// still in the paper account and the DB record stays OPEN; without it (state lost) the DB record
// is closed as paper_state_lost instead of staying OPEN forever.
func TestPaperRestartKeepsDBPositionOpen(t *testing.T) {
	for _, stateSurvives := range []bool{true, false} {
		name := "state lost"
		if stateSurvives {
			name = "state survives"
		}
		t.Run(name, func(t *testing.T) {
			dir := usePaperStateDir(t)
			src := newFakePaperSource()
			clk := &fakeClock{t: paperMonday}
			const id = "paper-restart-trader"

			p := acquirePaper(id, 10000, src, clk)
			at, st := newReconcileAutoTrader(t, id, p)
			order, err := p.OpenShort("BTCUSDT", 2, 5)
			if err != nil {
				t.Fatal(err)
			}
			at.recordAndConfirmOrder(order, "BTCUSDT", "open_short", 2, 100, 5, 0)
			if open, _ := st.Position().GetOpenPositions(id); len(open) != 1 {
				t.Fatalf("setup: DB must hold the open short, got %d", len(open))
			}

			// restart
			resetPaperRegistryForTest()
			if !stateSurvives {
				if err := os.Remove(filepath.Join(dir, id+".json")); err != nil {
					t.Fatal(err)
				}
			}
			p2 := acquirePaper(id, 10000, src, clk)
			at.trader = p2
			at.reconcilePaperPositions(p2)

			open, _ := st.Position().GetOpenPositions(id)
			held := mustPositions(t, p2)
			if stateSurvives {
				if len(held) != 1 || held[0]["side"] != "short" || len(open) != 1 {
					t.Fatalf("paper positions %d / DB open %d, want 1 / 1", len(held), len(open))
				}
				return
			}
			if len(held) != 0 || len(open) != 0 {
				t.Fatalf("paper positions %d / DB open %d, want 0 / 0", len(held), len(open))
			}
			closed, _ := st.Position().GetClosedPositions(id, 10)
			if len(closed) != 1 || closed[0].CloseReason != "paper_state_lost" {
				t.Fatalf("orphan must be closed as paper_state_lost: %+v", closed)
			}
		})
	}
}

// TestPaperSnapshotHandWrittenFileLoads is the positive control of the bad-file cases: the same
// document with a valid version is accepted, unknown JSON fields are tolerated, and the file stays in place.
func TestPaperSnapshotHandWrittenFileLoads(t *testing.T) {
	dir := usePaperStateDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":1,"trader_id":"t-hand","future_field":{"x":1},"initial_balance":1234,"cash":1000,"next_order_seq":3,
"positions":[{"id":"p1","symbol":"BTCUSDT","side":"long","qty":1,"entry_price":100,"leverage":5,"margin":20,"opened_at":"2026-10-05T12:00:00Z","stop_loss":{"id":"sl1","price":90}}]}`
	path := filepath.Join(dir, "t-hand.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p := acquirePaper("t-hand", 5000, newFakePaperSource(), &fakeClock{t: paperMonday})
	near(t, "restored wallet (cash + margin)", mustFloat(t, balanceOf(t, p), "totalWalletBalance"), 1020)
	pos := mustPositions(t, p)
	if len(pos) != 1 || pos[0]["symbol"] != "BTCUSDT" {
		t.Fatalf("positions: %+v", pos)
	}
	if orders, _ := p.GetOpenOrders(""); len(orders) != 1 || orders[0].StopPrice != 90 {
		t.Fatalf("orders: %+v", orders)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("accepted snapshot must stay in place: %v", err)
	}
	if _, err := os.Stat(path + paperRejectedSuffix); !os.IsNotExist(err) {
		t.Fatal("accepted snapshot must not be moved aside")
	}
}
