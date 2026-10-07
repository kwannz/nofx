package trader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"nofx/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Persistence of the bitget_paper account.
//
// The simulated account lives in memory (BitgetPaperTrader), but every state mutation (open,
// close, SL/TP set or cancel, leverage/margin-mode change, funding settlement, SL/TP/liquidation
// fill) is written to <state dir>/<sanitized trader id>.json with an atomic replace (temp file in
// the same directory, fsync, rename, fsync of the directory). AcquireBitgetPaperTrader loads the
// snapshot back, so a process restart no longer wipes balance, positions and SL/TP orders, and the
// positions recorded in the database stay consistent with the paper account.
//
// Writes are synchronous (under the account lock) so the file never lags behind memory: the
// mutation rate is a few per minute at most and a snapshot is a few KB.

const (
	// paperSnapshotVersion 3 adds the position mode (position_mode: "hedge" / "one_way") and
	// allows a long AND a short position of the same symbol (hedge mode). Version 2 stored
	// funding progress as unix timestamps (funding_boundary / last_funding_at); version 1 stored
	// a slot index in units of the contract's fundInterval at open time, which breaks when
	// Bitget changes that interval. Versions 1 and 2 are still readable and migrated on load
	// (see migrate): they predate hedge mode, so a non-empty account stays one-way.
	paperSnapshotVersion    = 3
	paperSnapshotMinVersion = 1
	paperPersistMaxFills    = 500 // recent fills kept for GetOrderStatus after a restart
	paperPersistMaxClosed   = 500 // recent closed-trade records kept for GetClosedPnL
	paperRejectedSuffix     = ".rejected"
)

var (
	paperStateDirMu sync.RWMutex
	paperStateDir   string
)

// SetPaperStateDir sets the directory where bitget_paper account snapshots are stored. An empty
// dir disables persistence (the default, and what unit tests use). main() calls it with
// DefaultPaperStateDir before any trader is loaded.
func SetPaperStateDir(dir string) {
	paperStateDirMu.Lock()
	paperStateDir = dir
	paperStateDirMu.Unlock()
}

// PaperStateDir returns the configured snapshot directory ("" = persistence disabled).
func PaperStateDir() string {
	paperStateDirMu.RLock()
	defer paperStateDirMu.RUnlock()
	return paperStateDir
}

// DefaultPaperStateDir derives the snapshot directory from the database configuration:
// <dir of the SQLite file>/paper, or data/paper for PostgreSQL (no local database directory).
func DefaultPaperStateDir(dbType, dbPath string) string {
	if strings.EqualFold(dbType, "postgres") || strings.TrimSpace(dbPath) == "" {
		return filepath.Join("data", "paper")
	}
	return filepath.Join(filepath.Dir(dbPath), "paper")
}

// paperStateFileName maps a trader ID to a safe file name. IDs that contain characters outside
// [A-Za-z0-9._-] get a short hash suffix so two different IDs can never share a file.
func paperStateFileName(traderID string) string {
	var b strings.Builder
	changed := false
	for _, r := range traderID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	name := b.String()
	if trimmed := strings.TrimLeft(name, "."); trimmed != name { // no hidden files, no "." / ".."
		name, changed = trimmed, true
	}
	if name == "" {
		name, changed = "trader", true
	}
	if len(name) > 120 {
		name = name[:120]
		changed = true
	}
	if changed {
		sum := sha256.Sum256([]byte(traderID))
		name += "-" + hex.EncodeToString(sum[:4])
	}
	return name + ".json"
}

func paperStatePath(dir, traderID string) string {
	return filepath.Join(dir, paperStateFileName(traderID))
}

// ------------------------------------------------------------------ snapshot format

type paperSnapshot struct {
	Version         int                 `json:"version"`
	TraderID        string              `json:"trader_id"`
	SavedAt         time.Time           `json:"saved_at"`
	Initial         float64             `json:"initial_balance"`
	Cash            float64             `json:"cash"`
	RealizedPnL     float64             `json:"realized_pnl"` // cumulative gross price PnL of closing fills
	Seq             uint64              `json:"next_order_seq"`
	PositionMode    string              `json:"position_mode,omitempty"` // v3+: "hedge" / "one_way"
	LastFundingAt   int64               `json:"last_funding_at"`
	LastFundingSlot int64               `json:"last_funding_slot,omitempty"` // v1 only (index), read for migration
	Positions       []paperPositionSnap `json:"positions"`
	Leverage        map[string]int      `json:"leverage"`
	CrossMode       map[string]bool     `json:"cross_mode"`
	Fills           []paperFillSnap     `json:"fills"`
	Closed          []paperClosedSnap   `json:"closed_trades"`
}

