package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const GraphBase = "https://graph.microsoft.com/v1.0"
const SubstrateBase = "https://substrate.office.com/todob2/api/v1"
const GraphScopes = "Tasks.ReadWrite offline_access User.Read"
const SubstrateScopes = "https://outlook.office.com/Tasks.ReadWrite offline_access"
const AuthScopes = GraphScopes + " https://outlook.office.com/Tasks.ReadWrite"

type Error struct {
	Code           string `json:"error"`
	Status         int    `json:"-"`
	UpstreamStatus int    `json:"upstreamStatus,omitempty"`
	RetryAfter     int    `json:"retryAfter,omitempty"`
	Unknown        bool   `json:"outcomeUnknown,omitempty"`
	SyncIncomplete bool   `json:"syncIncomplete,omitempty"`
}

func (e *Error) Error() string         { return e.Code }
func E(status int, code string) *Error { return &Error{Status: status, Code: code} }
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		copy := *e
		return &copy
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return E(504, "upstream_timeout")
	}
	if errors.Is(err, context.Canceled) {
		return E(504, "request_cancelled")
	}
	return E(500, "internal_error")
}

type tokenProvider interface {
	Get(context.Context, int64, string, string) (string, string, error)
}
type cooldown struct {
	Until     time.Time
	Forbidden bool
}
type Client struct {
	HTTP      *http.Client
	Tokens    tokenProvider
	Limiter   *rate.Limiter
	slots     chan struct{}
	substrate chan struct{}
	mu        sync.Mutex
	cooldowns map[string]cooldown
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_rejected") }}, Limiter: rate.NewLimiter(2, 2), slots: make(chan struct{}, 2), substrate: make(chan struct{}, 1), cooldowns: map[string]cooldown{}}
}
func Pin(raw, host string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != host || u.User != nil || u.Fragment != "" {
		return E(502, "upstream_url_rejected")
	}
	return nil
}
func Link(raw, host string) (string, error) {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		raw = "https://" + host + raw
	}
	if err := Pin(raw, host); err != nil {
		return "", err
	}
	return raw, nil
}
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func take(ctx context.Context, ch chan struct{}) error {
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Client) setCooldown(host string, d time.Duration, forbidden bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.cooldowns[host]
	until := time.Now().Add(d)
	if until.After(v.Until) {
		c.cooldowns[host] = cooldown{until, forbidden}
	}
}
func (c *Client) acquire(ctx context.Context, host string) (func(), error) {
	for {
		c.mu.Lock()
		v := c.cooldowns[host]
		c.mu.Unlock()
		d := time.Until(v.Until)
		if d > 0 {
			code := "upstream_rate_limited"
			status := 429
			if v.Forbidden {
				status = 503
				code = "my_day_unavailable"
				if host != "substrate.office.com" {
					code = "graph_unavailable"
				}
			}
			if deadline, ok := ctx.Deadline(); v.Forbidden || (ok && time.Until(deadline) <= d) {
				return nil, &Error{Code: code, Status: status, RetryAfter: int(math.Ceil(d.Seconds()))}
			}
			if err := wait(ctx, d); err != nil {
				return nil, err
			}
		}
		// Reserve the Substrate slot first so queued Substrate requests don't occupy
		// both general slots and starve Graph metadata or token refreshes.
		if host == "substrate.office.com" {
			if err := take(ctx, c.substrate); err != nil {
				return nil, err
			}
		}
		if err := take(ctx, c.slots); err != nil {
			if host == "substrate.office.com" {
				<-c.substrate
			}
			return nil, err
		}
		release := func() {
			<-c.slots
			if host == "substrate.office.com" {
				<-c.substrate
			}
		}
		if err := c.Limiter.Wait(ctx); err != nil {
			release()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, E(429, "upstream_rate_limited")
		}
		// Another in-flight response may have introduced a cooldown while waiting.
		c.mu.Lock()
		blocked := time.Now().Before(c.cooldowns[host].Until)
		c.mu.Unlock()
		if blocked {
			release()
			continue
		}
		return release, nil
	}
}
func (c *Client) send(ctx context.Context, method, raw, host string, headers http.Header, body []byte) ([]byte, int, http.Header, error) {
	if err := Pin(raw, host); err != nil {
		return nil, 0, nil, err
	}
	release, err := c.acquire(ctx, host)
	if err != nil {
		return nil, 0, nil, err
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, method, raw, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header = headers
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer res.Body.Close()
	// Preserve Retry-After even when the peer stalls while sending an error body.
	if res.StatusCode == 429 {
		c.setCooldown(host, retryDelay(res.Header.Get("Retry-After"), 0), false)
	}
	if res.StatusCode == 403 {
		c.setCooldown(host, 30*time.Second, true)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024+1))
	if len(b) > 16*1024*1024 {
		err = errors.New("upstream_body_too_large")
	}
	return b, res.StatusCode, res.Header, err
}
func retryDelay(v string, attempt int) time.Duration {
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f < 1e9 {
		return time.Duration(f * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, time.Until(t))
	}
	return time.Duration(1<<attempt)*time.Second + time.Duration(rand.IntN(250))*time.Millisecond
}
func (c *Client) Do(ctx context.Context, epoch int64, resource, method, raw string, body any, ifMatch string) (json.RawMessage, error) {
	host := "graph.microsoft.com"
	code := "graph_error"
	if resource == "substrate" {
		host = "substrate.office.com"
		code = "substrate_error"
	}
	if err := Pin(raw, host); err != nil {
		return nil, err
	}
	token, anchor, err := c.Tokens.Get(ctx, epoch, resource, "")
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	refreshed, retried, throttled := false, false, 0
	for {
		h := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}
		if payload != nil {
			h.Set("Content-Type", "application/json")
		}
		if ifMatch != "" {
			h.Set("If-Match", ifMatch)
		}
		if resource == "substrate" && anchor != "" {
			h.Set("x-anchormailbox", anchor)
		}
		b, status, headers, e := c.send(ctx, method, raw, host, h, payload)
		if e != nil {
			var business *Error
			if errors.As(e, &business) {
				return nil, e
			}
			if method == "GET" && !retried && ctx.Err() == nil {
				retried = true
				if err := wait(ctx, 200*time.Millisecond); err != nil {
					return nil, err
				}
				continue
			}
			if method != "GET" {
				return nil, &Error{Status: 502, Code: "write_outcome_unknown", Unknown: true}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, E(502, code)
		}
		if status == 401 && !refreshed {
			refreshed = true
			token, anchor, err = c.Tokens.Get(ctx, epoch, resource, token)
			if err != nil {
				return nil, err
			}
			continue
		}
		if status == 429 && throttled < 2 {
			d := retryDelay(headers.Get("Retry-After"), throttled)
			c.setCooldown(host, d, false)
			throttled++
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= d {
				return nil, &Error{Status: 429, Code: code, UpstreamStatus: 429, RetryAfter: int(math.Ceil(d.Seconds()))}
			}
			continue
		}
		if status >= 500 && method == "GET" && !retried {
			retried = true
			if err := wait(ctx, 200*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		if status < 200 || status >= 300 {
			e := &Error{Status: 502, Code: code, UpstreamStatus: status}
			if status == 429 {
				e.Status = 429
				e.RetryAfter = int(math.Ceil(retryDelay(headers.Get("Retry-After"), throttled).Seconds()))
			}
			if status == 403 && resource == "substrate" {
				e.Status = 503
				e.Code = "my_day_unavailable"
				e.RetryAfter = 30
			}
			if status == 404 {
				e.Status = 404
				e.Code = "not_found"
			}
			if status == 400 || status == 409 || status == 412 {
				e.Status = status
			}
			if status >= 500 && method != "GET" {
				e.Code = "write_outcome_unknown"
				e.Unknown = true
			}
			return nil, e
		}
		if status == 204 {
			return json.RawMessage(`{}`), nil
		}
		if !json.Valid(b) {
			if method != "GET" {
				return nil, &Error{Status: 502, Code: "write_outcome_unknown", Unknown: true}
			}
			return nil, E(502, code)
		}
		return b, nil
	}
}
