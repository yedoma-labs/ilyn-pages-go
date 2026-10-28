package ilynpages

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"testing"
)

func TestParseLinkHeader(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "standard next link",
			in:   `<https://api.example.com/items?page=2>; rel="next"`,
			want: map[string]string{"next": "https://api.example.com/items?page=2"},
		},
		{
			name: "unquoted rel",
			in:   `<https://a.example/2>; rel=next`,
			want: map[string]string{"next": "https://a.example/2"},
		},
		{
			name: "upper case rel name and value",
			in:   `<https://a.example/2>; REL="Next"`,
			want: map[string]string{"next": "https://a.example/2"},
		},
		{
			name: "several links",
			in:   `<https://a.example/1>; rel="first", <https://a.example/3>; rel="last", <https://a.example/2>; rel="next"`,
			want: map[string]string{
				"first": "https://a.example/1",
				"last":  "https://a.example/3",
				"next":  "https://a.example/2",
			},
		},
		{
			name: "several relations in one rel",
			in:   `<https://a.example/2>; rel="next last"`,
			want: map[string]string{"next": "https://a.example/2", "last": "https://a.example/2"},
		},
		{
			name: "quoted parameters are ignored",
			in:   `<https://a.example/2>; title="Seite 2"; type="application/json"; rel="next"`,
			want: map[string]string{"next": "https://a.example/2"},
		},
		{
			name: "unquoted media type parameter",
			in:   `<https://a.example/2>; type=application/json; rel="next"`,
			want: map[string]string{"next": "https://a.example/2"},
		},
		{
			name: "escaped quote inside a quoted parameter",
			in:   `<https://a.example/2>; title="say \"hi\""; rel="next"`,
			want: map[string]string{"next": "https://a.example/2"},
		},
		{
			name: "comma and semicolon inside the URI",
			in:   `<https://a.example/items?ids=1,2;3>; rel="next"`,
			want: map[string]string{"next": "https://a.example/items?ids=1,2;3"},
		},
		{
			name: "relative target is returned as written",
			in:   `</items?page=2>; rel="next"`,
			want: map[string]string{"next": "/items?page=2"},
		},
		{
			name: "first link wins for a repeated relation",
			in:   `<https://a.example/x>; rel="next", <https://a.example/y>; rel="next"`,
			want: map[string]string{"next": "https://a.example/x"},
		},
		{name: "empty header", in: ``, want: map[string]string{}},
		{name: "only whitespace", in: "   ", want: map[string]string{}},
		{name: "garbage without next", in: `hello world`, want: map[string]string{}},
		{
			name: "broken entry without next is ignored",
			in:   `<https://a.example/1>; rel="prev", <https://a.example/2; rel="x"`,
			want: map[string]string{"prev": "https://a.example/1"},
		},
		{name: "broken next entry", in: `<https://a.example/2; rel="next"`, wantErr: true},
		{name: "unterminated quote in next", in: `<https://a.example/2>; rel="next`, wantErr: true},
		{name: "empty target in next", in: `<>; rel="next"`, wantErr: true},
		{
			name:    "broken next after a valid prev",
			in:      `<https://a.example/1>; rel="prev", <https://a.example/2>; rel="next`,
			wantErr: true,
		},
		{
			name: "valid next wins over a broken duplicate",
			in:   `<https://a.example/2>; rel="next", <https://a.example/3; rel="next"`,
			want: map[string]string{"next": "https://a.example/2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLinkHeader(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidLink) {
					t.Fatalf("error = %v, want ErrInvalidLink", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("links = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextFromLinkHeader(t *testing.T) {
	base, err := url.Parse("https://api.example.com/v1/items?page=1")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		headers []string
		want    string
		wantErr bool
	}{
		{
			name:    "absolute next",
			headers: []string{`<https://api.example.com/v1/items?page=2>; rel="next"`},
			want:    "https://api.example.com/v1/items?page=2",
		},
		{
			name:    "absolute path is resolved against the request",
			headers: []string{`</v1/items?page=2>; rel="next"`},
			want:    "https://api.example.com/v1/items?page=2",
		},
		{
			name:    "target without angle brackets is a broken next link",
			headers: []string{`items?page=2; rel="next"`},
			wantErr: true,
		},
		{
			name:    "relative reference with angle brackets",
			headers: []string{`<items?page=2>; rel="next"`},
			want:    "https://api.example.com/v1/items?page=2",
		},
		{name: "no link header", headers: nil, want: ""},
		{
			name:    "no next relation",
			headers: []string{`<https://api.example.com/v1/items?page=1>; rel="last"`},
			want:    "",
		},
		{
			name: "several Link fields are combined",
			headers: []string{
				`<https://api.example.com/v1/items?page=1>; rel="prev"`,
				`<https://api.example.com/v1/items?page=2>; rel="next"`,
			},
			want: "https://api.example.com/v1/items?page=2",
		},
		{
			name:    "broken next link",
			headers: []string{`<https://api.example.com/2; rel="next"`},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}, Request: &http.Request{URL: base}}
			for _, h := range tt.headers {
				resp.Header.Add("Link", h)
			}
			got, err := NextFromLinkHeader(resp)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got next %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("next = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNextFromLinkHeaderWithoutRequest(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Link": {`</items?page=2>; rel="next"`}}}
	if _, err := NextFromLinkHeader(resp); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("error = %v, want ErrInvalidLink for a relative link without a base", err)
	}

	if _, err := NextFromLinkHeader(nil); err == nil {
		t.Fatal("expected an error for a nil response")
	}
}
