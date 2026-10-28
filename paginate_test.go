package ilynpages

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fetchFunc = func(context.Context, string) (Page[int], error)

// collect drains seq and returns the items yielded before the first error.
func collect[T any](seq iter.Seq2[T, error]) ([]T, error) {
	var out []T
	for item, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, item)
	}
	return out, nil
}

// serve returns a fetch function for the given pages. Cursor "" is page 1, cursor "cN" is page
// N+1. Only the last page has an empty NextCursor.
func serve(pages [][]int) fetchFunc {
	return func(_ context.Context, cursor string) (Page[int], error) {
		idx := 0
		if cursor != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
			if err != nil {
				return Page[int]{}, err
			}
			idx = n
		}
		if idx >= len(pages) {
			return Page[int]{}, fmt.Errorf("no page for cursor %q", cursor)
		}
		next := ""
		if idx+1 < len(pages) {
			next = "c" + strconv.Itoa(idx+1)
		}
		return Page[int]{Items: pages[idx], NextCursor: next}, nil
	}
}

// counted wraps fetch so that every call is counted.
func counted(calls *atomic.Int32, fetch fetchFunc) fetchFunc {
	return func(ctx context.Context, cursor string) (Page[int], error) {
		calls.Add(1)
		return fetch(ctx, cursor)
	}
}

// endless returns a fetch function that never ends. Page n yields item n-1 and cursor "cN".
func endless(withItems bool) fetchFunc {
	return func(_ context.Context, cursor string) (Page[int], error) {
		n := 0
		if cursor != "" {
			v, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
			if err != nil {
				return Page[int]{}, err
			}
			n = v
		}
		var items []int
		if withItems {
			items = []int{n}
		}
		return Page[int]{Items: items, NextCursor: "c" + strconv.Itoa(n+1)}, nil
	}
}

