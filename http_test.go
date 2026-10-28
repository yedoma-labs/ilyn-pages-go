package ilynpages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testPage struct {
	Items []int  `json:"items"`
	Next  string `json:"next,omitempty"`
}

func extractInts(body io.Reader) ([]int, string, error) {
	var doc testPage
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, "", err
	}
	return doc.Items, doc.Next, nil
}

// pageHandler serves pages[n-1] for page n. Next links are relative. In body mode they are in
// the JSON body, in link mode in the Link header.
func pageHandler(pages [][]int, linkHeader bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := 1
		if p := r.URL.Query().Get("page"); p != "" {
			v, err := strconv.Atoi(p)
			if err != nil || v < 1 || v > len(pages) {
				http.Error(w, "no such page", http.StatusNotFound)
				return
			}
			n = v
		}
		next := ""
		if n < len(pages) {
			next = "/items?page=" + strconv.Itoa(n+1)
		}
		doc := testPage{Items: pages[n-1]}
		if linkHeader {
			if next != "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
			}
		} else {
			doc.Next = next
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackedBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *trackedBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestHTTPPagerFollowsNextLinks(t *testing.T) {
	tests := []struct {
		name       string
		linkHeader bool
	}{
		{name: "next URL in the body", linkHeader: false},
		{name: "next URL in the Link header", linkHeader: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(pageHandler([][]int{{1, 2}, {3}, {4}}, tt.linkHeader))
			t.Cleanup(srv.Close)

			fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
			if err != nil {
				t.Fatalf("HTTPPager: %v", err)
			}
			got, err := collect(Paginate(context.Background(), fetch))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, []int{1, 2, 3, 4}) {
				t.Fatalf("items = %v, want [1 2 3 4]", got)
			}
		})
	}
}

func TestHTTPPagerRetriesRateLimit(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
	}{
		{name: "Retry-After in seconds", retryAfter: "0"},
		{name: "Retry-After as HTTP date in the past", retryAfter: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			inner := pageHandler([][]int{{1}}, false)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				inner(w, r)
			}))
			t.Cleanup(srv.Close)

			fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
			if err != nil {
				t.Fatalf("HTTPPager: %v", err)
			}
			got, err := collect(Paginate(context.Background(), fetch))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, []int{1}) {
				t.Fatalf("items = %v, want [1]", got)
			}
			if n := hits.Load(); n != 2 {
				t.Fatalf("requests = %d, want 2 (one retry)", n)
			}
		})
	}
}

func TestHTTPPagerRateLimitGivesUpAfterThreeRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
	if err != nil {
		t.Fatalf("HTTPPager: %v", err)
	}
	_, err = collect(Paginate(context.Background(), fetch))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	var pe *PaginationError
	if !errors.As(err, &pe) || pe.Page != 1 {
		t.Fatalf("error = %v, want page 1", err)
	}
	if n := hits.Load(); n != 4 {
		t.Fatalf("requests = %d, want 4 (first try plus three retries)", n)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	now := time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "missing value", value: "", want: time.Second},
		{name: "seconds", value: "2", want: 2 * time.Second},
		{name: "zero seconds", value: "0", want: 0},
		{name: "surrounding spaces", value: " 5 ", want: 5 * time.Second},
		{name: "seconds capped at 60", value: "3600", want: 60 * time.Second},
		{name: "overflowing seconds capped", value: "99999999999999999999", want: 60 * time.Second},
		{name: "HTTP date 30 seconds ahead", value: now.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second},
		{name: "HTTP date far ahead capped", value: now.Add(time.Hour).Format(http.TimeFormat), want: 60 * time.Second},
		{name: "HTTP date in the past", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0},
		{name: "garbage falls back to one second", value: "soon", want: time.Second},
		{name: "negative value falls back to one second", value: "-5", want: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryAfterDelay(tt.value, now); got != tt.want {
				t.Fatalf("retryAfterDelay(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestHTTPPagerRejectsCrossOriginNext(t *testing.T) {
	tests := []struct {
		name       string
		linkHeader bool
	}{
		{name: "body next URL", linkHeader: false},
		{name: "Link header next URL", linkHeader: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var foreignHits atomic.Int32
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				foreignHits.Add(1)
				_ = json.NewEncoder(w).Encode(testPage{Items: []int{666}})
			}))
			t.Cleanup(foreign.Close)
			foreignNext := foreign.URL + "/items?page=2"

			home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.linkHeader {
					w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, foreignNext))
					_ = json.NewEncoder(w).Encode(testPage{Items: []int{1}})
					return
				}
				_ = json.NewEncoder(w).Encode(testPage{Items: []int{1}, Next: foreignNext})
			}))
			t.Cleanup(home.Close)

			fetch, err := HTTPPager(home.Client(), home.URL+"/items", extractInts)
			if err != nil {
				t.Fatalf("HTTPPager: %v", err)
			}
			_, err = collect(Paginate(context.Background(), fetch))
			if !errors.Is(err, ErrInvalidLink) {
				t.Fatalf("error = %v, want ErrInvalidLink", err)
			}
			if n := foreignHits.Load(); n != 0 {
				t.Fatalf("foreign origin received %d requests, want 0", n)
			}
			if strings.Contains(err.Error(), foreign.URL) {
				t.Fatalf("error message contains the foreign URL: %v", err)
			}
		})
	}
}

