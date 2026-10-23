// Package simperium is a minimal HTTP client for the Simperium backend that
// powers Simplenote. Only the handful of endpoints snsync needs are
// implemented.
package simperium

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAuthHost = "https://auth.simperium.com"
	DefaultAPIHost  = "https://api.simperium.com"
	// NoteBucket is the Simperium bucket that holds Simplenote notes.
	NoteBucket = "note"
)

const (
	// requestTimeout bounds a single HTTP request, including reading the body.
	// A full index page can be tens of megabytes on a slow link, so the budget
	// is generous; retrying is preferred over aborting a sync.
	requestTimeout = 5 * time.Minute
	// maxAttempts is how often an idempotent request is retried when it fails
	// in a way that looks transient (network error, truncated body, 5xx, 429).
	maxAttempts = 3
)

// Note mirrors the JSON shape Simplenote stores in the "note" bucket.
type Note struct {
	Content          string   `json:"content"`
	Deleted          bool     `json:"deleted"`
	CreationDate     float64  `json:"creationDate"`
	ModificationDate float64  `json:"modificationDate"`
	Tags             []string `json:"tags"`
	SystemTags       []string `json:"systemTags"`
	ShareURL         string   `json:"shareURL"`
	PublishURL       string   `json:"publishURL"`
}

