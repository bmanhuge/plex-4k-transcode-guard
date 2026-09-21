package guard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnauthorized is returned when Plex rejects the token.
var ErrUnauthorized = errors.New("plex rejected the token (HTTP 401)")

// HTTPError reports an unexpected HTTP status.
type HTTPError struct {
	Op     string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: unexpected HTTP status %d", e.Op, e.Status)
}

// maxResponseBytes bounds every response body read from Plex.
const maxResponseBytes = 16 << 20

// TokenProvider supplies the Plex token for authenticated requests.
type TokenProvider interface {
	Token() (string, error)
	Invalidate()
}

// PlexAPI is the subset of the Plex HTTP API used by the guard. It is an
// interface so the runner can be tested without a network.
type PlexAPI interface {
	Ready(ctx context.Context) error
	Sessions(ctx context.Context) ([]byte, error)
	Metadata(ctx context.Context, ratingKey string) ([]byte, error)
	Terminate(ctx context.Context, sessionID, reason string) error
}

// Client talks to a local Plex Media Server. The token is only ever sent in
// the X-Plex-Token header, never in a URL, so it cannot leak through error
// strings, access logs or proxies.
type Client struct {
	base    *url.URL
	http    *http.Client
	tokens  TokenProvider
	product string
	version string
}

// NewClient creates a Client for baseURL with a bounded timeout applied to
// every request. Proxies from the environment are deliberately ignored:
// the guard only talks to the loopback address.
func NewClient(baseURL string, timeout time.Duration, tokens TokenProvider, version string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing plex url: %w", err)
	}
	if tokens == nil {
		return nil, errors.New("token provider is required")
	}
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	return &Client{
		base:    u,
		http:    &http.Client{Transport: transport, Timeout: timeout},
		tokens:  tokens,
		product: "plex-4k-transcode-guard",
		version: version,
	}, nil
}

// Ready reports whether Plex answers /identity, which needs no token.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.get(ctx, "identity", "/identity", nil, false)
	return err
}

// Sessions returns the raw XML of /status/sessions.
func (c *Client) Sessions(ctx context.Context) ([]byte, error) {
	return c.get(ctx, "sessions", "/status/sessions", nil, true)
}

// Metadata returns the raw XML of /library/metadata/{ratingKey}.
func (c *Client) Metadata(ctx context.Context, ratingKey string) ([]byte, error) {
	ratingKey = strings.TrimSpace(ratingKey)
	if ratingKey == "" || strings.ContainsAny(ratingKey, "/?#") {
		return nil, fmt.Errorf("metadata: invalid rating key %q", ratingKey)
	}
	return c.get(ctx, "metadata", "/library/metadata/"+url.PathEscape(ratingKey), nil, true)
}

// Terminate stops a session through GET /status/sessions/terminate with the
// Session id (the id attribute of the <Session> element, not sessionKey)
// and the reason shown to the viewer. This matches python-plexapi's
// PlexSession.stop() and Tautulli's get_sessions_terminate().
func (c *Client) Terminate(ctx context.Context, sessionID, reason string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("terminate: session id is required")
	}
	q := url.Values{}
	q.Set("sessionId", sessionID)
	q.Set("reason", reason)
	_, err := c.get(ctx, "terminate", "/status/sessions/terminate", q, true)
	return err
}

func (c *Client) get(ctx context.Context, op, path string, query url.Values, withToken bool) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", op, err)
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("User-Agent", c.product+"/"+c.version)
	req.Header.Set("X-Plex-Product", c.product)
	req.Header.Set("X-Plex-Version", c.version)
	req.Header.Set("X-Plex-Client-Identifier", c.product)
	req.Header.Set("X-Plex-Device-Name", c.product)
	req.Header.Set("X-Plex-Platform", "Docker")
	if withToken {
		token, err := c.tokens.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: reading response: %w", op, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%s: response larger than %d bytes", op, maxResponseBytes)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		c.tokens.Invalidate()
		return nil, fmt.Errorf("%s: %w", op, ErrUnauthorized)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &HTTPError{Op: op, Status: resp.StatusCode}
	}
	return body, nil
}
