package guard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticTokens struct {
	mu    sync.Mutex
	token string
	err   error
}

func (s *staticTokens) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.err
}

type recordedRequest struct {
	method, path, rawQuery, token, accept, requestURI string
}

func newRecordingServer(t *testing.T, status int, body string) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Plex-Token"), r.Header.Get("Accept"), r.RequestURI})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestClientTerminateExactRequest(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	tokens := &staticTokens{token: fixtureToken}
	c, err := NewClient(srv.URL, time.Second, tokens, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Terminate(context.Background(), "e6gmj1bjf7jlbz7hu5cqcags", DefaultStopMessage); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests: %+v", *reqs)
	}
	r := (*reqs)[0]
	if r.method != http.MethodGet || r.path != "/status/sessions/terminate" {
		t.Fatalf("method/path: %+v", r)
	}
	const wantQuery = "reason=You+are+not+allowed+to+transcode+4K+content%2C+please+play+the+normal+resolution+version.&sessionId=e6gmj1bjf7jlbz7hu5cqcags"
	if r.rawQuery != wantQuery {
		t.Fatalf("query:\n got %s\nwant %s", r.rawQuery, wantQuery)
	}
	q, err := url.ParseQuery(r.rawQuery)
	if err != nil || q.Get("reason") != DefaultStopMessage || q.Get("sessionId") != "e6gmj1bjf7jlbz7hu5cqcags" {
		t.Fatalf("decoded query: %v err=%v", q, err)
	}
	if r.token != fixtureToken {
		t.Fatalf("token header %q", r.token)
	}
	if strings.Contains(r.rawQuery, fixtureToken) {
		t.Fatal("token must never be in the URL")
	}
}

func TestClientTerminateUnicodeReason(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	reason := "Bitte 4K nicht transkodieren – danke & tschüss"
	if err := c.Terminate(context.Background(), "abc", reason); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery((*reqs)[0].rawQuery)
	if q.Get("reason") != reason {
		t.Fatalf("round trip %q", q.Get("reason"))
	}
}

func TestClientTerminateRequiresSessionID(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	if err := c.Terminate(context.Background(), "  ", "x"); err == nil {
		t.Fatal("expected error")
	}
	if len(*reqs) != 0 {
		t.Fatal("no request must be sent without a session id")
	}
}

func TestClientReadyAndSessionsHeaders(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, `<MediaContainer size="0"></MediaContainer>`)
	tokens := &staticTokens{token: fixtureToken}
	c, _ := NewClient(srv.URL+"/", time.Second, tokens, "test")
	if err := c.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Metadata(context.Background(), "769612"); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 3 {
		t.Fatalf("requests: %+v", *reqs)
	}
	if (*reqs)[0].path != "/identity" || (*reqs)[0].token != "" {
		t.Fatalf("identity must be unauthenticated: %+v", (*reqs)[0])
	}
	if (*reqs)[1].path != "/status/sessions" || (*reqs)[1].token != fixtureToken || (*reqs)[1].accept != "application/xml" {
		t.Fatalf("sessions: %+v", (*reqs)[1])
	}
	if (*reqs)[2].path != "/library/metadata/769612" || (*reqs)[2].token != fixtureToken {
		t.Fatalf("metadata: %+v", (*reqs)[2])
	}
}

func TestClientMetadataEscapesKeyOnce(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	if _, err := c.Metadata(context.Background(), "a b%c"); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if r.path != "/library/metadata/a b%c" || r.requestURI != "/library/metadata/a%20b%25c" {
		t.Fatalf("path %q uri %q", r.path, r.requestURI)
	}
}

func TestClientMetadataValidatesKey(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	for _, key := range []string{"", " ", "a/b", "1?x=1", "#"} {
		if _, err := c.Metadata(context.Background(), key); err == nil {
			t.Fatalf("key %q must be rejected", key)
		}
	}
	if len(*reqs) != 0 {
		t.Fatal("invalid keys must not produce requests")
	}
}

func TestClientUnauthorized(t *testing.T) {
	srv, _ := newRecordingServer(t, 401, "")
	tokens := &staticTokens{token: "t0ken-value"}
	c, _ := NewClient(srv.URL, time.Second, tokens, "test")
	_, err := c.Sessions(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if strings.Contains(err.Error(), "t0ken-value") {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestClientHTTPError(t *testing.T) {
	srv, _ := newRecordingServer(t, 503, strings.Repeat("busy", 1<<12))
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	err := c.Terminate(context.Background(), "abc", "x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 503 || he.Op != "terminate" {
		t.Fatalf("got %v", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{token: "t0ken-value"}, "test")
	err := c.Terminate(context.Background(), "abc", "x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusFound || hits != 1 {
		t.Fatalf("redirect must not be followed: err=%v hits=%d", err, hits)
	}
}

func TestClientTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c, _ := NewClient(srv.URL, 200*time.Millisecond, &staticTokens{token: "t0ken-value"}, "test")
	start := time.Now()
	_, err := c.Sessions(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var ne net.Error
	if !(errors.As(err, &ne) && ne.Timeout()) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not a timeout: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestClientContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c, _ := NewClient(srv.URL, 10*time.Second, &staticTokens{token: "t0ken-value"}, "test")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := c.Sessions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestClientBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i <= maxResponseBytes/len(chunk); i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient(srv.URL, 10*time.Second, &staticTokens{token: "t0ken-value"}, "test")
	_, err := c.Sessions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("want size error, got %v", err)
	}
}

func TestClientIgnoresProxyEnvironment(t *testing.T) {
	// Go never proxies loopback destinations, so a request-level check would
	// pass vacuously; assert the transport configuration directly, and that a
	// non-loopback host with a bogus proxy set still resolves the proxy to nil.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	c, _ := NewClient("http://plex.example.internal:32400", time.Second, &staticTokens{token: "t0ken-value"}, "test")
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport %T", c.http.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("transport must not consult proxy environment variables")
	}
}

func TestClientTokenErrorPropagates(t *testing.T) {
	srv, reqs := newRecordingServer(t, 200, "")
	c, _ := NewClient(srv.URL, time.Second, &staticTokens{err: ErrNoToken}, "test")
	_, err := c.Sessions(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("got %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatal("no request without a token")
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("http://127.0.0.1:32400", time.Second, nil, "test"); err == nil {
		t.Fatal("token provider is required")
	}
	if _, err := NewClient("::bad", time.Second, &staticTokens{}, "test"); err == nil {
		t.Fatal("bad url must fail")
	}
}
