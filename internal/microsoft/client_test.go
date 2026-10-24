package microsoft_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/testutil"
)

func token(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "access_token": access, "refresh_token": refresh, "expires_in": 3600})
}
func expire(t *testing.T, f *testutil.Fixture) {
	t.Helper()
	a, _ := f.Store.Account(context.Background(), f.Epoch)
	a.Graph.Expires = f.Clock.Now()
	a.Substrate.Expires = f.Clock.Now()
	if e := f.Store.SaveAccount(context.Background(), f.Epoch, a); e != nil {
		t.Fatal(e)
	}
}
func TestResourceRefreshSerializesRotationAndCoalesces(t *testing.T) {
	var mu sync.Mutex
	refreshes := []string{}
	entered := atomic.Int32{}
	maxConcurrent := atomic.Int32{}
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		active := entered.Add(1)
		defer entered.Add(-1)
		maxConcurrent.CompareAndSwap(0, active)
		if r.Host != "login.microsoftonline.com" {
			t.Error("unexpected host", r.Host)
		}
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		n := len(refreshes)
		want := fmt.Sprintf("r%d", n)
		if r.Form.Get("refresh_token") != want {
			t.Errorf("rotation chain got %q want %q", r.Form.Get("refresh_token"), want)
		}
		refreshes = append(refreshes, r.Form.Get("scope"))
		token(w, "access-"+r.Form.Get("scope"), fmt.Sprintf("r%d", n+1))
	})
	expire(t, f)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		resource := "graph"
		if i%2 == 0 {
			resource = "substrate"
		}
		go func() {
			defer wg.Done()
			_, _, e := f.Tokens.Get(context.Background(), f.Epoch, resource, "")
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	n := len(refreshes)
	mu.Unlock()
	if n != 2 || maxConcurrent.Load() != 1 {
		t.Fatal("refresh not coalesced/serialized", n, maxConcurrent.Load())
	}
	a, _ := f.Store.Account(context.Background(), f.Epoch)
	if a.Refresh != "r2" || a.Graph.Access == "" || a.Substrate.Access == "" {
		t.Fatal("rotation was not persisted")
	}
}
func TestCancelledWaiterStillPersistsRotatedRefreshToken(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-release; token(w, "new-graph", "r1") })
	expire(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, e := f.Tokens.Get(ctx, f.Epoch, "graph", ""); done <- e }()
	<-started
	cancel()
	if e := <-done; e != context.Canceled {
		t.Fatal(e)
	}
	close(release)
	if _, _, e := f.Tokens.Get(context.Background(), f.Epoch, "graph", ""); e != nil {
		t.Fatal(e)
	}
	a, _ := f.Store.Account(context.Background(), f.Epoch)
	if a.Refresh != "r1" {
		t.Fatal("rotated token lost")
	}
}
func TestOldRefreshCannotOverwriteNewAuthorization(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		token(w, "old-account-token", "r1")
	})
	expire(t, f)
	done := make(chan error, 1)
	go func() { _, _, e := f.Tokens.Get(context.Background(), f.Epoch, "graph", ""); done <- e }()
	<-started
	if err := f.Store.InstallAccount(context.Background(), f.Epoch, store.Account{ID: "new", Refresh: "new-r"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if e := <-done; e != store.ErrChanged {
		t.Fatal("old refresh accepted", e)
	}
	v, _ := f.Store.Snapshot(context.Background())
	a, _ := f.Store.Account(context.Background(), v.Epoch)
	if a.Refresh != "new-r" {
		t.Fatal("new token overwritten")
	}
}
func Test401RefreshesOnlyOnce(t *testing.T) {
	var requests, refreshes atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "login.microsoftonline.com" {
			refreshes.Add(1)
			token(w, "new-graph", "r1")
			return
		}
		n := requests.Add(1)
		if n == 1 {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-graph" {
			t.Error("old auth reused")
		}
		fmt.Fprint(w, `{"id":"L"}`)
	})
	if _, e := f.MS.Do(context.Background(), f.Epoch, "graph", "GET", microsoft.GraphBase+"/me/todo/lists/L", nil, ""); e != nil {
		t.Fatal(e)
	}
	if requests.Load() != 2 || refreshes.Load() != 1 {
		t.Fatal("wrong retry count")
	}
}
func TestRetryAfterDoesNotRetryEarlyOrExceedDeadline(t *testing.T) {
	var calls atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for i := 0; i < 2; i++ {
		_, e := f.MS.Do(ctx, f.Epoch, "substrate", "GET", microsoft.SubstrateBase+"/myDayFeed/sections/tasks?date=2026-10-24", nil, "")
		err := microsoft.AsError(e)
		if err.Status != 429 || err.RetryAfter < 29 {
			t.Fatalf("wrong throttle error: %+v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("retried before cooldown", calls.Load())
	}
}
func TestHostPinsAndRedirectsNeverForwardCredentials(t *testing.T) {
	var calls atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "graph.microsoft.com" {
			t.Error("credentials forwarded", r.Host)
		}
		w.Header().Set("Location", "https://evil.example/stolen")
		w.WriteHeader(302)
	})
	for _, raw := range []string{"http://graph.microsoft.com/v1.0/me", "https://graph.microsoft.com:444/v1.0/me", "https://user@graph.microsoft.com/v1.0/me", "https://evil.example/me"} {
		if _, e := f.MS.Do(context.Background(), f.Epoch, "graph", "GET", raw, nil, ""); microsoft.AsError(e).Code != "upstream_url_rejected" {
			t.Fatal(raw, e)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("pin checked too late")
	}
	if _, e := f.MS.Do(context.Background(), f.Epoch, "graph", "GET", microsoft.GraphBase+"/me", nil, ""); e == nil {
		t.Fatal("redirect accepted")
	}
	if calls.Load() > 2 {
		t.Fatal("redirect loop or leak")
	}
}
func TestUnconfirmedWriteNeverAutomaticallyRepeats(t *testing.T) {
	for _, scenario := range []string{"bad_json", "server_error", "stalled_body"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "bad_json":
					fmt.Fprint(w, "broken")
				case "server_error":
					w.WriteHeader(500)
				case "stalled_body":
					w.WriteHeader(201)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, e := f.MS.Do(ctx, f.Epoch, "graph", "POST", microsoft.GraphBase+"/me/todo/lists/L/tasks", map[string]any{"title": "test"}, "")
			err := microsoft.AsError(e)
			if !err.Unknown || err.Code != "write_outcome_unknown" || calls.Load() != 1 {
				t.Fatalf("unsafe write retry: %+v, calls=%d", err, calls.Load())
			}
		})
	}
}
func TestReauthorizationAndConsentArePersistent(t *testing.T) {
	for _, tc := range []struct{ resource, body, code string }{{"graph", `{"error":"invalid_grant"}`, "microsoft_reauth_required"}, {"substrate", `{"error":"invalid_grant","error_codes":[65001]}`, "my_day_unavailable"}} {
		t.Run(tc.resource, func(t *testing.T) {
			var n atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(400); fmt.Fprint(w, tc.body) })
			expire(t, f)
			for i := 0; i < 2; i++ {
				_, _, e := f.Tokens.Get(context.Background(), f.Epoch, tc.resource, "")
				if microsoft.AsError(e).Code != tc.code {
					t.Fatal(e)
				}
			}
			if n.Load() != 1 {
				t.Fatal("reauth latch retried")
			}
		})
	}
}
func TestMaintenanceSevenDaysAndFailureCooldown(t *testing.T) {
	var n atomic.Int32
	var fail atomic.Bool
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		token(w, "maintained", "rotated")
	})
	f.Clock.Add(7*24*time.Hour - time.Millisecond)
	if e := f.Tokens.Maintenance(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n.Load() != 0 {
		t.Fatal("maintenance early")
	}
	f.Clock.Add(time.Millisecond)
	fail.Store(true)
	if e := f.Tokens.Maintenance(context.Background()); e == nil {
		t.Fatal("maintenance error swallowed")
	}
	if n.Load() != 1 {
		t.Fatal(n.Load())
	}
	f.Clock.Add(24*time.Hour - time.Millisecond)
	_ = f.Tokens.Maintenance(context.Background())
	if n.Load() != 1 {
		t.Fatal("retry before 24h")
	}
	f.Clock.Add(time.Millisecond)
	fail.Store(false)
	if e := f.Tokens.Maintenance(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n.Load() != 3 {
		t.Fatal("resources not both maintained", n.Load())
	}
	a, _ := f.Store.Account(context.Background(), f.Epoch)
	if !a.Maintained.Equal(f.Clock.Now()) || a.MaintenanceError {
		t.Fatal("maintenance state not committed")
	}
}
func TestResource403HasShortCooldown(t *testing.T) {
	var n atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(403) })
	for i := 0; i < 2; i++ {
		_, e := f.MS.Do(context.Background(), f.Epoch, "substrate", "GET", microsoft.SubstrateBase+"/taskfolders", nil, "")
		if microsoft.AsError(e).Code != "my_day_unavailable" {
			t.Fatal(e)
		}
	}
	a, _ := f.Store.Account(context.Background(), f.Epoch)
	if a.SubstrateConsent || a.Reauth || n.Load() != 1 {
		t.Fatal("ordinary 403 latched forever")
	}
}
