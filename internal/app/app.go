package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/qaq112233/mstodo-go/internal/httpapi"
	"github.com/qaq112233/mstodo-go/internal/mcpserver"
	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/todo"
	"github.com/qaq112233/mstodo-go/internal/web"
)

type App struct {
	Store   *store.Store
	Tokens  *microsoft.Manager
	Todo    *todo.Service
	Handler http.Handler
	Fresh   bool
	Base    string
}

func Open(dataDir, base string) (*App, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return nil, errors.New("PUBLIC_BASE_URL must be an explicit HTTPS origin (HTTP is allowed only for loopback development)")
	}
	base = strings.TrimRight(base, "/")
	s, fresh, err := store.Open(filepath.Join(dataDir, "mstodo.db"))
	if err != nil {
		return nil, err
	}
	c := microsoft.NewClient()
	tokens := microsoft.NewManager(s, c)
	service := todo.New(s, c)
	return &App{Store: s, Tokens: tokens, Todo: service, Handler: Handler(s, tokens, service, base), Fresh: fresh, Base: base}, nil
}
func Handler(s *store.Store, tokens *microsoft.Manager, service *todo.Service, base string) http.Handler {
	setup := web.New(s, tokens, service, base)
	business := http.NewServeMux()
	business.Handle("/mcp", mcpserver.Handler(service, base))
	business.Handle("/", httpapi.REST(service))
	protected := httpapi.BusinessAuth(s, base, business)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		switch {
		case r.URL.Path == "/health":
			if r.Method != "GET" {
				w.Header().Set("Allow", "GET")
				httpapi.Failure(w, microsoft.E(405, "method_not_allowed"))
				return
			}
			httpapi.JSON(w, 200, map[string]any{"ok": true, "service": "mstodo-go", "version": "0.1.0"})
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><html lang=zh-CN><meta charset=utf-8><title>Microsoft To Do</title><p>服务已启动。请使用首次启动提供的管理员链接进行设置。</p></html>")
		case strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/auth/microsoft/callback":
			setup.ServeHTTP(w, r)
		default:
			protected.ServeHTTP(w, r)
		}
	})
}
func (a *App) Maintenance(ctx context.Context) {
	run := func() {
		bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := a.Tokens.Maintenance(bounded); err != nil {
			slog.Warn("token_maintenance", "error", microsoft.AsError(err).Code)
		}
	}
	run()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
