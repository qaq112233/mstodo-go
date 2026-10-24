package httpapi_test

import (
	"net/http/httptest"
	"testing"

	"github.com/qaq112233/mstodo-go/internal/httpapi"
)

func TestNoReferrerFormsStillRequireSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, origin, site string
		allow              bool
	}{
		{"native no-referrer form", "null", "same-origin", true},
		{"opaque origin without metadata", "null", "", false},
		{"cross-site opaque form", "null", "cross-site", false},
		{"same-site opaque form", "null", "same-site", false},
		{"matching explicit origin", "https://todo.example.test", "same-origin", true},
		{"foreign origin despite forged metadata", "https://evil.example.test", "same-origin", false},
		{"cross-site matching origin", "https://todo.example.test", "cross-site", false},
		{"non-browser client", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://todo.example.test/admin/private/save", nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if got := httpapi.AllowedOrigin(r, "https://todo.example.test"); got != tc.allow {
				t.Fatal("origin check", got, tc.allow)
			}
		})
	}
}
