package todo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
)

type fixedTokenProvider struct{}

func (fixedTokenProvider) Get(context.Context, int64, string, string) (string, string, error) {
	return "test-token", "", nil
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSharedReadsSurviveFirstCallerDeadline(t *testing.T) {
	for _, kind := range []string{"cached", "delta"} {
		t.Run(kind, func(t *testing.T) {
			db, _, err := store.Open(filepath.Join(t.TempDir(), "mstodo.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.DB.Close() })
			client := microsoft.NewClient()
			client.Tokens = fixedTokenProvider{}
			s := New(db, client)
			t.Cleanup(s.Wait)
			v, err := db.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var startOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			load := func(ctx context.Context) error {
				startOnce.Do(func() { close(entered) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			client.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if err := load(r.Context()); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"value":[],"@odata.deltaLink":"https://graph.microsoft.com/v1.0/me/todo/lists/L/tasks/delta?end=1"}`))}, nil
			})
			read := func(ctx context.Context) error {
				if kind == "delta" {
					return s.syncList(ctx, v, "L", false)
				}
				_, err := s.cached(ctx, v, "shared", false, 5*time.Second, func(ctx context.Context, _ store.Snapshot) (any, error) {
					return []Object{}, load(ctx)
				})
				return err
			}
			first, cancelFirst := context.WithTimeout(context.Background(), time.Second)
			defer cancelFirst()
			firstDone := make(chan error, 1)
			go func() { firstDone <- read(first) }()
			<-entered
			second, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelSecond()
			secondDone := make(chan error, 1)
			go func() { secondDone <- read(second) }()
			// Ensure both callers joined before the first one's deadline expires.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for joined := false; !joined; {
				s.mu.Lock()
				for _, f := range s.flights {
					joined = joined || f.users == 2
				}
				s.mu.Unlock()
				if !joined {
					select {
					case <-first.Done():
						t.Fatal("second caller did not join before the deadline")
					case <-ticker.C:
					}
				}
			}
			if err := <-firstDone; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first caller: %v", err)
			}
			unblock()
			if err := <-secondDone; err != nil {
				t.Fatalf("first deadline cancelled another caller: %v", err)
			}
		})
	}
}