type paperPositionSnap struct {
	ID       string    `json:"id"`
	Symbol   string    `json:"symbol"`
	Side     string    `json:"side"`
	Qty      float64   `json:"qty"`
	Entry    float64   `json:"entry_price"`
	Leverage int       `json:"leverage"`
	Margin   float64   `json:"margin"`
	OpenFee  float64   `json:"open_fee"`
	Funding  float64   `json:"funding"`
	OpenedAt time.Time `json:"opened_at"`
	// FundingBoundary is the unix time (seconds) funding has been settled through (v2).
	FundingBoundary int64 `json:"funding_boundary"`
	// FundingSlot is the v1 slot index; it is only read when migrating a v1 snapshot.
	FundingSlot int64        `json:"funding_slot,omitempty"`
	SL          *paperTrigSn `json:"stop_loss,omitempty"`
	TP          *paperTrigSn `json:"take_profit,omitempty"`
}

type paperTrigSn struct {
	ID    string  `json:"id"`
	Price float64 `json:"price"`
}

type paperFillSnap struct {
	OrderID      string    `json:"order_id"`
	Symbol       string    `json:"symbol"`
	Action       string    `json:"action"`
	PositionSide string    `json:"position_side"`
	Side         string    `json:"side"`
	Price        float64   `json:"price"`
	Quantity     float64   `json:"quantity"`
	Fee          float64   `json:"fee"`
	RealizedPnL  float64   `json:"realized_pnl"`
	EntryPrice   float64   `json:"entry_price"`
	Leverage     int       `json:"leverage"`
	Reason       string    `json:"reason"`
	Time         time.Time `json:"time"`
}

type paperClosedSnap struct {
	Symbol      string    `json:"symbol"`
	Side        string    `json:"side"`
	EntryPrice  float64   `json:"entry_price"`
	ExitPrice   float64   `json:"exit_price"`
	Quantity    float64   `json:"quantity"`
	RealizedPnL float64   `json:"realized_pnl"`
	Fee         float64   `json:"fee"`
	Leverage    int       `json:"leverage"`
	EntryTime   time.Time `json:"entry_time"`
	ExitTime    time.Time `json:"exit_time"`
	OrderID     string    `json:"order_id"`
	CloseType   string    `json:"close_type"`
	ExchangeID  string    `json:"exchange_id"`
}

func trigToSnap(tr *paperTrigger) *paperTrigSn {
	if tr == nil {
		return nil
	}
	return &paperTrigSn{ID: tr.id, Price: tr.price}
}

func snapToTrig(s *paperTrigSn) *paperTrigger {
	if s == nil {
		return nil
	}
	return &paperTrigger{id: s.ID, price: s.Price}
}

