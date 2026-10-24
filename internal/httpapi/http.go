package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qaq112233/mstodo-go/internal/microsoft"
	"github.com/qaq112233/mstodo-go/internal/store"
	"github.com/qaq112233/mstodo-go/internal/todo"
)

func Equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(data)
	}
}
func Failure(w http.ResponseWriter, err error) {
	e := microsoft.AsError(err)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	JSON(w, e.Status, e)
}
func AllowedOrigin(r *http.Request, base string) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// no-referrer intentionally hides the secret admin path, but the Fetch
	// standard also serializes the Origin of native form POSTs as "null".
	// Only accept that opaque value with browser-supplied same-origin metadata;
	// admin forms independently require their unpredictable CSRF token.
	if origin == "null" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, e := url.Parse(base)
	return e == nil && origin == u.Scheme+"://"+u.Host
}

// BusinessAuth reads the live configuration so credential rotation takes effect
// without a restart. In path mode the unprefixed business routes are absent.
func BusinessAuth(s *store.Store, base string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		v, err := s.Snapshot(r.Context())
		if err != nil {
			Failure(w, err)
			return
		}
		if v.Settings.AuthMode == "path" {
			prefix := "/s/" + v.Settings.PathSecret
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
			if len(parts) != 3 || parts[0] != "s" || !Equal(parts[1], v.Settings.PathSecret) {
				Failure(w, microsoft.E(404, "not_found"))
				return
			}
			if !AllowedOrigin(r, base) {
				Failure(w, microsoft.E(403, "origin_rejected"))
				return
			}
			http.StripPrefix(prefix, next).ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/s/") {
			Failure(w, microsoft.E(404, "not_found"))
			return
		}
		if v.Settings.AuthMode == "bearer" {
			value := r.Header.Get("Authorization")
			if !strings.HasPrefix(value, "Bearer ") || !Equal(strings.TrimPrefix(value, "Bearer "), v.Settings.Bearer) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				Failure(w, microsoft.E(401, "unauthorized"))
				return
			}
		} else if v.Settings.AuthMode != "none" {
			Failure(w, microsoft.E(503, "api_not_configured"))
			return
		}
		if !AllowedOrigin(r, base) {
			Failure(w, microsoft.E(403, "origin_rejected"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
func REST(s *todo.Service) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, methods map[string]string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			kind, ok := methods[r.Method]
			if !ok {
				allow := []string{}
				for m := range methods {
					allow = append(allow, m)
				}
				sort.Strings(allow)
				w.Header().Set("Allow", strings.Join(allow, ", "))
				Failure(w, microsoft.E(405, "method_not_allowed"))
				return
			}
			op := todo.Operation{Kind: kind, ListID: r.PathValue("listId"), TaskID: r.PathValue("taskId"), ItemID: r.PathValue("itemId"), Query: r.URL.Query(), IfMatch: r.Header.Get("If-Match"), Args: todo.Object{}}
			if r.Method == "POST" || r.Method == "PATCH" {
				d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
				err := d.Decode(&op.Args)
				if err == io.EOF && oneOfKind(kind, "add_myday") {
					err = nil
				}
				if err != nil || op.Args == nil {
					Failure(w, microsoft.E(400, "invalid_json"))
					return
				}
				var trailing any
				if err = d.Decode(&trailing); err != io.EOF {
					Failure(w, microsoft.E(400, "invalid_json"))
					return
				}
			}
			start := time.Now()
			res, err := s.Call(r.Context(), op)
			// Only symbolic route names; no task IDs, secret paths, queries or bodies.
			slog.Info("api", "operation", kind, "durationMs", time.Since(start).Milliseconds(), "ok", err == nil)
			if err != nil {
				Failure(w, err)
				return
			}
			if !res.Fetched.IsZero() {
				hit := "MISS"
				if res.Hit {
					hit = "HIT"
				}
				w.Header().Set("X-Cache", hit)
				w.Header().Set("X-Data-Fetched-At", res.Fetched.UTC().Format(time.RFC3339Nano))
				w.Header().Set("X-Cache-TTL", "180")
			}
			if res.Warning != "" {
				w.Header().Set("X-Cache-Warning", res.Warning)
			}
			JSON(w, res.Status, res.Data)
		})
	}
	route("/api/v1/myday", map[string]string{"GET": "myday"})
	route("/api/v1/myday/reorder", map[string]string{"POST": "reorder_myday"})
	route("/api/v1/status", map[string]string{"GET": "status"})
	route("/api/v1/tasks", map[string]string{"GET": "query"})
	route("/api/v1/lists", map[string]string{"GET": "lists", "POST": "create_list"})
	route("/api/v1/lists/{listId}", map[string]string{"GET": "get_list", "PATCH": "update_list", "DELETE": "delete_list"})
	route("/api/v1/lists/{listId}/tasks", map[string]string{"GET": "list_tasks", "POST": "create_task"})
	route("/api/v1/lists/{listId}/tasks/{taskId}", map[string]string{"GET": "get_task", "PATCH": "update_task", "DELETE": "delete_task"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/myday", map[string]string{"POST": "add_myday", "DELETE": "remove_myday"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/move", map[string]string{"POST": "move"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/reorder", map[string]string{"POST": "reorder_task"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/checklist-items", map[string]string{"GET": "list_checklist", "POST": "create_checklist"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/checklist-items/{itemId}", map[string]string{"GET": "get_checklist", "PATCH": "update_checklist", "DELETE": "delete_checklist"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/checklist-items/{itemId}/reorder", map[string]string{"POST": "reorder_checklist"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/linked-resources", map[string]string{"GET": "list_links", "POST": "create_link"})
	route("/api/v1/lists/{listId}/tasks/{taskId}/linked-resources/{itemId}", map[string]string{"GET": "get_link", "PATCH": "update_link", "DELETE": "delete_link"})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { Failure(w, microsoft.E(404, "not_found")) })
	return mux
}
func oneOfKind(s string, kinds ...string) bool {
	for _, k := range kinds {
		if s == k {
			return true
		}
	}
	return false
}
