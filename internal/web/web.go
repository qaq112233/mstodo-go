package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qaq112233/mstodo-go/internal/httpapi"
	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

//go:embed settings.html
var page string
var tmpl = template.Must(template.New("settings").Parse(page))

type state struct {
	Verifier string
	Browser  string
	Epoch    int64
	Expires  time.Time
}
type Handler struct {
	Store  *store.Store
	Auth   *microsoft.Manager
	Todo   *todo.Service
	Base   string
	mu     sync.Mutex
	states map[string]state
}

func New(s *store.Store, m *microsoft.Manager, t *todo.Service, base string) *Handler {
	return &Handler{Store: s, Auth: m, Todo: t, Base: base, states: map[string]state{}}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://login.microsoftonline.com; base-uri 'none'; frame-ancestors 'none'")
	if r.URL.Path == "/auth/microsoft/callback" {
		h.callback(w, r)
		return
	}
	v, err := h.Store.Snapshot(r.Context())
	if err != nil {
		httpapi.Failure(w, err)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "admin" || !httpapi.Equal(parts[1], v.Settings.AdminSecret) {
		httpapi.Failure(w, microsoft.E(404, "not_found"))
		return
	}
	action := ""
	if len(parts) == 3 {
		action = parts[2]
	}
	if action == "" {
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			httpapi.Failure(w, microsoft.E(405, "method_not_allowed"))
			return
		}
		h.render(w, r, v, "")
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		httpapi.Failure(w, microsoft.E(405, "method_not_allowed"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if !httpapi.AllowedOrigin(r, h.Base) || r.ParseForm() != nil || !httpapi.Equal(r.PostForm.Get("csrf"), v.Settings.CSRF) {
		httpapi.Failure(w, microsoft.E(403, "csrf_rejected"))
		return
	}
	switch action {
	case "save":
		cfg := v.Settings
		cfg.Tenant = strings.TrimSpace(r.PostForm.Get("tenant"))
		cfg.ClientID = strings.TrimSpace(r.PostForm.Get("clientId"))
		cfg.ExpectedAccount = strings.TrimSpace(r.PostForm.Get("expectedAccount"))
		if secret := r.PostForm.Get("clientSecret"); secret != "" {
			cfg.ClientSecret = secret
		}
		cfg.Timezone = r.PostForm.Get("timezone")
		cfg.AuthMode = r.PostForm.Get("authMode")
		pages, e := strconv.Atoi(r.PostForm.Get("myDayMaxPages"))
		if e != nil || pages < 1 || pages > 1000 {
			httpapi.Failure(w, microsoft.E(400, "invalid_page_limit"))
			return
		}
		cfg.MyDayMaxPages = pages
		if _, e = time.LoadLocation(cfg.Timezone); e != nil {
			httpapi.Failure(w, microsoft.E(400, "invalid_timezone"))
			return
		}
		if cfg.AuthMode != "none" && cfg.AuthMode != "bearer" && cfg.AuthMode != "path" {
			httpapi.Failure(w, microsoft.E(400, "invalid_auth_mode"))
			return
		}
		address, e := mail.ParseAddress(cfg.ExpectedAccount)
		if !regexp.MustCompile(`^[A-Za-z0-9.-]{1,255}$`).MatchString(cfg.Tenant) || !regexp.MustCompile(`^[a-fA-F0-9-]{36}$`).MatchString(cfg.ClientID) || cfg.ClientSecret == "" || e != nil || address.Address != cfg.ExpectedAccount {
			httpapi.Failure(w, microsoft.E(400, "invalid_microsoft_settings"))
			return
		}
		if err = h.Store.SaveSettings(r.Context(), v, cfg); err != nil {
			httpapi.Failure(w, err)
			return
		}
		http.Redirect(w, r, "/admin/"+cfg.AdminSecret, http.StatusSeeOther)
	case "connect":
		if v.Settings.ClientID == "" || v.Settings.ClientSecret == "" || v.Settings.ExpectedAccount == "" {
			httpapi.Failure(w, microsoft.E(400, "save_settings_first"))
			return
		}
		opaque, verifier, browser := store.Secret(), store.Secret(), store.Secret()
		h.mu.Lock()
		for key, value := range h.states {
			if time.Now().After(value.Expires) {
				delete(h.states, key)
			}
		}
		if len(h.states) >= 16 {
			h.mu.Unlock()
			httpapi.Failure(w, microsoft.E(429, "too_many_authorization_flows"))
			return
		}
		h.states[opaque] = state{verifier, browser, v.Epoch, time.Now().Add(10 * time.Minute)}
		h.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "mstodo_oauth", Value: browser, Path: "/auth/microsoft/callback", MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(h.Base, "https://"), SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, microsoft.AuthorizeURL(v.Settings, h.Base+"/auth/microsoft/callback", opaque, verifier), http.StatusSeeOther)
	case "rotate":
		cfg := v.Settings
		cfg.Bearer = store.Secret()
		cfg.PathSecret = store.Secret()
		if r.PostForm.Get("target") == "admin" {
			cfg.AdminSecret = store.Secret()
			cfg.CSRF = store.Secret()
		}
		if err = h.Store.SaveSettings(r.Context(), v, cfg); err != nil {
			httpapi.Failure(w, err)
			return
		}
		http.Redirect(w, r, "/admin/"+cfg.AdminSecret, http.StatusSeeOther)
	case "probe":
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		_, ge := h.Todo.Call(ctx, todo.Operation{Kind: "lists", Query: url.Values{"refresh": {"true"}}})
		result, me := h.Todo.Call(ctx, todo.Operation{Kind: "myday", Query: url.Values{"refresh": {"true"}}})
		message := "Graph: " + probeCode(ge) + "；My Day: " + probeCode(me)
		if me == nil {
			b, _ := json.Marshal(result.Data)
			var m struct {
				Count int `json:"count"`
			}
			_ = json.Unmarshal(b, &m)
			message += "（" + strconv.Itoa(m.Count) + " 项）"
		}
		h.render(w, r, v, message)
	default:
		httpapi.Failure(w, microsoft.E(404, "not_found"))
	}
}
func probeCode(e error) string {
	if e == nil {
		return "可用"
	}
	return microsoft.AsError(e).Code
}
func (h *Handler) render(w http.ResponseWriter, r *http.Request, v store.Snapshot, message string) {
	a, _ := h.Store.Account(r.Context(), v.Epoch)
	prefix := h.Base
	if v.Settings.AuthMode == "path" {
		prefix += "/s/" + v.Settings.PathSecret
	}
	status, _ := h.Store.Status(r.Context(), v.Epoch)
	b, _ := json.MarshalIndent(status, "", "  ")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{"Settings": v.Settings, "Account": a, "Base": h.Base, "Prefix": prefix, "Admin": "/admin/" + v.Settings.AdminSecret, "Callback": h.Base + "/auth/microsoft/callback", "Message": message, "Sync": string(b)}
	_ = tmpl.Execute(w, data)
}
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		httpapi.Failure(w, microsoft.E(405, "method_not_allowed"))
		return
	}
	opaque := r.URL.Query().Get("state")
	cookie, err := r.Cookie("mstodo_oauth")
	h.mu.Lock()
	flow, ok := h.states[opaque]
	valid := ok && err == nil && time.Now().Before(flow.Expires) && httpapi.Equal(cookie.Value, flow.Browser)
	if valid {
		delete(h.states, opaque)
	}
	h.mu.Unlock()
	if !valid {
		httpapi.Failure(w, microsoft.E(400, "invalid_oauth_state"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "mstodo_oauth", Value: "", Path: "/auth/microsoft/callback", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(h.Base, "https://"), SameSite: http.SameSiteLaxMode})
	v, err := h.Store.Snapshot(r.Context())
	if err != nil {
		httpapi.Failure(w, err)
		return
	}
	if v.Epoch != flow.Epoch {
		httpapi.Failure(w, microsoft.E(409, "settings_changed_restart_authorization"))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		httpapi.Failure(w, microsoft.E(400, "microsoft_authorization_denied"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err = h.Auth.Exchange(ctx, v, code, flow.Verifier, h.Base+"/auth/microsoft/callback"); err != nil {
		httpapi.Failure(w, err)
		return
	}
	http.Redirect(w, r, "/admin/"+v.Settings.AdminSecret, http.StatusSeeOther)
}
