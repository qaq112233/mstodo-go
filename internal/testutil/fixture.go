// Package testutil supplies a real temporary SQLite database and a local HTTP
// upstream. Production hostname checks stay enabled; only the test transport
// maps those pinned URLs onto httptest's listener.
package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/todo"
	"golang.org/x/time/rate"
)

type TransportFunc func(*http.Request) (*http.Response, error)

func (f TransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type Clock struct{ ms atomic.Int64 }

func (c *Clock) Now() time.Time      { return time.UnixMilli(c.ms.Load()) }
func (c *Clock) Add(d time.Duration) { c.ms.Add(d.Milliseconds()) }

type Fixture struct {
	Store  *store.Store
	MS     *microsoft.Client
	Tokens *microsoft.Manager
	Todo   *todo.Service
	Epoch  int64
	Clock  *Clock
	Server *httptest.Server
}

func New(t testing.TB, handler http.HandlerFunc) *Fixture {
	t.Helper()
	s, _, err := store.Open(filepath.Join(t.TempDir(), "mstodo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	clock := &Clock{}
	clock.ms.Store(time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC).UnixMilli())
	v, _ := s.Snapshot(context.Background())
	cfg := v.Settings
	cfg.ClientID = "00000000-0000-0000-0000-000000000000"
	cfg.ClientSecret = "test-app-secret"
	cfg.Tenant = "common"
	cfg.ExpectedAccount = "owner@example.test"
	if err = s.SaveSettings(context.Background(), v, cfg); err != nil {
		t.Fatal(err)
	}
	v, _ = s.Snapshot(context.Background())
	a := store.Account{ID: "owner", Email: cfg.ExpectedAccount, Anchor: "OID:owner@tenant", Refresh: "r0", Graph: store.Token{Access: "graph-token", Expires: clock.Now().Add(time.Hour)}, Substrate: store.Token{Access: "exo-token", Expires: clock.Now().Add(time.Hour)}, Maintained: clock.Now()}
	if err = s.InstallAccount(context.Background(), v.Epoch, a); err != nil {
		t.Fatal(err)
	}
	v, _ = s.Snapshot(context.Background())
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client := microsoft.NewClient()
	client.Limiter = rate.NewLimiter(rate.Inf, 2)
	client.HTTP.Transport = TransportFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = target.Scheme
		u.Host = target.Host
		clone.URL = &u
		clone.Host = r.URL.Host
		return transport.RoundTrip(clone)
	})
	tokens := microsoft.NewManager(s, client)
	tokens.Now = clock.Now
	service := todo.New(s, client)
	service.Now = clock.Now
	t.Cleanup(func() { service.Wait(); tokens.Wait() })
	return &Fixture{s, client, tokens, service, v.Epoch, clock, server}
}
