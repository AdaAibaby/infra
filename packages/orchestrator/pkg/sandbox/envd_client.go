//go:build linux

package sandbox

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

// envdReadMaxBytes caps one envd Connect message, on the wire and after
// decompression; envd emits process output in chunks of at most 32 KiB.
const envdReadMaxBytes = 1 << 20

// envdBodyMaxBytes caps a status or error body read from envd.
const envdBodyMaxBytes = 64 << 10

// ErrEnvdBodyTooLarge is a status or error body from envd over envdBodyMaxBytes.
var ErrEnvdBodyTooLarge = fmt.Errorf("envd response body is over %d KiB", envdBodyMaxBytes>>10)

// ReadEnvdBody reads a status or error body from envd, refusing one over
// envdBodyMaxBytes. The returned bytes are at most envdBodyMaxBytes.
func ReadEnvdBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(&cappedBody{Reader: r, left: envdBodyMaxBytes + 1})
	if errors.Is(err, ErrEnvdBodyTooLarge) {
		return body[:envdBodyMaxBytes], err
	}

	return body, err
}

// cappedBody fails the read that would take it past left-1 bytes.
type cappedBody struct {
	io.Reader

	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, ErrEnvdBodyTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.Reader.Read(p)
	b.left -= int64(n)
	if b.left <= 0 {
		return n, ErrEnvdBodyTooLarge
	}

	return n, err
}

// errorBodyCap caps every non-200 body as it is read, before and after gzip:
// connect-go decodes a unary call's error body without its read limit. The
// streaming path never reads the body, so nothing here may read it eagerly.
type errorBodyCap struct {
	hc connect.HTTPClient
}

func (c errorBodyCap) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil || resp.StatusCode == http.StatusOK {
		return resp, err
	}

	// connect-go decodes by the first value alone, so the cap must own that one.
	gzipped := resp.Header.Get("Content-Encoding") == "gzip"
	if gzipped {
		resp.Header.Del("Content-Encoding")
	}
	resp.Body = &cappedErrorBody{Closer: resp.Body, wire: &cappedBody{Reader: resp.Body, left: envdBodyMaxBytes + 1}, gzipped: gzipped}

	return resp, nil
}

// cappedErrorBody gunzips, if asked, on first read; wire spans every read.
type cappedErrorBody struct {
	io.Closer

	wire    *cappedBody
	gzipped bool
	r       io.Reader
}

func (b *cappedErrorBody) Read(p []byte) (int, error) {
	if b.r == nil {
		if !b.gzipped {
			b.r = b.wire
		} else {
			zr, err := gzip.NewReader(b.wire)
			if err != nil {
				return 0, err
			}
			b.r = &cappedBody{Reader: zr, left: envdBodyMaxBytes + 1}
		}
	}

	return b.r.Read(p)
}

// NewEnvdHTTPClient returns a client for host-to-envd HTTP requests over
// sandboxHttpTransport. A zero timeout means none.
func NewEnvdHTTPClient(timeout time.Duration) *http.Client {
	c := envdHTTPClient(sandboxHttpTransport, timeout)

	return &c
}

// NewEnvdProcessClient returns a Process client for the envd at baseURL.
func NewEnvdProcessClient(baseURL string, timeout time.Duration) processconnect.ProcessClient {
	return processconnect.NewProcessClient(errorBodyCap{hc: NewEnvdHTTPClient(timeout)}, baseURL, connect.WithReadMaxBytes(envdReadMaxBytes))
}

// envdHTTPClient never follows a redirect: the guest writes envd's responses.
func envdHTTPClient(rt http.RoundTripper, timeout time.Duration) http.Client {
	return http.Client{
		Timeout:   timeout,
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
