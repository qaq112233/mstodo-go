package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/qaq112233/mstodo-go/internal/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func run() error {
	listen := env("LISTEN_ADDR", ":8080")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		host, port, e := net.SplitHostPort(listen)
		if e != nil {
			return e
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c := http.Client{Timeout: 3 * time.Second}
		r, e := c.Get("http://" + net.JoinHostPort(host, port) + "/health")
		if e != nil {
			return e
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("health status %d", r.StatusCode)
		}
		return nil
	}
	a, err := app.Open(env("DATA_DIR", "/data"), os.Getenv("PUBLIC_BASE_URL"))
	if err != nil {
		return err
	}
	defer a.Store.DB.Close()
	if len(os.Args) > 1 && os.Args[1] == "admin-url" {
		v, e := a.Store.Snapshot(context.Background())
		if e != nil {
			return e
		}
		fmt.Println(a.Base + "/admin/" + v.Settings.AdminSecret)
		return nil
	}
	if a.Fresh {
		v, e := a.Store.Snapshot(context.Background())
		if e != nil {
			return e
		}
		fmt.Fprintln(os.Stdout, "首次初始化，请保存管理员链接："+a.Base+"/admin/"+v.Settings.AdminSecret)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); a.Maintenance(ctx) }()
	server := &http.Server{Addr: listen, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	errCh := make(chan error, 1)
	go func() { slog.Info("server_started", "listen", listen); errCh <- server.ListenAndServe() }()
	select {
	case err = <-errCh:
		if err == http.ErrServerClosed {
			err = nil
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdown)
	stop()
	<-maintenanceDone
	a.Todo.Wait()
	a.Tokens.Wait()
	if err != nil {
		return err
	}
	return shutdownErr
}
func main() {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(strings.ToUpper(env("LOG_LEVEL", "info"))))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if err := run(); err != nil {
		slog.Error("service_failed", "error", err.Error())
		os.Exit(1)
	}
}
