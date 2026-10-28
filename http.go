package ilynpages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxRateLimitRetries = 3
	defaultRetryAfter   = time.Second
	maxRetryAfter       = 60 * time.Second
	maxRedirects        = 10
	maxDrainBytes       = 64 << 10
)

// HTTPPager returns a fetch function for Paginate that loads pages over HTTP GET.
//
// The first page is startURL. Every later cursor is an absolute URL. extract decodes one response
// body into items and returns the next URL from the body, or an empty string. When the body has no
// next URL, the rel="next" link of the Link header is used (RFC 8288). Relative next URLs are
// resolved against the URL of the request.
//
// Next URLs and redirects must keep the origin (scheme, host and port) of startURL. Otherwise the
// request fails with ErrInvalidLink and no request is sent to the other origin. The client passed
// in is not modified.
//
// HTTP 429 waits for the Retry-After value (seconds or HTTP date, capped at 60 seconds) and
// retries the same page up to 3 times. Then it fails with ErrRateLimited. The response body is
// always closed. Other non-2xx responses fail with an error that contains only the status code.
//
// A nil client means http.DefaultClient.
func HTTPPager[T any](client *http.Client, startURL string, extract func(body io.Reader) (items []T, next string, err error)) (func(ctx context.Context, cursor string) (Page[T], error), error) {
	if extract == nil {
		return nil, errors.New("ilynpages: extract must not be nil")
	}
	start, err := parseHTTPURL(startURL)
	if err != nil {
		return nil, fmt.Errorf("ilynpages: start URL: %w", err)
	}
	base := client
	if base == nil {
		base = http.DefaultClient
	}

	// Copy the client so that the caller's client keeps its own redirect policy.
	c := *base
	userCheck := base.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, start) {
			return crossOriginError()
		}
		if userCheck != nil {
			return userCheck(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}

	return func(ctx context.Context, cursor string) (Page[T], error) {
		target := start
		if cursor != "" {
			u, err := parseHTTPURL(cursor)
			if err != nil {
				return Page[T]{}, &PaginationError{Code: ErrInvalidLink, Cause: err}
			}
			if !sameOrigin(u, start) {
				return Page[T]{}, crossOriginError()
			}
			target = u
		}
		return fetchPage(ctx, &c, target, start, extract)
	}, nil
}

// fetchPage performs the request for target and retries HTTP 429 responses.
func fetchPage[T any](ctx context.Context, client *http.Client, target, start *url.URL, extract func(io.Reader) ([]T, string, error)) (Page[T], error) {
	retries := 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return Page[T]{}, errors.New("ilynpages: could not build request")
		}
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return Page[T]{}, transportError(err)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
			drainAndClose(resp.Body)
			if retries >= maxRateLimitRetries {
				return Page[T]{}, &PaginationError{
					Code:  ErrRateLimited,
					Cause: fmt.Errorf("HTTP 429 after %d retries", retries),
				}
			}
			retries++
			if err := sleepContext(ctx, delay); err != nil {
				return Page[T]{}, err
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			drainAndClose(resp.Body)
			return Page[T]{}, fmt.Errorf("ilynpages: unexpected HTTP status %d", resp.StatusCode)
		}

		items, bodyNext, err := extract(resp.Body)
		if err != nil {
			drainAndClose(resp.Body)
			return Page[T]{}, fmt.Errorf("ilynpages: decode page: %w", err)
		}
		next, err := nextCursor(resp, bodyNext, target, start)
		drainAndClose(resp.Body)
		if err != nil {
			return Page[T]{}, err
		}
		return Page[T]{Items: items, NextCursor: next}, nil
	}
}

// nextCursor returns the absolute URL of the next page. A next URL from the body wins over the
// Link header. The URL must have the origin of start.
func nextCursor(resp *http.Response, bodyNext string, target, start *url.URL) (string, error) {
	raw := bodyNext
	if raw == "" {
		link, err := NextFromLinkHeader(resp)
		if err != nil {
			return "", err
		}
		raw = link
	}
	if raw == "" {
		return "", nil
	}
	next, err := resolveReference(target, raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(next)
	if err != nil {
		return "", &PaginationError{Code: ErrInvalidLink, Cause: errors.New("next link is not a valid URL")}
	}
	if !sameOrigin(u, start) {
		return "", crossOriginError()
	}
	return next, nil
}

// crossOriginError is returned for next links and redirects to another origin. The message does
// not contain the URL.
func crossOriginError() error {
	return &PaginationError{
		Code:  ErrInvalidLink,
		Cause: errors.New("link points to another origin and was not followed"),
	}
}

// transportError removes the URL from errors of http.Client, which would otherwise be part of
// the message. A PaginationError from CheckRedirect is returned as it is.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var pe *PaginationError
	if errors.As(err, &pe) {
		return pe
	}
	return fmt.Errorf("ilynpages: request failed: %w", err)
}

// parseHTTPURL parses an absolute http or https URL. The error message does not contain the URL.
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("URL must be an absolute http or https URL")
	}
	return u, nil
}

// sameOrigin reports whether a and b have the same scheme, host and effective port.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

// effectivePort returns the explicit port or the default port of the scheme.
func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// retryAfterDelay converts a Retry-After value into a delay. The value is either a number of
// seconds or an HTTP date. A missing or unparsable value gives one second. The delay is capped at
// 60 seconds and is never negative.
func retryAfterDelay(value string, now time.Time) time.Duration {
	v := strings.TrimSpace(value)
	if v == "" {
		return defaultRetryAfter
	}
	if isDigits(v) {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return defaultRetryAfter
	}
	return min(max(at.Sub(now), 0), maxRetryAfter)
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainAndClose discards a bounded part of the body so that the connection can be reused, and
// closes it.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	_ = body.Close()
}