// UnmarshalJSON is deliberately lenient. Simperium stores values in a
// schemaless way: "deleted" is usually a boolean but is sometimes 0/1, dates
// are usually numbers but sometimes numeric strings, and content may be null.
func (n *Note) UnmarshalJSON(data []byte) error {
	var raw struct {
		Content          json.RawMessage `json:"content"`
		Deleted          json.RawMessage `json:"deleted"`
		CreationDate     json.RawMessage `json:"creationDate"`
		ModificationDate json.RawMessage `json:"modificationDate"`
		Tags             []string        `json:"tags"`
		SystemTags       []string        `json:"systemTags"`
		ShareURL         string          `json:"shareURL"`
		PublishURL       string          `json:"publishURL"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n.Content = rawString(raw.Content)
	n.Deleted = rawBool(raw.Deleted)
	n.CreationDate = rawFloat(raw.CreationDate)
	n.ModificationDate = rawFloat(raw.ModificationDate)
	n.Tags = raw.Tags
	n.SystemTags = raw.SystemTags
	n.ShareURL = raw.ShareURL
	n.PublishURL = raw.PublishURL
	return nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch strings.TrimSpace(string(raw)) {
	case "true", "1", `"true"`, `"1"`:
		return true
	case "false", "0", "null", `"false"`, `"0"`, `""`:
		return false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f != 0
	}
	return false
}

func rawFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		f, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f
	}
	return 0
}

// RemoteNote is a note as it appears in the bucket index.
type RemoteNote struct {
	ID      string
	Version int
	Note    Note
}

// Client talks to a single Simperium app.
type Client struct {
	AppID    string
	AppKey   string
	Token    string
	ClientID string
	AuthHost string
	APIHost  string
	HTTP     *http.Client
}

// New builds a client. Token may be empty until Authorize is called.
func New(appID, appKey, token string) *Client {
	c := &Client{
		AppID:    appID,
		AppKey:   appKey,
		Token:    token,
		ClientID: "snsync-" + randHex(8),
		AuthHost: DefaultAuthHost,
		APIHost:  DefaultAPIHost,
		HTTP:     newHTTPClient(),
	}
	if h := os.Getenv("SN_AUTH_HOST"); h != "" {
		c.AuthHost = h
	}
	if h := os.Getenv("SN_API_HOST"); h != "" {
		c.APIHost = h
	}
	return c
}

// newHTTPClient returns a client pinned to HTTP/1.1. Simperium's edge
// (nginx) rejects Go's HTTP/2 requests with a 403, so ALPN is limited to
// http/1.1. The default transport is kept so HTTPS_PROXY etc. still work.
//
// The timeout covers the whole request including the body, so it must be
// generous enough for a full index page (tens of megabytes) on a slow link.
func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: requestTimeout}
}

// transportError wraps a failure that happened while sending the request or
// reading its response, as opposed to an HTTP status the server chose to
// return. These are usually transient.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// statusError is a non-2xx response that is worth retrying (5xx, 429).
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// isRetryable reports whether a failed attempt should be tried again.
func isRetryable(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		// A canceled request means the caller is going away, not a flake.
		// A deadline, on the other hand, is exactly what retrying is for.
		if errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusTooManyRequests || se.status >= 500
	}
	return false
}

// Ping performs a cheap authenticated request to check that the token is
// accepted. It is used to validate tokens obtained elsewhere.
func (c *Client) Ping() error {
	q := url.Values{}
	q.Set("limit", "1")
	u := fmt.Sprintf("%s/1/%s/%s/index?%s", c.APIHost, c.AppID, NoteBucket, q.Encode())
	return c.getJSON(u, nil)
}

// AuthError indicates the token/credentials were rejected.
type AuthError struct{ msg string }

func (e *AuthError) Error() string { return e.msg }

// Authorize exchanges email/password for a long-lived access token.
func (c *Client) Authorize(email, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": email, "password": password})
	u := fmt.Sprintf("%s/1/%s/authorize/", c.AuthHost, c.AppID)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Simperium-API-Key", c.AppKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("authorize: read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &AuthError{"invalid email or password"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authorize: HTTP %d: %s", resp.StatusCode, truncate(rb))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("authorize: bad response: %w", err)
	}
	if out.AccessToken == "" {
		return "", &AuthError{"authorize: empty access token"}
	}
	c.Token = out.AccessToken
	return out.AccessToken, nil
}

type indexItem struct {
	ID string          `json:"id"`
	V  int             `json:"v"`
	D  json.RawMessage `json:"d"`
}

type indexResponse struct {
	Current string      `json:"current"`
	Mark    string      `json:"mark"`
	Count   int         `json:"count"`
	Index   []indexItem `json:"index"`
}

// getJSON performs an idempotent GET, retrying transient failures with a
// short backoff. Simperium occasionally drops a large response mid-body; a
// retry is far better than aborting the whole sync (and the truncated body
// used to surface as a confusing "bad JSON" error).
func (c *Client) getJSON(u string, out any) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * retryDelay)
		}
		lastErr = c.doJSON(http.MethodGet, u, nil, out)
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("%w (after %d attempts)", lastErr, maxAttempts)
}

// retryDelay is the pause before the second attempt; it grows linearly with
// each further attempt. It is a variable so tests need not sleep.
var retryDelay = time.Second

// Index fetches notes. If since is empty it fetches every note (full
// reconciliation); otherwise only notes changed after the given cursor.
// Pagination via "mark" is handled transparently. The returned string is the
// new cursor ("current").
func (c *Client) Index(since string) ([]RemoteNote, string, error) {
	var all []RemoteNote
	mark := ""
	cursor := ""
	seenMarks := map[string]bool{}
	for {
		// A server that keeps returning the same pagination mark would make
		// this loop run forever, so refuse to follow a mark twice.
		if mark != "" {
			if seenMarks[mark] {
				return nil, "", fmt.Errorf("index: pagination mark %q repeated", mark)
			}
			seenMarks[mark] = true
		}
		q := url.Values{}
		q.Set("data", "1")
		q.Set("limit", "1000")
		if since != "" {
			q.Set("since", since)
		}
		if mark != "" {
			q.Set("mark", mark)
		}
		u := fmt.Sprintf("%s/1/%s/%s/index?%s", c.APIHost, c.AppID, NoteBucket, q.Encode())

		var resp indexResponse
		if err := c.getJSON(u, &resp); err != nil {
			return nil, "", err
		}
		if cursor == "" {
			cursor = resp.Current
		}
		for _, it := range resp.Index {
			var n Note
			if len(it.D) > 0 {
				if err := json.Unmarshal(it.D, &n); err != nil {
					return nil, "", fmt.Errorf("index: bad note %s: %w", it.ID, err)
				}
			}
			all = append(all, RemoteNote{ID: it.ID, Version: it.V, Note: n})
		}
		if resp.Mark == "" {
			break
		}
		mark = resp.Mark
	}
	return all, cursor, nil
}

// Save creates (key == "") or updates (key != "") a note and returns the
// server's view of it together with its key.
func (c *Client) Save(key string, n Note) (string, *Note, error) {
	if key == "" {
		key = randHex(16) // 32 hex chars, same shape as the official clients
	}
	q := url.Values{}
	q.Set("clientid", c.ClientID)
	q.Set("ccid", randHex(16))
	q.Set("response", "1")
	u := fmt.Sprintf("%s/1/%s/%s/i/%s?%s", c.APIHost, c.AppID, NoteBucket, url.PathEscape(key), q.Encode())

	var out Note
	if err := c.doJSON(http.MethodPost, u, n, &out); err != nil {
		return key, nil, err
	}
	return key, &out, nil
}

// Get fetches a single note by key.
func (c *Client) Get(key string) (*Note, error) {
	u := fmt.Sprintf("%s/1/%s/%s/i/%s", c.APIHost, c.AppID, NoteBucket, url.PathEscape(key))
	var n Note
	if err := c.getJSON(u, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// notFoundError is an HTTP 404, e.g. when a note was already hard-deleted.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

// IsNotFound reports whether err means the note does not exist (HTTP 404).
func IsNotFound(err error) bool {
	var nf *notFoundError
	return errors.As(err, &nf)
}

// Delete permanently removes a note (hard delete). Prefer marking
// Note.Deleted = true to move a note to the trash. Transient failures are
// retried; a 404 is returned as IsNotFound so callers can treat an already
// missing note as success.
func (c *Client) Delete(key string) error {
	q := url.Values{}
	q.Set("clientid", c.ClientID)
	q.Set("ccid", randHex(16))
	u := fmt.Sprintf("%s/1/%s/%s/i/%s?%s", c.APIHost, c.AppID, NoteBucket, url.PathEscape(key), q.Encode())
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * retryDelay)
		}
		lastErr = c.doJSON(http.MethodDelete, u, nil, nil)
		if lastErr == nil || IsNotFound(lastErr) || !isRetryable(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("%w (after %d attempts)", lastErr, maxAttempts)
}

func (c *Client) doJSON(method, u string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Simperium-Token", c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &transportError{err}
	}
	defer resp.Body.Close()

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		// The status/body may be fine, but we never saw all of it. Report it
		// as a transport failure so a retry can be attempted.
		return &transportError{fmt.Errorf("%s %s: read response body: %w", method, u, err)}
	}
	if resp.ContentLength >= 0 && int64(len(rb)) != resp.ContentLength {
		return &transportError{fmt.Errorf("%s %s: truncated response: got %d of %d bytes", method, u, len(rb), resp.ContentLength)}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &AuthError{fmt.Sprintf("HTTP 401 from %s", method)}
	}
	if resp.StatusCode == http.StatusNotFound {
		return &notFoundError{fmt.Sprintf("%s %s: HTTP 404", method, u)}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &statusError{resp.StatusCode, fmt.Sprintf("%s %s: HTTP %d: %s", method, u, resp.StatusCode, truncate(rb))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, u, resp.StatusCode, truncate(rb))
	}
	if out != nil && len(rb) > 0 {
		if err := json.Unmarshal(rb, out); err != nil {
			return fmt.Errorf("%s %s: bad JSON in %d-byte response: %w", method, u, len(rb), err)
		}
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