func TestPaginateIteratesAllPages(t *testing.T) {
	tests := []struct {
		name  string
		pages [][]int
		want  []int
	}{
		{name: "single page", pages: [][]int{{7}}, want: []int{7}},
		{name: "three pages", pages: [][]int{{1, 2}, {3}, {4, 5}}, want: []int{1, 2, 3, 4, 5}},
		{name: "single empty page", pages: [][]int{{}}, want: nil},
		{name: "empty page between items", pages: [][]int{{1}, {}, {2}}, want: []int{1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collect(Paginate(context.Background(), serve(tt.pages)))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("items = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPaginateFirstCallUsesEmptyCursor(t *testing.T) {
	var mu sync.Mutex
	var cursors []string
	recorder := func(ctx context.Context, cursor string) (Page[int], error) {
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()
		return serve([][]int{{1}, {2}, {3}})(ctx, cursor)
	}

	if _, err := collect(Paginate(context.Background(), recorder)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"", "c1", "c2"}
	if !slices.Equal(cursors, want) {
		t.Fatalf("cursors = %q, want %q", cursors, want)
	}
}

func TestPaginateGuards(t *testing.T) {
	tests := []struct {
		name      string
		build     func(calls *atomic.Int32) fetchFunc
		opts      []Option
		wantCode  error
		wantPage  int
		wantCalls int32
		wantItems []int
	}{
		{
			name: "cursor loop",
			build: func(calls *atomic.Int32) fetchFunc {
				return counted(calls, func(_ context.Context, cursor string) (Page[int], error) {
					if cursor == "" {
						return Page[int]{Items: []int{1}, NextCursor: "a"}, nil
					}
					return Page[int]{Items: []int{2}, NextCursor: "a"}, nil
				})
			},
			wantCode:  ErrCursorLoop,
			wantPage:  2,
			wantCalls: 2,
			wantItems: []int{1},
		},
		{
			name: "page limit with more pages available",
			build: func(calls *atomic.Int32) fetchFunc {
				return counted(calls, endless(true))
			},
			opts:      []Option{WithMaxPages(3)},
			wantCode:  ErrPageLimit,
			wantPage:  3,
			wantCalls: 3,
			wantItems: []int{0, 1, 2},
		},
		{
			name: "endless empty pages with next cursor",
			build: func(calls *atomic.Int32) fetchFunc {
				return counted(calls, endless(false))
			},
			wantCode:  ErrEmptyPagesLoop,
			wantPage:  3,
			wantCalls: 3,
		},
		{
			name: "empty pages below the limit are fine",
			build: func(calls *atomic.Int32) fetchFunc {
				return counted(calls, serve([][]int{{}, {}, {1}, {}, {}, {2}}))
			},
			wantCalls: 6,
			wantItems: []int{1, 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := new(atomic.Int32)
			items, err := collect(Paginate(context.Background(), tt.build(calls), tt.opts...))

			if tt.wantCode == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if !errors.Is(err, tt.wantCode) {
					t.Fatalf("error = %v, want code %v", err, tt.wantCode)
				}
				var pe *PaginationError
				if !errors.As(err, &pe) || pe.Page != tt.wantPage {
					t.Fatalf("error = %v, want page %d", err, tt.wantPage)
				}
			}
			if !slices.Equal(items, tt.wantItems) {
				t.Fatalf("items = %v, want %v", items, tt.wantItems)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("fetch calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestPaginateMaxItemsStopsWithoutError(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		wantItems []int
		wantCalls int32
	}{
		{name: "unlimited", max: 0, wantItems: []int{1, 2, 3, 4, 5}, wantCalls: 2},
		{name: "stops inside first page", max: 2, wantItems: []int{1, 2}, wantCalls: 1},
		{name: "stops at end of first page", max: 3, wantItems: []int{1, 2, 3}, wantCalls: 1},
		{name: "stops inside second page", max: 4, wantItems: []int{1, 2, 3, 4}, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := new(atomic.Int32)
			items, err := collect(Paginate(context.Background(),
				counted(calls, serve([][]int{{1, 2, 3}, {4, 5}})), WithMaxItems(tt.max)))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(items, tt.wantItems) {
				t.Fatalf("items = %v, want %v", items, tt.wantItems)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("fetch calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestPaginateInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "max pages zero", opts: []Option{WithMaxPages(0)}},
		{name: "max items negative", opts: []Option{WithMaxItems(-1)}},
		{name: "timeout zero", opts: []Option{WithTimeout(0)}},
		{name: "total timeout negative", opts: []Option{WithTotalTimeout(-time.Second)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := new(atomic.Int32)
			_, err := collect(Paginate(context.Background(), counted(calls, serve([][]int{{1}})), tt.opts...))
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("fetch was called %d times for an invalid option", calls.Load())
			}
		})
	}

	t.Run("nil fetch", func(t *testing.T) {
		_, err := collect(Paginate[int](context.Background(), nil))
		if !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("error = %v, want ErrInvalidOption", err)
		}
	})
}

func TestPaginateFetchErrorIsYieldedUnchanged(t *testing.T) {
	errBoom := errors.New("boom")
	fetch := func(ctx context.Context, cursor string) (Page[int], error) {
		if cursor == "" {
			return Page[int]{Items: []int{1}, NextCursor: "c1"}, nil
		}
		return Page[int]{}, errBoom
	}

	items, err := collect(Paginate(context.Background(), fetch))
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want errBoom", err)
	}
	var pe *PaginationError
	if errors.As(err, &pe) {
		t.Fatalf("fetch error was wrapped in PaginationError: %v", err)
	}
	if !slices.Equal(items, []int{1}) {
		t.Fatalf("items = %v, want [1]", items)
	}
}

func TestPaginateCancellation(t *testing.T) {
	t.Run("context cancelled while a request is open", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fetch := func(ctx context.Context, _ string) (Page[int], error) {
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			<-ctx.Done()
			return Page[int]{}, ctx.Err()
		}

		_, err := collect(Paginate(ctx, fetch))
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("error = %v, want ErrAborted", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want it to match context.Canceled", err)
		}
		if errors.Is(err, ErrTimeout) {
			t.Fatalf("cancellation was reported as timeout: %v", err)
		}
	})

	t.Run("context cancelled by the consumer between items", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var items []int
		var gotErr error
		for item, err := range Paginate(ctx, serve([][]int{{1, 2, 3}})) {
			if err != nil {
				gotErr = err
				break
			}
			items = append(items, item)
			if item == 1 {
				cancel()
			}
		}
		if !slices.Equal(items, []int{1}) {
			t.Fatalf("items = %v, want [1]", items)
		}
		if !errors.Is(gotErr, ErrAborted) || !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("error = %v, want ErrAborted matching context.Canceled", gotErr)
		}
	})
}

func TestPaginateTimeouts(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	tests := []struct {
		name      string
		honourCtx bool
		opts      []Option
		wantIs    error
	}{
		{
			name:      "page timeout while fetch ignores the context",
			honourCtx: false,
			opts:      []Option{WithTimeout(20 * time.Millisecond)},
			wantIs:    ErrTimeout,
		},
		{
			name:      "page timeout while fetch honours the context",
			honourCtx: true,
			opts:      []Option{WithTimeout(20 * time.Millisecond)},
			wantIs:    ErrTimeout,
		},
		{
			name:      "total timeout while fetch honours the context",
			honourCtx: true,
			opts:      []Option{WithTimeout(time.Minute), WithTotalTimeout(30 * time.Millisecond)},
			wantIs:    ErrTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetch := func(ctx context.Context, _ string) (Page[int], error) {
				if tt.honourCtx {
					<-ctx.Done()
					return Page[int]{}, ctx.Err()
				}
				<-release
				return Page[int]{}, nil
			}

			start := time.Now()
			_, err := collect(Paginate(context.Background(), fetch, tt.opts...))
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("error = %v, want %v", err, tt.wantIs)
			}
			if errors.Is(err, ErrAborted) {
				t.Fatalf("timeout was reported as abort: %v", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want it to match context.DeadlineExceeded", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("iteration took %v, the time limit was not enforced", elapsed)
			}
		})
	}
}

func TestPaginateBreakCancelsOpenRequest(t *testing.T) {
	cancelled := make(chan error, 1)
	fetch := func(ctx context.Context, cursor string) (Page[int], error) {
		if cursor == "" {
			return Page[int]{Items: []int{1, 2}, NextCursor: "next"}, nil
		}
		// The prefetch of page 2 is open while the consumer handles item 1.
		<-ctx.Done()
		cancelled <- ctx.Err()
		return Page[int]{}, ctx.Err()
	}

	for item, err := range Paginate(context.Background(), fetch) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if item != 1 {
			t.Fatalf("first item = %d, want 1", item)
		}
		break
	}

	select {
	case got := <-cancelled:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("open request ended with %v, want context.Canceled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open request was not cancelled after break")
	}
}

func TestPaginateFetchPanicIsRethrownInConsumer(t *testing.T) {
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want boom", r)
		}
	}()
	panicking := func(context.Context, string) (Page[int], error) {
		panic("boom")
	}
	for _, err := range Paginate(context.Background(), panicking) {
		_ = err
	}
	t.Fatal("expected a panic")
}

func TestPaginateErrorMessageHidesCursorValues(t *testing.T) {
	const secret = "https://api.example.test/items?token=secret123"
	fetch := func(_ context.Context, _ string) (Page[int], error) {
		return Page[int]{Items: []int{1}, NextCursor: secret}, nil
	}

	_, err := collect(Paginate(context.Background(), fetch))
	if !errors.Is(err, ErrCursorLoop) {
		t.Fatalf("error = %v, want ErrCursorLoop", err)
	}
	for _, leak := range []string{"secret123", "token=", "api.example.test"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error message %q contains %q", err.Error(), leak)
		}
	}
}

func TestPaginationErrorCodesAreDistinct(t *testing.T) {
	codes := []error{ErrCursorLoop, ErrPageLimit, ErrEmptyPagesLoop, ErrAborted, ErrTimeout,
		ErrRateLimited, ErrInvalidLink, ErrInvalidOption}
	for i, a := range codes {
		for j, b := range codes {
			if i != j && errors.Is(a, b) {
				t.Fatalf("sentinel %q matches %q", a, b)
			}
		}
	}
}
