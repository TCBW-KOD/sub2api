// SPDX-License-Identifier: LGPL-3.0-only
// Package cprnative implements the protocol-preserving HTTP boundary to a CPR
// upstream. It deliberately knows nothing about Codex tool or model schemas.
// Authentication, account selection and billing remain the host's responsibility.
package cprnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Capture records at most Limit bytes without disrupting the host's existing
// body-size limiter. Overflow MUST be checked before using Bytes for forwarding.
// Like an HTTP request body, Capture must be read/written by only one goroutine.
type Capture struct {
	Bytes    []byte
	Limit    int
	Overflow bool
}

func (c *Capture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := c.Limit - len(c.Bytes)
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		c.Overflow = true
		p = p[:remaining]
	}
	c.Bytes = append(c.Bytes, p...)
	return n, nil
}

type RequestInfo struct {
	Model       string
	Stream      bool
	Effort      string
	ServiceTier string
}

// ValidateJSON rejects ambiguous duplicate keys, non-UTF8 and excessive nesting.
// It does not normalize, rename, add, or delete any data.
func ValidateJSON(body []byte) error {
	if !utf8.Valid(body) {
		return errors.New("request must be UTF-8 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 128 {
			return errors.New("JSON nesting exceeds 128 levels")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]struct{}{}
			for d.More() {
				t, e := d.Token()
				if e != nil {
					return e
				}
				k, ok := t.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, exists := keys[k]; exists {
					return errors.New("duplicate JSON object key")
				}
				keys[k] = struct{}{}
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			t, e := d.Token()
			if e != nil {
				return e
			}
			if t != json.Delim('}') {
				return errors.New("unclosed JSON object")
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
			t, e := d.Token()
			if e != nil {
				return e
			}
			if t != json.Delim(']') {
				return errors.New("unclosed JSON array")
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("multiple or incomplete JSON documents")
	}
	return nil
}

func InspectRequest(body []byte) (RequestInfo, error) {
	var info RequestInfo
	if err := ValidateJSON(body); err != nil {
		return info, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return info, errors.New("request must be a JSON object")
	}
	if err := json.Unmarshal(fields["model"], &info.Model); err != nil || strings.TrimSpace(info.Model) == "" {
		return info, errors.New("model must be a nonempty string")
	}
	if raw, ok := fields["stream"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return info, errors.New("stream must be boolean")
		}
		if err := json.Unmarshal(raw, &info.Stream); err != nil {
			return info, errors.New("stream must be boolean")
		}
	}
	var reasoning struct {
		Effort string `json:"effort"`
	}
	_ = json.Unmarshal(fields["reasoning"], &reasoning)
	info.Effort = reasoning.Effort
	_ = json.Unmarshal(fields["service_tier"], &info.ServiceTier)
	return info, nil
}

// PolicyFieldsEqual allows the host to detect a configured override without
// sending the overridden payload. A conflict must be rejected, not bypassed.
func PolicyFieldsEqual(a, b []byte) bool {
	var x, y map[string]json.RawMessage
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	for _, key := range []string{"model", "reasoning", "reasoning_effort", "service_tier"} {
		var xv, yv any
		dx := json.NewDecoder(bytes.NewReader(x[key]))
		dx.UseNumber()
		_ = dx.Decode(&xv)
		dy := json.NewDecoder(bytes.NewReader(y[key]))
		dy.UseNumber()
		_ = dy.Decode(&yv)
		// Absent vs explicit null is also a semantic change and remains a conflict.
		_, xp := x[key]
		_, yp := y[key]
		if xp != yp || !reflect.DeepEqual(xv, yv) {
			return false
		}
	}
	return true
}

var hopHeaders = map[string]bool{
	"connection": true, "proxy-connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

func connectionTokens(h http.Header) map[string]bool {
	tokens := map[string]bool{}
	for k, vs := range h {
		if strings.EqualFold(k, "connection") {
			for _, v := range vs {
				for _, name := range strings.Split(v, ",") {
					tokens[strings.ToLower(strings.TrimSpace(name))] = true
				}
			}
		}
	}
	return tokens
}
func requestHeaderBlocked(name string) bool {
	if hopHeaders[name] {
		return true
	}
	switch name {
	case "authorization", "x-api-key", "api-key", "x-goog-api-key", "cookie", "host", "content-length", "expect", "accept-encoding",
		"forwarded", "via", "x-real-ip", "true-client-ip", "traceparent", "tracestate", "baggage":
		return true
	}
	return strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "cf-") || strings.HasPrefix(name, "x-sub2api-")
}

// RequestHeaders forwards end-to-end fields including UNKNOWN future Codex
// headers. Only transport, downstream credentials and proxy environment fields
// are removed. Values are neither regenerated nor namespaced.
func RequestHeaders(src http.Header, token string) http.Header {
	dst := make(http.Header)
	connection := connectionTokens(src)
	for k, vs := range src {
		lower := strings.ToLower(k)
		if requestHeaderBlocked(lower) || connection[lower] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	dst.Set("Authorization", "Bearer "+token)
	// Compression is transport negotiation, not Codex payload adaptation.
	dst.Set("Accept-Encoding", "identity")
	// Suppress Go's implicit Go-http-client User-Agent when the client sent none.
	if _, exists := dst["User-Agent"]; !exists {
		dst["User-Agent"] = []string{""}
	}
	return dst
}

func ResponseHeaders(src http.Header) http.Header {
	dst := make(http.Header)
	connection := connectionTokens(src)
	for k, vs := range src {
		lower := strings.ToLower(k)
		// Never let an upstream set authentication cookies on the sub2api origin,
		// override the host's CORS/CSP security policy, or inject a redirect handler.
		if hopHeaders[lower] || connection[lower] || lower == "set-cookie" || lower == "content-length" || lower == "server" || lower == "x-powered-by" ||
			strings.HasPrefix(lower, "access-control-") || lower == "content-security-policy" || lower == "strict-transport-security" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	return dst
}

// EndpointURL only accepts an administrator-supplied /v1 base and known paths.
// The host must still apply its existing URL allowlist / SSRF checks.
func EndpointURL(base, endpoint, rawQuery string) (string, error) {
	if endpoint != "responses" && endpoint != "models" {
		return "", errors.New("unsupported CPR endpoint")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("invalid CPR base URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return "", errors.New("CPR base URL must be HTTP(S), without credentials, query or fragment")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/v1") || u.RawPath != "" {
		return "", errors.New("CPR base URL must end in /v1")
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errors.New("invalid query string")
	}
	for key := range q {
		switch strings.ToLower(key) {
		case "api_key", "api-key", "apikey", "key", "access_token", "token", "authorization":
			return "", errors.New("credentials in query strings are not supported")
		}
	}
	u.Path = path + "/" + endpoint
	u.RawQuery = rawQuery
	return u.String(), nil
}
func NewRequest(ctx context.Context, base, endpoint, method, rawQuery string, headers http.Header, body []byte, token string) (*http.Request, error) {
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("missing or invalid CPR API key")
	}
	target, err := EndpointURL(base, endpoint, rawQuery)
	if err != nil {
		return nil, err
	}
	if (endpoint == "responses" && method != http.MethodPost) || (endpoint == "models" && method != http.MethodGet) {
		return nil, errors.New("unsupported CPR HTTP method")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construct CPR request: %w", err)
	}
	req.Header = RequestHeaders(headers, token)
	if endpoint == "responses" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// A stateful POST must not become automatically replayable because a
	// client supplied an Idempotency-Key. CPR owns protocol-level recovery.
	if method == http.MethodPost {
		req.GetBody = nil
	}
	return req, nil
}
