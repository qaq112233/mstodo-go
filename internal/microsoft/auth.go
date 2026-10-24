package microsoft

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qaq112233/mstodo-go/internal/store"
	"golang.org/x/sync/singleflight"
)

type Manager struct {
	Store  *store.Store
	Client *Client
	group  singleflight.Group
	serial chan struct{}
	Now    func() time.Time
	work   sync.WaitGroup
}

func (m *Manager) Wait() { m.work.Wait() }
func (m *Manager) flight(key string, fn func() (any, error)) <-chan singleflight.Result {
	m.work.Add(1)
	shared := m.group.DoChan(key, fn)
	ch := make(chan singleflight.Result, 1)
	go func() { defer m.work.Done(); ch <- <-shared }()
	return ch
}

func NewManager(s *store.Store, c *Client) *Manager {
	m := &Manager{Store: s, Client: c, serial: make(chan struct{}, 1), Now: time.Now}
	c.Tokens = m
	return m
}

type tokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_in"`
	Type    string `json:"token_type"`
}

func (m *Manager) post(ctx context.Context, cfg store.Settings, values url.Values) (tokenResponse, error) {
	values.Set("client_id", cfg.ClientID)
	values.Set("client_secret", cfg.ClientSecret)
	raw := "https://login.microsoftonline.com/" + url.PathEscape(cfg.Tenant) + "/oauth2/v2.0/token"
	b, status, h, err := m.Client.send(ctx, "POST", raw, "login.microsoftonline.com", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(values.Encode()))
	if err != nil {
		if ctx.Err() != nil {
			return tokenResponse{}, ctx.Err()
		}
		return tokenResponse{}, E(502, "microsoft_token_error")
	}
	if status != 200 {
		var e struct {
			Error string `json:"error"`
			Codes []int  `json:"error_codes"`
		}
		_ = json.Unmarshal(b, &e)
		for _, code := range e.Codes {
			if code == 65001 {
				return tokenResponse{}, E(503, "microsoft_consent_required")
			}
		}
		if e.Error == "invalid_grant" || e.Error == "interaction_required" || e.Error == "consent_required" {
			return tokenResponse{}, E(503, "microsoft_reauth_required")
		}
		out := E(502, "microsoft_token_error")
		if status == 429 {
			out.Status = 429
			out.Code = "upstream_rate_limited"
			out.RetryAfter = int(math.Ceil(retryDelay(h.Get("Retry-After"), 0).Seconds()))
		}
		return tokenResponse{}, out
	}
	var t tokenResponse
	if json.Unmarshal(b, &t) != nil || t.Access == "" || t.Expires <= 0 || !strings.EqualFold(t.Type, "Bearer") {
		return t, E(502, "microsoft_token_invalid_response")
	}
	return t, nil
}
func (m *Manager) Get(ctx context.Context, epoch int64, resource, rejected string) (string, string, error) {
	a, err := m.Store.Account(ctx, epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", E(503, "not_authenticated")
	}
	if err != nil {
		return "", "", err
	}
	if a.Reauth {
		return "", "", E(503, "microsoft_reauth_required")
	}
	if resource == "substrate" && a.SubstrateConsent {
		return "", "", E(503, "my_day_unavailable")
	}
	t := a.Graph
	if resource == "substrate" {
		t = a.Substrate
	}
	if t.Access != "" && t.Access != rejected && t.Expires.After(m.Now().Add(5*time.Minute)) {
		return t.Access, a.Anchor, nil
	}
	// Waiters can cancel; the shared operation retains its own bounded lifetime
	// so a rotated refresh token is persisted even when all waiters leave.
	ch := m.flight(strings.Join([]string{itoa(epoch), resource}, ":"), func() (any, error) {
		shared, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := take(shared, m.serial); err != nil {
			return nil, err
		}
		defer func() { <-m.serial }()
		v, err := m.Store.Snapshot(shared)
		if err != nil {
			return nil, err
		}
		if v.Epoch != epoch {
			return nil, store.ErrChanged
		}
		current, err := m.Store.Account(shared, epoch)
		if err != nil {
			return nil, err
		}
		if current.Reauth {
			return nil, E(503, "microsoft_reauth_required")
		}
		t := current.Graph
		scope := GraphScopes
		if resource == "substrate" {
			t = current.Substrate
			scope = SubstrateScopes
			if current.SubstrateConsent {
				return nil, E(503, "my_day_unavailable")
			}
		}
		if t.Access != "" && t.Access != rejected && t.Expires.After(m.Now().Add(5*time.Minute)) {
			return [2]string{t.Access, current.Anchor}, nil
		}
		res, err := m.post(shared, v.Settings, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {current.Refresh}, "scope": {scope}})
		if err != nil {
			code := AsError(err).Code
			if code == "microsoft_reauth_required" || code == "microsoft_consent_required" {
				if resource == "substrate" && code == "microsoft_consent_required" {
					current.SubstrateConsent = true
				} else {
					current.Reauth = true
				}
				if saveErr := m.Store.SaveAccount(shared, epoch, current); saveErr != nil {
					return nil, saveErr
				}
				if current.SubstrateConsent {
					return nil, E(503, "my_day_unavailable")
				}
				return nil, E(503, "microsoft_reauth_required")
			}
			return nil, err
		}
		if res.Refresh != "" {
			current.Refresh = res.Refresh
		}
		token := store.Token{Access: res.Access, Expires: m.Now().Add(time.Duration(res.Expires) * time.Second)}
		if resource == "substrate" {
			current.Substrate = token
		} else {
			current.Graph = token
		}
		if err = m.Store.SaveAccount(shared, epoch, current); err != nil {
			return nil, err
		}
		return [2]string{token.Access, current.Anchor}, nil
	})
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return "", "", r.Err
		}
		pair := r.Val.([2]string)
		return pair[0], pair[1], nil
	}
}
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func (m *Manager) Maintenance(ctx context.Context) error {
	v, err := m.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	a, err := m.Store.Account(ctx, v.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	now := m.Now()
	if a.Reauth || a.SubstrateConsent || now.Sub(a.Maintained) < 7*24*time.Hour || (a.MaintenanceError && now.Sub(a.Attempted) < 24*time.Hour) {
		return nil
	}
	ch := m.flight("maintenance", func() (any, error) {
		_, _, e := m.Get(context.Background(), v.Epoch, "graph", a.Graph.Access)
		if e == nil {
			_, _, e = m.Get(context.Background(), v.Epoch, "substrate", a.Substrate.Access)
		}
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if take(bounded, m.serial) != nil {
			return nil, e
		}
		defer func() { <-m.serial }()
		current, loadErr := m.Store.Account(bounded, v.Epoch)
		if loadErr != nil {
			return nil, loadErr
		}
		current.Attempted = now
		current.MaintenanceError = e != nil
		if e == nil {
			current.Maintained = m.Now()
		}
		if err := m.Store.SaveAccount(bounded, v.Epoch, current); err != nil {
			return nil, err
		}
		return nil, e
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		return r.Err
	}
}
func AuthorizeURL(cfg store.Settings, redirect, state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {cfg.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "response_mode": {"query"}, "scope": {AuthScopes}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "prompt": {"select_account"}}
	return "https://login.microsoftonline.com/" + url.PathEscape(cfg.Tenant) + "/oauth2/v2.0/authorize?" + q.Encode()
}
func (m *Manager) Exchange(ctx context.Context, v store.Snapshot, code, verifier, redirect string) error {
	res, err := m.post(ctx, v.Settings, url.Values{"grant_type": {"authorization_code"}, "scope": {AuthScopes}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return err
	}
	if res.Refresh == "" {
		return E(502, "microsoft_token_missing_refresh_token")
	}
	b, status, _, err := m.Client.send(ctx, "GET", GraphBase+"/me", "graph.microsoft.com", http.Header{"Authorization": {"Bearer " + res.Access}}, nil)
	if err != nil || status != 200 {
		return E(502, "microsoft_identity_error")
	}
	var me struct {
		ID   string `json:"id"`
		Name string `json:"displayName"`
		Mail string `json:"mail"`
		UPN  string `json:"userPrincipalName"`
	}
	if json.Unmarshal(b, &me) != nil || me.ID == "" {
		return E(502, "microsoft_identity_error")
	}
	expected := strings.ToLower(strings.TrimSpace(v.Settings.ExpectedAccount))
	if expected == "" || (!strings.EqualFold(strings.TrimSpace(me.Mail), expected) && !strings.EqualFold(strings.TrimSpace(me.UPN), expected)) {
		return E(403, "account_mismatch")
	}
	// These claims come only from our just-issued TLS token and are routing hints,
	// never identity evidence. The authenticated /me result above is the gate.
	anchor := ""
	parts := strings.Split(res.Access, ".")
	if len(parts) >= 2 {
		if p, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil {
			var claims map[string]any
			if json.Unmarshal(p, &claims) == nil {
				oid, _ := claims["oid"].(string)
				tid, _ := claims["tid"].(string)
				if oid != "" && tid != "" {
					anchor = "OID:" + oid + "@" + tid
				}
			}
		}
	}
	email := me.Mail
	if email == "" {
		email = me.UPN
	}
	return m.Store.InstallAccount(ctx, v.Epoch, store.Account{ID: me.ID, Name: me.Name, Email: email, Anchor: anchor, Refresh: res.Refresh, Graph: store.Token{Access: res.Access, Expires: m.Now().Add(time.Duration(res.Expires) * time.Second)}, Maintained: m.Now()})
}
