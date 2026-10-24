package mcpserver_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qaq112233/mstodo-go/internal/mcpserver"
	"github.com/qaq112233/mstodo-go/internal/testutil"
)

func TestConfiguredPublicHostBehindLoopbackProxy(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { t.Error("initialization must not call Microsoft") })
	srv := httptest.NewServer(mcpserver.Handler(f.Todo, "https://todo.example.test"))
	defer srv.Close()
	for _, tc := range []struct {
		host   string
		status int
	}{{"todo.example.test", 200}, {"evil.example.test", 403}} {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		req.Host = tc.host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, e := srv.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatal(tc.host, res.StatusCode, string(b))
		}
	}
}