// snapshotLocked captures the account state. t.mu must be held.
func (t *BitgetPaperTrader) snapshotLocked() *paperSnapshot {
	s := &paperSnapshot{
		Version:       paperSnapshotVersion,
		TraderID:      t.traderID,
		SavedAt:       t.now().UTC(),
		Initial:       t.initial,
		Cash:          t.cash,
		RealizedPnL:   t.realized,
		Seq:           t.seq,
		PositionMode:  t.posMode,
		LastFundingAt: t.lastFundingAt,
		Positions:     make([]paperPositionSnap, 0, len(t.positions)),
		Leverage:      make(map[string]int, len(t.leverage)),
		CrossMode:     make(map[string]bool, len(t.crossMode)),
	}
	for _, p := range t.sortedPositionsLocked() {
		s.Positions = append(s.Positions, paperPositionSnap{
			ID: p.id, Symbol: p.symbol, Side: p.side, Qty: p.qty, Entry: p.entry,
			Leverage: p.leverage, Margin: p.margin, OpenFee: p.openFee, Funding: p.funding,
			OpenedAt: p.openedAt, FundingBoundary: p.fundedThrough,
			SL: trigToSnap(p.sl), TP: trigToSnap(p.tp),
		})
	}
	for k, v := range t.leverage {
		s.Leverage[k] = v
	}
	for k, v := range t.crossMode {
		s.CrossMode[k] = v
	}
	fills := t.fills
	if len(fills) > paperPersistMaxFills {
		fills = fills[len(fills)-paperPersistMaxFills:]
	}
	s.Fills = make([]paperFillSnap, 0, len(fills))
	for _, f := range fills {
		s.Fills = append(s.Fills, paperFillSnap{
			OrderID: f.OrderID, Symbol: f.Symbol, Action: f.Action, PositionSide: f.PositionSide,
			Side: f.Side, Price: f.Price, Quantity: f.Quantity, Fee: f.Fee, RealizedPnL: f.RealizedPnL,
			EntryPrice: f.EntryPrice, Leverage: f.Leverage, Reason: f.Reason, Time: f.Time,
		})
	}
	closed := t.closed
	if len(closed) > paperPersistMaxClosed {
		closed = closed[len(closed)-paperPersistMaxClosed:]
	}
	s.Closed = make([]paperClosedSnap, 0, len(closed))
	for _, r := range closed {
		s.Closed = append(s.Closed, paperClosedSnap{
			Symbol: r.Symbol, Side: r.Side, EntryPrice: r.EntryPrice, ExitPrice: r.ExitPrice,
			Quantity: r.Quantity, RealizedPnL: r.RealizedPnL, Fee: r.Fee, Leverage: r.Leverage,
			EntryTime: r.EntryTime, ExitTime: r.ExitTime, OrderID: r.OrderID,
			CloseType: r.CloseType, ExchangeID: r.ExchangeID,
		})
	}
	return s
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// validate rejects snapshots that would put the account into an impossible state.
func (s *paperSnapshot) validate(traderID string) error {
	if s.Version < paperSnapshotMinVersion || s.Version > paperSnapshotVersion {
		return fmt.Errorf("unsupported snapshot version %d (this build reads versions %d-%d)", s.Version, paperSnapshotMinVersion, paperSnapshotVersion)
	}
	if s.TraderID != "" && s.TraderID != traderID {
		return fmt.Errorf("snapshot belongs to trader %q", s.TraderID)
	}
	if !finite(s.Initial, s.Cash, s.RealizedPnL) || s.Initial <= 0 || s.Cash < -1e-6 {
		return fmt.Errorf("invalid balance (initial %v, cash %v)", s.Initial, s.Cash)
	}
	// Hedge mode (and every pre-v3 snapshot's one-way mode) allows at most one position per
	// symbol AND side; one-way mode at most one per symbol.
	oneWay := s.Version < 3 || NormalizeBitgetPositionMode(s.PositionMode) == BitgetPositionModeOneWay
	seen := map[string]bool{}
	for _, p := range s.Positions {
		if p.Side != "long" && p.Side != "short" {
			return fmt.Errorf("position %s: invalid side %q", p.Symbol, p.Side)
		}
		key := paperPosKey(p.Symbol, p.Side)
		if oneWay {
			key = p.Symbol
		}
		if p.Symbol == "" || seen[key] {
			return fmt.Errorf("invalid or duplicate position %q (%s)", p.Symbol, p.Side)
		}
		seen[key] = true
		// Margin may legitimately be negative: funding is charged against the position margin and
		// can exceed it while unrealized PnL still keeps the position above liquidation. Only
		// NaN/Inf are rejected (finite check below).
		if !validPrice(p.Qty) || !validPrice(p.Entry) || !finite(p.Margin, p.OpenFee, p.Funding) || p.Leverage < 1 {
			return fmt.Errorf("position %s: invalid numbers (qty %v entry %v margin %v lev %d)", p.Symbol, p.Qty, p.Entry, p.Margin, p.Leverage)
		}
		for _, tr := range []*paperTrigSn{p.SL, p.TP} {
			if tr != nil && !validPrice(tr.Price) {
				return fmt.Errorf("position %s: invalid trigger price %v", p.Symbol, tr.Price)
			}
		}
	}
	return nil
}

// migrate upgrades an older snapshot in place to the current version.
//
// v1 -> v2: v1 kept per position a funding slot index in units of the contract's fundInterval AT
// OPEN TIME. That interval is not recorded, so the index cannot be turned into a time reliably
// (guessing 8h would put a 4h contract's marker far in the future). Instead the per-position
// marker is reset to the time the snapshot was written: funding is considered settled up to
// that moment (the matcher settles every few seconds and persists each settlement, so this is
// exact in the normal case) and the boundaries that passed since are settled on the first tick
// with the contract's CURRENT interval. At worst a boundary that was still pending (rate
// unavailable) when the v1 snapshot was written is not paid. The marker is clamped to
// [position open time, now].
//
// v2 -> v3: hedge mode did not exist, every account was one-way. An account that holds
// positions keeps the one-way mode it was opened in (behaviour preserved: no opposite-side
// open, one position per symbol) until it is flat and the configured mode is applied; an empty
// account has nothing to preserve and takes the default (hedge).
func (s *paperSnapshot) migrate(now time.Time) {
	if s.Version < 2 {
		saved := s.SavedAt.UTC().Unix()
		if s.SavedAt.IsZero() || saved > now.UTC().Unix() {
			saved = now.UTC().Unix()
		}
		var latest int64
		for i := range s.Positions {
			p := &s.Positions[i]
			through := saved
			if opened := p.OpenedAt.UTC().Unix(); !p.OpenedAt.IsZero() && through < opened {
				through = opened
			}
			p.FundingBoundary, p.FundingSlot = through, 0
			if through > latest {
				latest = through
			}
		}
		s.LastFundingAt, s.LastFundingSlot = latest, 0
		s.Version = 2
	}
	if s.Version < 3 {
		s.PositionMode = BitgetPositionModeHedge
		if len(s.Positions) > 0 {
			s.PositionMode = BitgetPositionModeOneWay
		}
		s.Version = 3
	}
}

// applySnapshotLocked replaces the account state with s. t.mu must be held; s must be valid.
func (t *BitgetPaperTrader) applySnapshotLocked(s *paperSnapshot) {
	t.initial = s.Initial
	t.cash = s.Cash
	t.realized = s.RealizedPnL
	t.seq = s.Seq
	t.lastFundingAt = s.LastFundingAt
	t.posMode = NormalizeBitgetPositionMode(s.PositionMode)
	t.positions = make(map[string]*paperPosition, len(s.Positions))
	for _, p := range s.Positions {
		t.positions[paperPosKey(p.Symbol, p.Side)] = &paperPosition{
			id: p.ID, symbol: p.Symbol, side: p.Side, qty: p.Qty, entry: p.Entry,
			leverage: p.Leverage, margin: p.Margin, openFee: p.OpenFee, funding: p.Funding,
			openedAt: p.OpenedAt, fundedThrough: p.FundingBoundary,
			sl: snapToTrig(p.SL), tp: snapToTrig(p.TP),
		}
	}
	t.leverage = make(map[string]int, len(s.Leverage))
	for k, v := range s.Leverage {
		t.leverage[k] = v
	}
	t.crossMode = make(map[string]bool, len(s.CrossMode))
	for k, v := range s.CrossMode {
		t.crossMode[k] = v
	}
	t.fills = make([]*PaperFill, 0, len(s.Fills))
	t.fillIndex = make(map[string]*PaperFill, len(s.Fills))
	for _, f := range s.Fills {
		pf := &PaperFill{
			OrderID: f.OrderID, Symbol: f.Symbol, Action: f.Action, PositionSide: f.PositionSide,
			Side: f.Side, Price: f.Price, Quantity: f.Quantity, Fee: f.Fee, RealizedPnL: f.RealizedPnL,
			EntryPrice: f.EntryPrice, Leverage: f.Leverage, Reason: f.Reason, Time: f.Time,
		}
		t.fills = append(t.fills, pf)
		t.fillIndex[pf.OrderID] = pf
	}
	t.closed = make([]ClosedPnLRecord, 0, len(s.Closed))
	for _, r := range s.Closed {
		t.closed = append(t.closed, ClosedPnLRecord{
			Symbol: r.Symbol, Side: r.Side, EntryPrice: r.EntryPrice, ExitPrice: r.ExitPrice,
			Quantity: r.Quantity, RealizedPnL: r.RealizedPnL, Fee: r.Fee, Leverage: r.Leverage,
			EntryTime: r.EntryTime, ExitTime: r.ExitTime, OrderID: r.OrderID,
			CloseType: r.CloseType, ExchangeID: r.ExchangeID,
		})
	}
}

// ------------------------------------------------------------------ file I/O

// writeFileAtomic writes data to path so that readers see either the old or the new content,
// never a partial file: temp file in the same directory, fsync, rename, fsync of the directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return cause
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil { // best effort: make the rename itself durable
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// persistLocked writes the current state when persistence is enabled. A failed write is logged
// and retried on the next mutation and on Stop; it never fails the trade (already applied in
// memory). t.mu must be held.
func (t *BitgetPaperTrader) persistLocked() {
	if t.statePath == "" {
		return
	}
	data, err := json.MarshalIndent(t.snapshotLocked(), "", " ")
	if err == nil {
		err = writeFileAtomic(t.statePath, data)
	}
	if err != nil {
		t.persistFailed = true
		logger.Warnf("⚠️ [BitgetPaper] failed to persist account of trader %s to %s: %v", t.traderID, t.statePath, err)
		return
	}
	t.persistFailed = false
}

// flushIfFailed retries a previously failed write (called from Stop).
func (t *BitgetPaperTrader) flushIfFailed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.persistFailed {
		t.persistLocked()
	}
}

// disablePersist stops all further writes (the account was deleted).
func (t *BitgetPaperTrader) disablePersist() {
	t.mu.Lock()
	t.statePath = ""
	t.persistFailed = false
	t.mu.Unlock()
}

// attachState binds the account to the snapshot file of traderID and restores the snapshot when
// one exists. Persistence is disabled when no state directory is configured.
func (t *BitgetPaperTrader) attachState(traderID string) {
	dir := PaperStateDir()
	if dir == "" {
		return
	}
	path := paperStatePath(dir, traderID)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.traderID = traderID
	t.statePath = path

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("⚠️ [BitgetPaper] cannot read account snapshot %s, starting a fresh account: %v", path, err)
		}
		return
	}
	var snap paperSnapshot
	err = json.Unmarshal(data, &snap)
	if err == nil {
		err = snap.validate(traderID)
	}
	if err == nil && snap.Version < paperSnapshotVersion {
		from := snap.Version
		snap.migrate(t.now())
		note := "account is one-way as before (hedge applies once it is flat)"
		if len(snap.Positions) == 0 {
			note = "empty account takes the configured position mode"
		}
		if from < 2 {
			note += "; funding markers reset to the save time"
		}
		logger.Infof("♻️ [BitgetPaper] migrated account snapshot of trader %s from v%d to v%d (%s)", traderID, from, snap.Version, note)
	}
	if err != nil {
		// Keep the unreadable file for inspection instead of overwriting it with the next write.
		rejected := path + paperRejectedSuffix
		if rerr := os.Rename(path, rejected); rerr != nil {
			rejected = "(could not be moved aside: " + rerr.Error() + ")"
		}
		logger.Warnf("⚠️ [BitgetPaper] ignoring account snapshot %s: %v; starting a fresh account (file kept as %s)", path, err, rejected)
		return
	}
	t.applySnapshotLocked(&snap)
	logger.Infof("♻️ [BitgetPaper] restored account of trader %s from %s: cash %.4f, %s mode, %d open positions, %d recent fills, %d closed trades (saved %s)",
		traderID, path, t.cash, t.posMode, len(t.positions), len(t.fills), len(t.closed), snap.SavedAt.Format(time.RFC3339))
}

// removePaperSnapshot deletes the snapshot of traderID (and any rejected or temp leftovers).
func removePaperSnapshot(traderID string) {
	dir := PaperStateDir()
	if dir == "" || traderID == "" {
		return
	}
	path := paperStatePath(dir, traderID)
	for _, p := range []string{path, path + paperRejectedSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("⚠️ [BitgetPaper] failed to delete account snapshot %s: %v", p, err)
		}
	}
	if entries, err := os.ReadDir(dir); err == nil {
		prefix := filepath.Base(path) + ".tmp-"
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), prefix) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
