// Package httplog logs every HTTP request the proxy makes or serves.
//
// Two wrappers cover the two directions: Transport logs outbound requests (to
// Anthropic, to the Codex backend, to the OAuth token endpoint — anything
// dialed through it), and Log wraps an http.Handler to log inbound requests
// (everything Claude Code sends the proxy). Both log the same fields —
// method, URL, request/response size, status, duration — so the two
// directions read the same way in the log.
package httplog

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
)

// Transport wraps an http.RoundTripper to log every request it sends.
//
// The response is logged when its body is closed, not when RoundTrip
// returns: a streaming response (Codex's Responses API never sends
// Content-Length) would otherwise be logged with an unknown size, or would
// have to be buffered in full just to count it. Logging on Close reports the
// real transferred size and the real duration — including how long a stream
// stayed open — at the cost of nothing appearing in the log for a response
// whose body a caller never closes. Every call site in this codebase closes
// its response bodies, so that cost is not paid in practice.
type Transport struct {
	// Next is the transport to call. http.DefaultTransport is used when nil.
	Next http.RoundTripper
	// Label tags each log line, so requests from different clients (the
	// codex backend, the OAuth token endpoint, plain pass-through) can be
	// told apart at a glance.
	Label string
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}

	start := time.Now()
	method := req.Method
	url := req.URL.String()
	reqSize := req.ContentLength

	resp, err := next.RoundTrip(req)
	if err != nil {
		logx.Warnf("http %sout %s %s -> error after %s: %v", t.tag(), method, url, formatDuration(time.Since(start)), err)
		return nil, err
	}

	resp.Body = &countingBody{
		ReadCloser: resp.Body,
		onClose: func(n int64, closeErr error) {
			dur := time.Since(start)
			if closeErr != nil && !errors.Is(closeErr, io.EOF) {
				logx.Infof("http %sout %s %s -> %d req=%s resp=%s in %s (body: %v)",
					t.tag(), method, url, resp.StatusCode, formatSize(reqSize), formatSize(n), formatDuration(dur), closeErr)
				return
			}
			logx.Infof("http %sout %s %s -> %d req=%s resp=%s in %s",
				t.tag(), method, url, resp.StatusCode, formatSize(reqSize), formatSize(n), formatDuration(dur))
		},
	}
	return resp, nil
}

func (t *Transport) tag() string {
	if t.Label == "" {
		return ""
	}
	return t.Label + " "
}

// countingBody counts the bytes read through it and reports the total, and
// any error other than a clean EOF, exactly once when it is closed.
type countingBody struct {
	io.ReadCloser
	n       int64
	readErr error
	onClose func(n int64, err error)
	closed  bool
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		c.readErr = err
	}
	return n, err
}

func (c *countingBody) Close() error {
	err := c.ReadCloser.Close()
	if !c.closed {
		c.closed = true
		reportErr := c.readErr
		if reportErr == nil && err != nil {
			reportErr = err
		}
		c.onClose(c.n, reportErr)
	}
	return err
}

// Log wraps handler to log every inbound request: the client's method, URL,
// request size, and — once the handler finishes — the response status, size
// and how long the request took.
func Log(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		handler.ServeHTTP(rec, req)

		dur := time.Since(start)
		logx.Infof("http in  %s %s -> %d req=%s resp=%s in %s",
			req.Method, requestURL(req), rec.status, formatSize(req.ContentLength), formatSize(rec.written), formatDuration(dur))
	})
}

// requestURL renders the URL the way it is meaningful for this proxy: with
// the Host the client asked for, since req.URL itself carries only the path
// once TLS has been terminated.
func requestURL(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" {
		return req.URL.RequestURI()
	}
	scheme := "https"
	if req.TLS == nil && req.URL.Scheme != "" {
		scheme = req.URL.Scheme
	}
	return scheme + "://" + host + req.URL.RequestURI()
}

// statusWriter records the status code and byte count of a response as it is
// written, without buffering it.
//
// Unwrap exposes the underlying ResponseWriter so http.ResponseController —
// which streaming handlers use to flush and to hijack — finds the real
// http.Flusher / http.Hijacker on the other side instead of failing on this
// wrapper, which implements neither directly.
type statusWriter struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func formatSize(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return itoa(n) + "b"
	}
	div, exp := int64(unit), 0
	for next := n / unit; next >= unit; next /= unit {
		div *= unit
		exp++
	}
	units := "KMGT"
	value := float64(n) / float64(div)
	return trimFloat(value) + string(units[exp]) + "b"
}

func formatDuration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

// itoa and trimFloat avoid pulling in strconv/fmt just to format a byte
// count; both are called on the hot logging path for every request.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trimFloat(v float64) string {
	scaled := int64(v*10 + 0.5)
	whole := scaled / 10
	frac := scaled % 10
	if frac == 0 {
		return itoa(whole)
	}
	return itoa(whole) + "." + itoa(frac)
}
