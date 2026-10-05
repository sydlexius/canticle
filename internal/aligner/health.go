package aligner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	// healthTimeout bounds one GET /health probe: the sidecar answers
	// liveness without loading a model, so a slow answer is unavailable.
	healthTimeout = 3 * time.Second
	// maxHealthResponseSize bounds how much of the probe's body is read.
	maxHealthResponseSize = 8 << 10
)

// ErrUnhealthy is returned by Health when the sidecar answered without the
// liveness shape: a non-200 (a 3xx included), an oversized or non-JSON body,
// or a status other than "ok".
var ErrUnhealthy = errors.New("aligner: sidecar is not healthy")

// Health probes GET /health and returns nil only for a 200 whose JSON body
// carries status "ok". It uses Align's http.Client (a redirect is never
// followed) under its own 3 s deadline (independent of the client's timeout), and neither consults nor moves the
// breaker: availability is not evidence about alignment calls.
func (c *HTTPClient) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	target, err := c.healthURL()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("aligner: build health request")
	}
	// A shallow copy with no Timeout: the alignment timeout must not cut the
	// probe short, so the request context is its only deadline. Transport and
	// CheckRedirect (no redirect is followed) are shared; c.client is untouched.
	probe := *c.client
	probe.Timeout = 0
	res, err := probe.Do(req)
	if err != nil {
		return scrubTransportError(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnhealthy, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHealthResponseSize+1))
	if err != nil {
		return fmt.Errorf("aligner: read health response: %w", err)
	}
	if len(body) > maxHealthResponseSize {
		return fmt.Errorf("%w: response too large", ErrUnhealthy)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: undecodable response", ErrUnhealthy)
	}
	if payload.Status != "ok" {
		return fmt.Errorf("%w: status field is not ok", ErrUnhealthy)
	}
	return nil
}

// scrubTransportError drops everything a transport error can embed (the
// request URL, the dialed address, a looked-up or certificate host) and keeps
// only a coarse class. An ALLOWLIST of causes known not to carry a host passes
// through wrapped, so errors.Is/As still see a cancel, deadline, errno or EOF;
// every other cause is replaced by fixed text.
func scrubTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	// Wrap the matched SENTINEL, never err: err may be a *net.OpError whose
	// text carries the dialed address around that very cause.
	var matched []error
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded,
		os.ErrDeadlineExceeded, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, cause) {
			matched = append(matched, cause)
		}
	}
	if len(matched) > 0 {
		return fmt.Errorf("aligner: health request: %w", errors.Join(matched...))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return errors.New("aligner: health request: host lookup failed (not found)")
		case dnsErr.IsTimeout:
			return errors.New("aligner: health request: host lookup failed (timeout)")
		}
		return errors.New("aligner: health request: host lookup failed")
	}
	var (
		hostErr x509.HostnameError
		authErr x509.UnknownAuthorityError
		invErr  x509.CertificateInvalidError
		verErr  *tls.CertificateVerificationError
		alert   tls.AlertError
		hdrErr  tls.RecordHeaderError
	)
	if errors.As(err, &hostErr) || errors.As(err, &authErr) || errors.As(err, &invErr) ||
		errors.As(err, &verErr) || errors.As(err, &alert) || errors.As(err, &hdrErr) {
		return errors.New("aligner: health request: tls failure")
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("aligner: health request: %w", errno)
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return errors.New("aligner: health request: connect failure")
	}
	return errors.New("aligner: health request: failed")
}

// healthURL builds the probe URL from the parsed base: a trailing /align path
// segment is dropped (a host named align is untouched), the query and
// fragment are discarded, and /health is appended with no doubled slash. The
// error never carries the configured url.
func (c *HTTPClient) healthURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", errors.New("aligner: configured url is not parsable")
	}
	escaped := strings.TrimSuffix(u.EscapedPath(), "/")
	escaped = strings.TrimSuffix(escaped, "/align") + "/health"
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", errors.New("aligner: configured url is not parsable")
	}
	u.Path, u.RawPath = decoded, escaped
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String(), nil
}
