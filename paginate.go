// Package ilynpages iterates over all items of a paginated API.
//
// Paginate drives a page-fetching function with cursor semantics and guards the loop against
// endless pagination: repeated cursors, too many pages, endless empty pages and timeouts.
// HTTPPager is an HTTP adapter that reads next links from the response body or from the
// RFC 8288 Link header. It retries HTTP 429 responses and refuses next links to another origin.
package ilynpages

import (
	"context"
	"fmt"
	"iter"
	"time"
)

const (
	defaultMaxPages = 100
	defaultTimeout  = 30 * time.Second
	maxEmptyPages   = 3
)

// Page is one response of a paginated API.
type Page[T any] struct {
	// Items are the items of this page. They may be empty.
	Items []T
	// NextCursor identifies the next page. An empty string marks the last page.
	NextCursor string
}

// Option configures Paginate.
type Option func(*config)

type config struct {
	maxPages     int
	maxItems     int
	timeout      time.Duration
	totalTimeout time.Duration
	err          error
}

// fail records the first invalid option value. Paginate yields it before any request is made.
func (c *config) fail(msg string) {
	if c.err == nil {
		c.err = fmt.Errorf("%w: %s", ErrInvalidOption, msg)
	}
}

// WithMaxPages sets the maximum number of pages that may be requested. Default: 100. When more
// pages are available after the last allowed page, Paginate yields ErrPageLimit.
func WithMaxPages(n int) Option {
	return func(c *config) {
		if n < 1 {
			c.fail("WithMaxPages needs n >= 1")
			return
		}
		c.maxPages = n
	}
}

// WithMaxItems stops the iteration without error after n items. Default: 0, which means
// unlimited.
func WithMaxItems(n int) Option {
	return func(c *config) {
		if n < 0 {
			c.fail("WithMaxItems needs n >= 0")
			return
		}
		c.maxItems = n
	}
}

// WithTimeout sets the time limit for a single page request. Default: 30 seconds. The clock
// starts when the request starts, which can be before the items of the previous page are
// consumed.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		if d <= 0 {
			c.fail("WithTimeout needs a positive duration")
			return
		}
		c.timeout = d
	}
}

// WithTotalTimeout sets the time limit for the whole iteration. Default: 0, which means no
// limit.
func WithTotalTimeout(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			c.fail("WithTotalTimeout needs a non-negative duration")
			return
		}
		c.totalTimeout = d
	}
}

// Paginate iterates over all items of a paginated API.
//
// The first call of fetch uses an empty cursor. Each following call uses the NextCursor of the
// previous page. An empty NextCursor ends the iteration. Each item is yielded with a nil error.
// A failure is yielded once as a zero item with the error, and the iteration ends.
//
// Guards:
//   - a cursor that was already used yields ErrCursorLoop,
//   - more than WithMaxPages pages yields ErrPageLimit,
//   - three consecutive empty pages with a next cursor yield ErrEmptyPagesLoop,
//   - a done context yields ErrAborted, a time limit yields ErrTimeout,
//   - reaching WithMaxItems stops without error.
//
// The next page is requested while the items of the current page are yielded. When the consumer
// stops early (break), the context of an open request is cancelled. A fetch function must honour
// its context. Errors returned by fetch are yielded unchanged.
func Paginate[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (Page[T], error), opts ...Option) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		cfg := config{maxPages: defaultMaxPages, timeout: defaultTimeout}
		for _, opt := range opts {
			if opt != nil {
				opt(&cfg)
			}
		}
		if cfg.err != nil {
			yield(zero, cfg.err)
			return
		}
		if fetch == nil {
			yield(zero, fmt.Errorf("%w: fetch must not be nil", ErrInvalidOption))
			return
		}
		if ctx == nil {
			ctx = context.Background()
		}

		total, cancelTotal := newTotalContext(ctx, cfg.totalTimeout)
		defer cancelTotal()
		p := &pager[T]{user: ctx, total: total, fetch: fetch, timeout: cfg.timeout}

		cur := p.start("", 1)
		defer func() {
			if cur != nil {
				cur.cancel()
			}
		}()

		seen := make(map[string]struct{})
		emitted := 0
		emptyStreak := 0

		for {
			c := cur
			cur = nil
			page, err := p.await(c)
			if err != nil {
				yield(zero, err)
				return
			}

			next := page.NextCursor
			if next != "" {
				if _, dup := seen[next]; dup {
					yield(zero, &PaginationError{Code: ErrCursorLoop, Page: c.page})
					return
				}
			}

			switch {
			case len(page.Items) > 0:
				emptyStreak = 0
			case next != "":
				emptyStreak++
				if emptyStreak >= maxEmptyPages {
					yield(zero, &PaginationError{
						Code:  ErrEmptyPagesLoop,
						Page:  c.page,
						Cause: fmt.Errorf("%d consecutive empty pages with a next cursor", maxEmptyPages),
					})
					return
				}
			}

			more := next != ""
			if cfg.maxItems > 0 && emitted+len(page.Items) >= cfg.maxItems {
				more = false
			}

			var limitErr error
			if more {
				seen[next] = struct{}{}
				if c.page >= cfg.maxPages {
					limitErr = &PaginationError{
						Code:  ErrPageLimit,
						Page:  c.page,
						Cause: fmt.Errorf("more pages after the limit of %d pages", cfg.maxPages),
					}
					more = false
				} else {
					cur = p.start(next, c.page+1)
				}
			}

			for _, item := range page.Items {
				if err := p.check(c.page, nil); err != nil {
					yield(zero, err)
					return
				}
				if !yield(item, nil) {
					return
				}
				emitted++
				if cfg.maxItems > 0 && emitted >= cfg.maxItems {
					return
				}
			}

			if limitErr != nil {
				yield(zero, limitErr)
				return
			}
			if !more {
				return
			}
		}
	}
}

