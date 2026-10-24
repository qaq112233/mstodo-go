package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qaq112233/mstodo-go/internal/app"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/testutil"
)

func endpoint(t *testing.T, f *testutil.Fixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(app.Handler(f.Store, f.Tokens, f.Todo, "http://localhost:8080"))
	t.Cleanup(srv.Close)
	return srv
}
func request(t *testing.T, client *http.Client, method, raw, auth, body string) *http.Response {
	t.Helper()
	r, e := http.NewRequest(method, raw, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	res, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}
func config(t *testing.T, s *store.Store, mode string) store.Snapshot {
	t.Helper()
	v, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	cfg := v.Settings
	cfg.AuthMode = mode
	if e = s.SaveSettings(context.Background(), v, cfg); e != nil {
		t.Fatal(e)
	}
	v, e = s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestRESTModesHeadersAndAdminIsolation(t *testing.T) {
	for _, mode := range []string{"none", "bearer", "path"} {
		t.Run(mode, func(t *testing.T) {
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "graph.microsoft.com" {
					io.WriteString(w, `{"value":[]}`)
				} else {
					io.WriteString(w, `{"Value":[]}`)
				}
			})
			v := config(t, f.Store, mode)
			front := endpoint(t, f)
			base := front.URL
			auth := ""
			if mode == "path" {
				base += "/s/" + v.Settings.PathSecret
			}
			if mode == "bearer" {
				auth = v.Settings.Bearer
			}
			health := request(t, front.Client(), "GET", front.URL+"/health", "", "")
			if health.StatusCode != 200 {
				t.Fatal(health.StatusCode)
			}
			res := request(t, front.Client(), "GET", base+"/api/v1/myday", auth, "")
			if res.StatusCode != 200 || res.Header.Get("X-Cache") != "MISS" || res.Header.Get("X-Cache-TTL") != "180" || res.Header.Get("Cache-Control") != "no-store" {
				b, _ := io.ReadAll(res.Body)
				t.Fatal(res.StatusCode, res.Header, string(b))
			}
			res = request(t, front.Client(), "GET", base+"/api/v1/myday?includeCompleted=false", auth, "")
			if res.Header.Get("X-Cache") != "HIT" {
				t.Fatal("REST snapshot not reused")
			}
			for _, tc := range []struct {
				method, path string
				status       int
			}{{"GET", "/api/v1/myday?date=2026-02-30", 400}, {"GET", "/api/v1/myday?refresh=TRUE", 400}, {"GET", "/api/v1/not-real", 404}, {"PATCH", "/api/v1/myday", 405}} {
				res = request(t, front.Client(), tc.method, base+tc.path, auth, "")
				if res.StatusCode != tc.status || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
					t.Fatal(tc, res.StatusCode, res.Header)
				}
			}
			if mode == "bearer" {
				if r := request(t, front.Client(), "GET", front.URL+"/api/v1/status", "wrong", ""); r.StatusCode != 401 {
					t.Fatal("bad bearer accepted")
				}
			}
			if mode == "path" {
				if r := request(t, front.Client(), "GET", front.URL+"/api/v1/status", "", ""); r.StatusCode != 404 {
					t.Fatal("plain route present in path mode")
				}
			}
			if r := request(t, front.Client(), "GET", front.URL+"/admin/wrong", "", ""); r.StatusCode != 404 {
				t.Fatal("admin opened")
			}
			if r := request(t, front.Client(), "GET", front.URL+"/admin/"+v.Settings.AdminSecret, "", ""); r.StatusCode != 200 {
				t.Fatal("admin secret link failed", r.StatusCode)
			}
		})
	}
}
func TestMCPAndRESTShareCacheInEveryAuthMode(t *testing.T) {
	for _, mode := range []string{"bearer", "path", "none"} {
		t.Run(mode, func(t *testing.T) {
			var feeds atomic.Int32
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "graph.microsoft.com" {
					io.WriteString(w, `{"value":[]}`)
				} else {
					feeds.Add(1)
					io.WriteString(w, `{"Value":[{"Id":"a","ParentFolderId":"L","CommittedDay":null,"Status":"Completed"}]}`)
				}
			})
			v := config(t, f.Store, mode)
			front := endpoint(t, f)
			base := front.URL
			auth := ""
			if mode == "path" {
				base += "/s/" + v.Settings.PathSecret
			}
			if mode == "bearer" {
				auth = v.Settings.Bearer
			}
			rest := request(t, front.Client(), "GET", base+"/api/v1/myday", auth, "")
			if rest.StatusCode != 200 {
				t.Fatal(rest.StatusCode)
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			clientHTTP := &http.Client{Transport: testutil.TransportFunc(func(r *http.Request) (*http.Response, error) {
				r = r.Clone(r.Context())
				if auth != "" {
					r.Header.Set("Authorization", "Bearer "+auth)
				}
				return transport.RoundTrip(r)
			})}
			client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
			session, e := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: clientHTTP}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			tools, e := session.ListTools(context.Background(), nil)
			if e != nil {
				t.Fatal(e)
			}
			if len(tools.Tools) != 32 {
				t.Fatal("missing tool coverage", len(tools.Tools))
			}
			for _, tool := range tools.Tools {
				if tool.Annotations == nil || tool.InputSchema == nil {
					t.Fatal("missing schema or annotations", tool.Name)
				}
			}
			result, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_my_day_tasks", Arguments: map[string]any{}})
			if e != nil || result.IsError {
				t.Fatal(result, e)
			}
			if feeds.Load() != 1 {
				t.Fatal("MCP did not reuse REST cache", feeds.Load())
			}
			data, _ := json.Marshal(result.StructuredContent)
			if !bytes.Contains(data, []byte(`"count":1`)) {
				t.Fatal(string(data))
			}
			failed, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_my_day_tasks", Arguments: map[string]any{"date": "2026-02-30"}})
			if e != nil || !failed.IsError {
				t.Fatal("business error disguised as success", failed, e)
			}
		})
	}
}
func TestWebCSRFAndOneShotOAuthOwnerGate(t *testing.T) {
	for _, match := range []bool{true, false} {
		t.Run(map[bool]string{true: "owner", false: "wrong_owner"}[match], func(t *testing.T) {
			claims := base64.RawURLEncoding.EncodeToString([]byte(`{"oid":"test-oid","tid":"test-tid"}`))
			f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "login.microsoftonline.com" {
					_ = r.ParseForm()
					if r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "test-app-secret" || r.Form.Get("redirect_uri") != "http://localhost:8080/auth/microsoft/callback" {
						t.Error("bad token exchange")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "e30." + claims + ".test", "refresh_token": "new-refresh", "expires_in": 3600, "token_type": "Bearer"})
					return
				}
				email := "owner@example.test"
				if !match {
					email = "wrong@example.test"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "verified-owner", "mail": email, "displayName": "Test"})
			})
			front := endpoint(t, f)
			client := front.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			v, _ := f.Store.Snapshot(context.Background())
			admin := front.URL + "/admin/" + v.Settings.AdminSecret
			res := request(t, client, "POST", admin+"/connect", "", "")
			if res.StatusCode != 403 {
				t.Fatal("CSRF absent accepted")
			}
			form := url.Values{"csrf": {v.Settings.CSRF}}
			req, _ := http.NewRequest("POST", admin+"/connect", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "https://evil.example")
			res, e := client.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != 403 {
				t.Fatal("foreign origin accepted")
			}
			req, _ = http.NewRequest("POST", admin+"/connect", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "http://localhost:8080")
			res, e = client.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != 303 {
				t.Fatal("connect failed", res.StatusCode)
			}
			authURL, e := url.Parse(res.Header.Get("Location"))
			if e != nil {
				t.Fatal(e)
			}
			if authURL.Query().Get("code_challenge_method") != "S256" || authURL.Query().Get("scope") != "Tasks.ReadWrite offline_access User.Read https://outlook.office.com/Tasks.ReadWrite" {
				t.Fatal("PKCE/scopes")
			}
			cookies := res.Cookies()
			if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatal("OAuth browser binding")
			}
			callback := front.URL + "/auth/microsoft/callback?state=" + authURL.Query().Get("state") + "&code=code"
			noCookie := request(t, client, "GET", callback, "", "")
			if noCookie.StatusCode != 400 {
				t.Fatal("unbound callback accepted")
			}
			req, _ = http.NewRequest("GET", callback, nil)
			req.AddCookie(cookies[0])
			res, e = client.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if match && res.StatusCode != 303 || !match && res.StatusCode != 403 {
				t.Fatal("owner gate", res.StatusCode)
			}
			now, _ := f.Store.Snapshot(context.Background())
			a, e := f.Store.Account(context.Background(), now.Epoch)
			if e != nil {
				t.Fatal(e)
			}
			if match && (a.ID != "verified-owner" || a.Refresh != "new-refresh" || a.Anchor != "OID:test-oid@test-tid") {
				t.Fatal("identity not installed")
			}
			if !match && a.ID != "owner" {
				t.Fatal("wrong account persisted")
			}
			req, _ = http.NewRequest("GET", callback, nil)
			req.AddCookie(cookies[0])
			res, e = client.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != 400 {
				t.Fatal("callback reused")
			}
		})
	}
}
func TestConfirmedCreateReturnsWarningWhenCacheWriteFails(t *testing.T) {
	var created atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		created.Add(1)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"new","title":"created"}`)
	})
	_, e := f.Store.DB.Exec(`CREATE TRIGGER fail_cache BEFORE INSERT ON tasks BEGIN SELECT RAISE(FAIL,'cache failure'); END;`)
	if e != nil {
		t.Fatal(e)
	}
	front := endpoint(t, f)
	v, _ := f.Store.Snapshot(context.Background())
	res := request(t, front.Client(), "POST", front.URL+"/api/v1/lists/L/tasks", v.Settings.Bearer, `{"title":"created"}`)
	if res.StatusCode != 201 || res.Header.Get("X-Cache-Warning") == "" || created.Load() != 1 {
		t.Fatal("confirmed write reported failure", res.StatusCode, res.Header)
	}
}

func TestNativeNoReferrerFormStillRequiresCSRF(t *testing.T) {
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("local authorization start must not request Microsoft")
	})
	front := endpoint(t, f)
	client := front.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v, _ := f.Store.Snapshot(context.Background())
	for _, tc := range []struct {
		name, site, csrf string
		status           int
	}{
		{"valid same-origin form", "same-origin", v.Settings.CSRF, 303},
		{"missing CSRF", "same-origin", "", 403},
		{"wrong CSRF", "same-origin", "wrong", 403},
		{"cross-site with valid CSRF", "cross-site", v.Settings.CSRF, 403},
		{"missing browser metadata", "", v.Settings.CSRF, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := url.Values{"csrf": {tc.csrf}}
			req, _ := http.NewRequest("POST", front.URL+"/admin/"+v.Settings.AdminSecret+"/connect", strings.NewReader(body.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "null")
			req.Header.Set("Sec-Fetch-Site", tc.site)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatal("unexpected form result", res.StatusCode, tc.status)
			}
		})
	}
}

func TestAdminRotationInvalidatesPendingOAuth(t *testing.T) {
	var upstream atomic.Int32
	f := testutil.New(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		if r.Host == "login.microsoftonline.com" {
			io.WriteString(w, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`)
		} else {
			io.WriteString(w, `{"id":"owner","mail":"owner@example.test"}`)
		}
	})
	front := endpoint(t, f)
	client := front.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v, err := f.Store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	post := func(action string, form url.Values) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", front.URL+"/admin/"+v.Settings.AdminSecret+"/"+action, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d", action, res.StatusCode)
		}
		return res
	}
	flow := post("connect", url.Values{"csrf": {v.Settings.CSRF}})
	authorize, err := url.Parse(flow.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := post("rotate", url.Values{"csrf": {v.Settings.CSRF}, "target": {"admin"}})
	if rotated.Header.Get("Location") == "/admin/"+v.Settings.AdminSecret {
		t.Fatal("admin credential did not rotate")
	}
	req, err := http.NewRequest("GET", front.URL+"/auth/microsoft/callback?state="+authorize.Query().Get("state")+"&code=old-flow", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(flow.Cookies()[0])
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict || res.Header.Get("Location") != "" || upstream.Load() != 0 {
		t.Fatalf("old flow survived admin rotation: status=%d, upstream=%d", res.StatusCode, upstream.Load())
	}
}
