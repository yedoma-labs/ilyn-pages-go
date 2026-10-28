package ilynpages

import (
	"errors"
	"strconv"
	"strings"
)

// Sentinel error codes. Test a returned error with errors.Is, for example
// errors.Is(err, ilynpages.ErrCursorLoop). The messages never contain URLs or query values.
var (
	// ErrCursorLoop reports that the server returned a cursor that was already used.
	ErrCursorLoop = errors.New("cursor loop")
	// ErrPageLimit reports that more pages were available after the page limit was reached.
	ErrPageLimit = errors.New("page limit reached")
	// ErrEmptyPagesLoop reports consecutive empty pages that still carried a next cursor.
	ErrEmptyPagesLoop = errors.New("empty pages loop")
	// ErrAborted reports that the context passed to Paginate was done. The error also matches
	// the context's error (context.Canceled or context.DeadlineExceeded) with errors.Is.
	ErrAborted = errors.New("pagination aborted")
	// ErrTimeout reports that a page or the total run exceeded its time limit.
	ErrTimeout = errors.New("timeout")
	// ErrRateLimited reports that HTTP 429 persisted after the retries of HTTPPager.
	ErrRateLimited = errors.New("rate limited")
	// ErrInvalidLink reports a malformed next link, or a next link to another origin.
	ErrInvalidLink = errors.New("invalid link")
	// ErrInvalidOption reports an invalid option value.
	ErrInvalidOption = errors.New("invalid option")
)

// PaginationError is returned by the iterator and the helpers for guard violations.
type PaginationError struct {
	// Code is one of the sentinel errors of this package, for example ErrCursorLoop.
	Code error
	// Page is the 1-based number of the page that caused the error. It is 0 when unknown.
	Page int
	// Cause is an optional underlying error, for example context.Canceled.
	Cause error
}

// Error returns a message without URLs or query values.
func (e *PaginationError) Error() string {
	var b strings.Builder
	b.WriteString("ilynpages: ")
	if e.Code != nil {
		b.WriteString(e.Code.Error())
	} else {
		b.WriteString("pagination error")
	}
	if e.Page > 0 {
		b.WriteString(" on page ")
		b.WriteString(strconv.Itoa(e.Page))
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes the code and the cause to errors.Is and errors.As.
func (e *PaginationError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Code != nil {
		errs = append(errs, e.Code)
	}
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	return errs
}