// newTotalContext derives the context of the whole run. A zero duration means no time limit.
func newTotalContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

// pager starts page requests and classifies the reasons why a request or a run stops.
type pager[T any] struct {
	user    context.Context
	total   context.Context
	fetch   func(context.Context, string) (Page[T], error)
	timeout time.Duration
}

// call is one page request that runs in its own goroutine.
type call[T any] struct {
	page   int
	ctx    context.Context
	cancel context.CancelFunc
	result chan result[T]
}

// result is the outcome of a fetch call. A panic in fetch is passed on to the consumer.
type result[T any] struct {
	page     Page[T]
	err      error
	panicked bool
	panicVal any
}

// start begins a request for page number page. The goroutine sends its result into a buffered
// channel, so it never blocks when nobody waits for it anymore.
func (p *pager[T]) start(cursor string, page int) *call[T] {
	pctx, cancel := context.WithTimeout(p.total, p.timeout)
	c := &call[T]{page: page, ctx: pctx, cancel: cancel, result: make(chan result[T], 1)}
	go func() {
		c.result <- invoke(p.fetch, pctx, cursor)
	}()
	return c
}

// invoke runs fetch and turns a panic into a result, so that the consumer re-raises it.
func invoke[T any](fetch func(context.Context, string) (Page[T], error), ctx context.Context, cursor string) (res result[T]) {
	defer func() {
		if r := recover(); r != nil {
			res = result[T]{panicked: true, panicVal: r}
		}
	}()
	page, err := fetch(ctx, cursor)
	return result[T]{page: page, err: err}
}

// await waits for the outcome of c. It returns when the page arrives, the page timeout passes or
// the run is stopped.
func (p *pager[T]) await(c *call[T]) (Page[T], error) {
	defer c.cancel()
	select {
	case res := <-c.result:
		return p.finish(c, res)
	case <-c.ctx.Done():
		select {
		case res := <-c.result:
			return p.finish(c, res)
		default:
		}
		if err := p.check(c.page, c.ctx); err != nil {
			return Page[T]{}, err
		}
		return Page[T]{}, &PaginationError{Code: ErrAborted, Page: c.page}
	}
}

// finish converts the result of a call into a page or an error. Stop reasons take precedence
// over errors returned by fetch, because fetch often reports a cancelled context as its own
// error.
func (p *pager[T]) finish(c *call[T], res result[T]) (Page[T], error) {
	if res.panicked {
		panic(res.panicVal)
	}
	if res.err == nil {
		return res.page, nil
	}
	if err := p.check(c.page, c.ctx); err != nil {
		return Page[T]{}, err
	}
	// Adapters such as HTTPPager do not know the page number. Set it here.
	if pe, ok := res.err.(*PaginationError); ok && pe.Page == 0 {
		cp := *pe
		cp.Page = c.page
		return Page[T]{}, &cp
	}
	return Page[T]{}, res.err
}

// check reports why the run must stop: the caller's context, the total time limit or, when pctx
// is given, the page time limit. It returns nil while the run may continue.
func (p *pager[T]) check(page int, pctx context.Context) error {
	if err := p.user.Err(); err != nil {
		return &PaginationError{Code: ErrAborted, Page: page, Cause: err}
	}
	if err := p.total.Err(); err != nil {
		return &PaginationError{Code: ErrTimeout, Page: page, Cause: fmt.Errorf("total time limit: %w", err)}
	}
	if pctx != nil {
		if err := pctx.Err(); err != nil {
			return &PaginationError{Code: ErrTimeout, Page: page, Cause: fmt.Errorf("page time limit: %w", err)}
		}
	}
	return nil
}
