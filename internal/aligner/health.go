package aligner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
// followed) under its own 3 s deadline, and neither consults nor moves the
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
	res, err := c.client.Do(req)
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

// scrubTransportError drops the request URL (userinfo, path, query) and the
// dialed address that Go's transport errors embed, keeping only the
// underlying cause so errors.Is/As still see a cancel, deadline or errno.
func scrubTransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return fmt.Errorf("aligner: health request %s: %w", oe.Op, oe.Err)
	}
	return fmt.Errorf("aligner: health request: %w", err)
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
	p := strings.TrimSuffix(u.Path, "/")
	p = strings.TrimSuffix(p, "/align")
	u.Path, u.RawPath = p+"/health", ""
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String(), nil
}
