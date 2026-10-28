# ilyn-pages-go

Iterate over every page of a paginated API with a Go 1.23 iterator, and stop safely when the
server misbehaves. The library has no dependencies outside the standard library.

```sh
go get github.com/yedoma-labs/ilyn-pages-go
```

## The problem

**Offset pagination is unreliable when the data changes.** A request such as
`?offset=200&limit=100` counts rows. If a row is inserted or deleted between two requests, the
rows shift. The next page then skips a row you have not seen, or returns a row you already have.

**Cursor pagination fixes the shift, but the loop needs guards.** The server returns an opaque
cursor for the next page. A naive loop has four failure modes:

- The server returns the same cursor again, and the loop runs forever.
- A buggy server never ends the pages, and the loop has no limit.
- The server answers with HTTP 429 (Too Many Requests) in the middle of the loop, and the loop
  fails on the first 429.
- The next link is in the response body for one endpoint and in the `Link` header (RFC 8288)
  for another, so each endpoint needs its own loop.

`ilyn-pages-go` provides one iterator with the guards, and one HTTP adapter that handles both
ways of sending the next link.

## Quickstart

### Next link in the `Link` header

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	ilyn "github.com/yedoma-labs/ilyn-pages-go"
)

type order struct {
	ID string `json:"id"`
}

// The body holds only the items. The next link comes from the Link header.
func extractOrders(body io.Reader) ([]order, string, error) {
	var doc struct {
		Data []order `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, "", err
	}
	return doc.Data, "", nil
}

func main() {
	fetch, err := ilyn.HTTPPager(http.DefaultClient, "https://api.example.com/v1/orders", extractOrders)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for order, err := range ilyn.Paginate(ctx, fetch, ilyn.WithMaxItems(10_000)) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(order.ID)
	}
}
```

### Next URL in the response body

```go
func extractOrdersWithBodyNext(body io.Reader) ([]order, string, error) {
	var doc struct {
		Data []order `json:"data"`
		Next string  `json:"next"` // for example "/v1/orders?after=abc123"
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, "", err
	}
	return doc.Data, doc.Next, nil
}
```

A relative next URL is resolved against the URL of the request. A next URL to another origin is
refused with `ErrInvalidLink`.

### Your own fetch function

`Paginate` does not depend on HTTP. Any cursor-based source works:

```go
fetch := func(ctx context.Context, cursor string) (ilyn.Page[Item], error) {
	items, next, err := client.ListItems(ctx, cursor) // your own call
	if err != nil {
		return ilyn.Page[Item]{}, err
	}
	return ilyn.Page[Item]{Items: items, NextCursor: next}, nil
}

for item, err := range ilyn.Paginate(ctx, fetch) {
	// ...
}
```

### Link header parser

```go
links, err := ilyn.ParseLinkHeader(`<https://api.example.com/items?page=2>; rel="next"`)
// links["next"] == "https://api.example.com/items?page=2"
```

## Guards

| Guard | Default | Behaviour |
| --- | --- | --- |
| `WithMaxPages(n)` | 100 | More pages after page `n` yield `ErrPageLimit`. |
| `WithMaxItems(n)` | 0 (unlimited) | Stops without error after `n` items. |
| `WithTimeout(d)` | 30 s per page | A page that takes longer yields `ErrTimeout`. The clock starts when the request starts. |
| `WithTotalTimeout(d)` | 0 (none) | The whole iteration yields `ErrTimeout` after `d`. |
| Cursor repeated | always on | Yields `ErrCursorLoop`. |
| Empty pages | 3 in a row | Three empty pages with a next cursor yield `ErrEmptyPagesLoop`. |
| Context done | always on | Yields `ErrAborted`. It also matches `context.Canceled` or `context.DeadlineExceeded`. |
| HTTP 429 | 3 retries | Waits for `Retry-After` (seconds or HTTP date, capped at 60 s), then `ErrRateLimited`. |
| Cross-origin next link or redirect | refused | Yields `ErrInvalidLink`. No request is sent to the other origin. |

Errors are `*PaginationError` values. Test the code with `errors.Is` and read the page number
from the `Page` field:

```go
var pe *ilyn.PaginationError
if errors.As(err, &pe) {
	log.Printf("stopped on page %d: %v", pe.Page, pe.Code)
}
if errors.Is(err, ilyn.ErrCursorLoop) {
	// the server repeated a cursor
}
```

Error messages contain no full URLs and no query values.

A `break` in the `for range` loop cancels the request that is still open. The iterator requests
the next page while the items of the current page are handled.

## Offset pagination

If an API only offers `offset` and `limit`, this library cannot make the results stable. Ask the
API provider for cursor pagination, or for a snapshot or a sort key that does not change. Until
then, treat offset results as a best effort and check for duplicates or gaps yourself.

## Development

```sh
gofmt -l .
go vet ./...
go test -race -count=1 ./...
go build ./...
```

Commit messages follow Conventional Commits. Releases are created by semantic-release from `main`.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability.

## License

MIT. See [LICENSE](LICENSE).
