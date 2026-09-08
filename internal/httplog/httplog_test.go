package httplog

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLogs redirects logx output for the duration of the test by
// intercepting the underlying writer is not exposed, so instead we drive the
// package through behavior we can observe directly (response contents,
// Flush/Unwrap plumbing) rather than scraping log text. The one place log
// text matters (size/duration formatting) is tested directly on the
// formatting helpers.

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		-1:          "?",
		0:           "0b",
		1:           "1b",
		1023:        "1023b",
		1024:        "1Kb",
		1536:        "1.5Kb",
		1024 * 1024: "1Mb",
	}
	for n, want := range cases {
		if got := formatSize(n); got != want {
			t.Fatalf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(1500 * time.Microsecond); got != "2ms" {
		t.Fatalf("formatDuration(1.5ms) = %q, want rounded to 2ms", got)
	}
	if got := formatDuration(0); got != "0s" {
		t.Fatalf("formatDuration(0) = %q, want 0s", got)
	}
}

func TestTransportLogsOnBodyClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello, world"))
	}))
	defer upstream.Close()

	transport := &Transport{Next: http.DefaultTransport}
	resp, err := transport.RoundTrip(mustRequest(t, http.MethodGet, upstream.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// Nothing has counted anything yet; only reading + closing does.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello, world" {
		t.Fatalf("body = %q", body)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTransportCountsBytesReadNotContentLength(t *testing.T) {
	// A streaming (chunked, no Content-Length) response is exactly the case
	// this package exists for: the Codex backend never sends one.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("chunk"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	var reportedSize int64 = -1
	var reportedStatus int
	done := make(chan struct{})

	transport := &Transport{Next: &instrumentedTransport{next: http.DefaultTransport, onDone: func(n int64, status int) {
		reportedSize, reportedStatus = n, status
		close(done)
	}}}
	resp, err := transport.RoundTrip(mustRequest(t, http.MethodGet, upstream.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.ContentLength >= 0 {
		t.Fatalf("test setup is wrong: response has a Content-Length of %d, want streaming (-1)", resp.ContentLength)
	}

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	<-done

	if n != 15 { // "chunk" x3
		t.Fatalf("read %d bytes", n)
	}
	if reportedSize != 15 {
		t.Fatalf("reported size = %d, want the bytes actually read (15), not the absent Content-Length", reportedSize)
	}
	if reportedStatus != http.StatusOK {
		t.Fatalf("reported status = %d", reportedStatus)
	}
}

// instrumentedTransport lets a test observe what Transport's countingBody
// reports without scraping log output: it wraps the response body a second
// time, purely to learn the final byte count and status once the outer
// Transport's own wrapper (and therefore ours) has been closed.
type instrumentedTransport struct {
	next   http.RoundTripper
	onDone func(n int64, status int)
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	status := resp.StatusCode
	resp.Body = &countingBody{ReadCloser: resp.Body, onClose: func(n int64, _ error) {
		t.onDone(n, status)
	}}
	return resp, nil
}

func TestTransportReturnsTheUnderlyingError(t *testing.T) {
	transport := &Transport{Next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrClosedPipe
	})}
	if _, err := transport.RoundTrip(mustRequest(t, http.MethodGet, "http://example.invalid", nil)); err != io.ErrClosedPipe {
		t.Fatalf("error = %v, want it passed through unchanged", err)
	}
}

func TestTransportDefaultsToDefaultTransport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	transport := &Transport{} // Next left nil
	resp, err := transport.RoundTrip(mustRequest(t, http.MethodGet, upstream.URL, nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCountingBodyReportsOnlyOnce(t *testing.T) {
	var calls int
	body := &countingBody{
		ReadCloser: io.NopCloser(strings.NewReader("abc")),
		onClose:    func(int64, error) { calls++ },
	}
	_, _ = io.ReadAll(body)
	_ = body.Close()
	_ = body.Close()
	_ = body.Close()
	if calls != 1 {
		t.Fatalf("onClose called %d times, want exactly once", calls)
	}
}

func TestLogRecordsStatusAndSize(t *testing.T) {
	handler := Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("0123456789"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("request body"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want the handler's own status to reach the client", rec.Code)
	}
	if rec.Body.String() != "0123456789" {
		t.Fatalf("body = %q, want it passed through unchanged", rec.Body.String())
	}
}

func TestLogDefaultsStatusTo200WhenHandlerNeverCallsWriteHeader(t *testing.T) {
	handler := Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (net/http's own default)", rec.Code)
	}
}

func TestLogPreservesRequestHostInTheURL(t *testing.T) {
	// Once TLS has been terminated, req.URL carries only the path; the
	// meaningful URL for a log line has to come from Host instead.
	var captured string
	handler := Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = requestURL(r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "api.anthropic.com"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if captured != "https://api.anthropic.com/v1/models" {
		t.Fatalf("requestURL = %q", captured)
	}
}

// TestLogSupportsResponseControllerFlush is the regression this package
// exists to not cause: the streaming Codex path calls
// http.NewResponseController(w).Flush() to push SSE chunks to the client as
// they are translated. If Log's wrapper does not expose the underlying
// ResponseWriter via Unwrap, that call silently fails and streaming breaks.
func TestLogSupportsResponseControllerFlush(t *testing.T) {
	var flushErr error
	handler := Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first chunk"))
		flushErr = http.NewResponseController(w).Flush()
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if flushErr != nil {
		t.Fatalf("ResponseController.Flush() = %v, want it to find httptest.ResponseRecorder's Flusher through Unwrap", flushErr)
	}
	if !rec.Flushed {
		t.Fatal("the underlying recorder never saw a Flush call")
	}
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
