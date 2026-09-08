package router

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
)

// requestBody is a buffered request body in both the form the client sent and
// the form the router can read.
//
// Claude Code gzips request bodies once they get large, which is most real
// sessions. Without decoding, the model name cannot be read out of the JSON
// and every request would look like one for an unknown model and be passed
// through to Anthropic.
type requestBody struct {
	// Raw is exactly what the client sent, still under its content coding.
	Raw []byte
	// JSON is Raw with the content coding removed. It is nil when the coding
	// is one this proxy cannot decode.
	JSON []byte
	// Encoding is the Content-Encoding the client declared, lowercased.
	Encoding string
}

// Readable reports whether the body could be decoded and inspected.
func (b *requestBody) Readable() bool { return b.JSON != nil }

// Compressed reports whether Raw is under a content coding.
func (b *requestBody) Compressed() bool { return b.Encoding != "" && b.Encoding != "identity" }

// readBody buffers the request body and decodes it for inspection.
func readBody(req *http.Request) (*requestBody, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("request has no body")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(raw) > maxRequestBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
	}

	body := &requestBody{
		Raw:      raw,
		Encoding: strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))),
	}
	if !body.Compressed() {
		body.JSON = raw
		return body, nil
	}

	decoded, err := decodeBody(raw, body.Encoding)
	if err != nil {
		// An undecodable body is not fatal: it can still be forwarded
		// untouched, it just cannot be routed by the model it names.
		logx.Warnf("request body is %s encoded and could not be read (%v); passing it through", body.Encoding, err)
		return body, nil
	}
	body.JSON = decoded
	return body, nil
}

// decodeBody removes a content coding. Only the codings a client actually
// applies to a request body are supported; a response-only coding such as
// brotli or zstd is reported as unsupported rather than guessed at.
func decodeBody(raw []byte, encoding string) ([]byte, error) {
	var reader io.Reader
	switch encoding {
	case "gzip", "x-gzip":
		gzipReader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	case "deflate":
		// "deflate" is zlib on the wire, but raw deflate is common enough
		// in the field to be worth the fallback.
		if zlibReader, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer func() { _ = zlibReader.Close() }()
			reader = zlibReader
		} else {
			reader = flate.NewReader(bytes.NewReader(raw))
		}
	default:
		return nil, fmt.Errorf("unsupported content coding %q", encoding)
	}

	decoded, err := io.ReadAll(io.LimitReader(reader, maxRequestBody+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maxRequestBody {
		return nil, fmt.Errorf("decoded body exceeds %d bytes", maxRequestBody)
	}
	return decoded, nil
}

// forward puts the body the client sent back on the request, byte for byte,
// so a pass-through request reaches Anthropic exactly as it was made.
func (b *requestBody) forward(req *http.Request) {
	setBody(req, b.Raw)
}

// forwardModified puts a rewritten body on the request. The content coding is
// dropped because the new bytes are not under it.
func (b *requestBody) forwardModified(req *http.Request, body []byte) {
	req.Header.Del("Content-Encoding")
	setBody(req, body)
}

func setBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// A buffered body is no longer chunked.
	req.TransferEncoding = nil
}