func TestHTTPPagerRedirects(t *testing.T) {
	t.Run("redirect to another origin is refused", func(t *testing.T) {
		var foreignHits atomic.Int32
		foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			foreignHits.Add(1)
			_ = json.NewEncoder(w).Encode(testPage{Items: []int{666}})
		}))
		t.Cleanup(foreign.Close)

		home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/items", http.StatusFound)
		}))
		t.Cleanup(home.Close)

		client := home.Client()
		fetch, err := HTTPPager(client, home.URL+"/items", extractInts)
		if err != nil {
			t.Fatalf("HTTPPager: %v", err)
		}
		_, err = collect(Paginate(context.Background(), fetch))
		if !errors.Is(err, ErrInvalidLink) {
			t.Fatalf("error = %v, want ErrInvalidLink", err)
		}
		if n := foreignHits.Load(); n != 0 {
			t.Fatalf("foreign origin received %d requests, want 0", n)
		}
		if client.CheckRedirect != nil {
			t.Fatal("HTTPPager modified the caller's client")
		}
	})

	t.Run("redirect within the same origin is followed", func(t *testing.T) {
		inner := pageHandler([][]int{{5}}, false)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/items", http.StatusFound)
				return
			}
			inner(w, r)
		}))
		t.Cleanup(srv.Close)

		fetch, err := HTTPPager(srv.Client(), srv.URL+"/start", extractInts)
		if err != nil {
			t.Fatalf("HTTPPager: %v", err)
		}
		got, err := collect(Paginate(context.Background(), fetch))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(got, []int{5}) {
			t.Fatalf("items = %v, want [5]", got)
		}
	})
}

func TestHTTPPagerStatusAndDecodeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
	if err != nil {
		t.Fatalf("HTTPPager: %v", err)
	}
	_, err = collect(Paginate(context.Background(), fetch))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want an error with the status code", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatalf("500 was reported as rate limit: %v", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error message contains the URL: %v", err)
	}
}

func TestHTTPPagerClosesEveryBody(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "success", status: http.StatusOK, body: `{"items":[1]}`},
		{name: "server error", status: http.StatusInternalServerError, body: "oops", wantErr: true},
		{name: "invalid JSON", status: http.StatusOK, body: "{", wantErr: true},
		{name: "rate limit after retries", status: http.StatusTooManyRequests, body: "slow down", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opened, closed atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				opened.Add(1)
				return &http.Response{
					StatusCode: tt.status,
					Header:     http.Header{"Retry-After": {"0"}},
					Body:       &trackedBody{Reader: strings.NewReader(tt.body), closed: &closed},
					Request:    req,
				}, nil
			})}

			fetch, err := HTTPPager(client, "https://api.example.test/items", extractInts)
			if err != nil {
				t.Fatalf("HTTPPager: %v", err)
			}
			_, err = fetch(context.Background(), "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if opened.Load() == 0 || opened.Load() != closed.Load() {
				t.Fatalf("opened %d bodies, closed %d", opened.Load(), closed.Load())
			}
		})
	}
}

func TestHTTPPagerTimeouts(t *testing.T) {
	t.Run("page timeout while the server does not answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)

		fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
		if err != nil {
			t.Fatalf("HTTPPager: %v", err)
		}
		_, err = collect(Paginate(context.Background(), fetch, WithTimeout(50*time.Millisecond)))
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("error = %v, want ErrTimeout", err)
		}
	})

	t.Run("page timeout interrupts a long Retry-After wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)

		fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
		if err != nil {
			t.Fatalf("HTTPPager: %v", err)
		}
		start := time.Now()
		_, err = collect(Paginate(context.Background(), fetch, WithTimeout(50*time.Millisecond)))
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("error = %v, want ErrTimeout", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waited %v for Retry-After, the page timeout was not applied", elapsed)
		}
	})
}

func TestHTTPPagerContextAbort(t *testing.T) {
	srv := httptest.NewServer(pageHandler([][]int{{1}}, false))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fetch, err := HTTPPager(srv.Client(), srv.URL+"/items", extractInts)
	if err != nil {
		t.Fatalf("HTTPPager: %v", err)
	}
	_, err = collect(Paginate(ctx, fetch))
	if !errors.Is(err, ErrAborted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want ErrAborted matching context.Canceled", err)
	}
}

func TestHTTPPagerInvalidStartURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "relative", url: "/items"},
		{name: "unsupported scheme", url: "ftp://example.test/items"},
		{name: "missing host", url: "https:///items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := HTTPPager[int](nil, tt.url, extractInts); err == nil {
				t.Fatalf("expected an error for start URL %q", tt.url)
			}
		})
	}

	t.Run("nil extract", func(t *testing.T) {
		if _, err := HTTPPager[int](nil, "https://example.test/items", nil); err == nil {
			t.Fatal("expected an error for nil extract")
		}
	})
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "identical", a: "https://api.example.test/x", b: "https://api.example.test/y", want: true},
		{name: "host case differs", a: "https://API.example.test/x", b: "https://api.example.test/x", want: true},
		{name: "default port equals implicit port", a: "https://api.example.test:443/x", b: "https://api.example.test/x", want: true},
		{name: "different port", a: "https://api.example.test:8443/x", b: "https://api.example.test/x", want: false},
		{name: "different scheme", a: "http://api.example.test/x", b: "https://api.example.test/x", want: false},
		{name: "different host", a: "https://evil.example.test/x", b: "https://api.example.test/x", want: false},
		{name: "subdomain is another host", a: "https://a.api.example.test/x", b: "https://api.example.test/x", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := parseHTTPURL(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := parseHTTPURL(tt.b)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameOrigin(a, b); got != tt.want {
				t.Fatalf("sameOrigin(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
