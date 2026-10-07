package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nofx/manager"
	"nofx/store"

	"github.com/gin-gonic/gin"
)

// The per-exchange "bitget_position_mode" setting: create / update / list through the HTTP
// handlers (plain JSON, i.e. transport encryption off).

func newPositionModeTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("TRANSPORT_ENCRYPTION", "false")
	gin.SetMode(gin.TestMode)
	st, err := store.New(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: st, traderManager: manager.NewTraderManager()}
}

func callHandler(t *testing.T, h gin.HandlerFunc, body string) (int, map[string]interface{}) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", "u1")
	h(c)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func listedPositionMode(t *testing.T, s *Server, id string) string {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("user_id", "u1")
	s.handleGetExchangeConfigs(c)
	var list []SafeExchangeConfig
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	for _, e := range list {
		if e.ID == id {
			return e.BitgetPositionMode
		}
	}
	t.Fatalf("exchange %s not listed: %s", id, w.Body.String())
	return ""
}

func TestExchangeAPIBitgetPositionMode(t *testing.T) {
	s := newPositionModeTestServer(t)

	create := func(body string) (int, string) {
		code, out := callHandler(t, s.handleCreateExchange, body)
		id, _ := out["id"].(string)
		return code, id
	}

	// default (field omitted) is hedge
	code, idDefault := create(`{"exchange_type":"bitget","account_name":"a","enabled":true,"api_key":"k","secret_key":"s","passphrase":"p"}`)
	if code != http.StatusOK || idDefault == "" {
		t.Fatalf("create default: %d", code)
	}
	if got := listedPositionMode(t, s, idDefault); got != "hedge" {
		t.Errorf("default mode = %q, want hedge", got)
	}

	// explicit one_way, also for bitget_paper
	code, idOneWay := create(`{"exchange_type":"bitget","account_name":"b","enabled":true,"api_key":"k","secret_key":"s","passphrase":"p","bitget_position_mode":"one_way"}`)
	if code != http.StatusOK {
		t.Fatalf("create one_way: %d", code)
	}
	if got := listedPositionMode(t, s, idOneWay); got != "one_way" {
		t.Errorf("mode = %q, want one_way", got)
	}
	code, idPaper := create(`{"exchange_type":"bitget_paper","account_name":"p","enabled":true,"bitget_position_mode":"one_way"}`)
	if code != http.StatusOK || listedPositionMode(t, s, idPaper) != "one_way" {
		t.Errorf("bitget_paper one_way: code %d", code)
	}

	// invalid value is rejected and nothing is created
	before, _ := s.store.Exchange().List("u1")
	if code, _ := create(`{"exchange_type":"bitget","account_name":"c","enabled":true,"bitget_position_mode":"both"}`); code != http.StatusBadRequest {
		t.Errorf("invalid mode on create: %d, want 400", code)
	}
	if after, _ := s.store.Exchange().List("u1"); len(after) != len(before) {
		t.Errorf("a rejected create must not insert a row")
	}

	update := func(id, mode string) int {
		field := ""
		if mode != "" {
			field = `,"bitget_position_mode":"` + mode + `"`
		}
		code, _ := callHandler(t, s.handleUpdateExchangeConfigs,
			`{"exchanges":{"`+id+`":{"enabled":true,"api_key":"","secret_key":"","passphrase":""`+field+`}}}`)
		return code
	}
	if update(idOneWay, "") != http.StatusOK || listedPositionMode(t, s, idOneWay) != "one_way" {
		t.Errorf("an update without the field must keep one_way")
	}
	if update(idOneWay, "hedge") != http.StatusOK || listedPositionMode(t, s, idOneWay) != "hedge" {
		t.Errorf("update to hedge")
	}
	if update(idOneWay, "bogus") != http.StatusBadRequest || listedPositionMode(t, s, idOneWay) != "hedge" {
		t.Errorf("an invalid update must be rejected and change nothing")
	}
}
