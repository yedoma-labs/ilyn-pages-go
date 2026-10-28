package ilynpages

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// nextMention finds a broken link-value that names rel="next", so that a broken header is not
// mistaken for the last page.
var nextMention = regexp.MustCompile(`(?i)\brel\s*=\s*"?[^";,]*\bnext\b`)

// ParseLinkHeader parses an RFC 8288 Link header value into a map from relation type to target
// URI reference. Relation names are lower-cased, and the first link wins for a repeated relation.
// Targets may be relative.
//
// Quoted and unquoted rel values, several relations in one rel, several links separated by
// commas, further parameters, and URIs that contain commas or semicolons are supported. An empty
// value yields an empty map. A malformed link-value is skipped silently, unless it mentions
// rel="next" and no valid next link exists. Then ParseLinkHeader returns an error that matches
// ErrInvalidLink.
func ParseLinkHeader(value string) (map[string]string, error) {
	links, malformedNext := scanLinkHeader(value)
	if _, ok := links["next"]; !ok && malformedNext {
		return nil, &PaginationError{
			Code:  ErrInvalidLink,
			Cause: errors.New(`rel="next" entry is malformed`),
		}
	}
	return links, nil
}

// NextFromLinkHeader returns the absolute URL of the rel="next" link in the Link headers of resp.
// Relative targets are resolved against the URL of the request. It returns an empty string when
// there is no next link. A malformed next link yields an error that matches ErrInvalidLink.
// Several Link header fields are combined as RFC 8288 requires.
func NextFromLinkHeader(resp *http.Response) (string, error) {
	if resp == nil {
		return "", errors.New("ilynpages: nil response")
	}
	values := resp.Header.Values("Link")
	if len(values) == 0 {
		return "", nil
	}
	links, err := ParseLinkHeader(strings.Join(values, ", "))
	if err != nil {
		return "", err
	}
	raw, ok := links["next"]
	if !ok {
		return "", nil
	}
	var base *url.URL
	if resp.Request != nil {
		base = resp.Request.URL
	}
	return resolveReference(base, raw)
}

// resolveReference resolves raw against base. Without a base, raw must be absolute.
func resolveReference(base *url.URL, raw string) (string, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return "", &PaginationError{Code: ErrInvalidLink, Cause: errors.New("next link is not a valid URL")}
	}
	if base != nil {
		ref = base.ResolveReference(ref)
	}
	if !ref.IsAbs() {
		return "", &PaginationError{Code: ErrInvalidLink, Cause: errors.New("next link is relative without a base URL")}
	}
	return ref.String(), nil
}

// scanLinkHeader returns the valid links of value. malformedNext reports a malformed link-value
// that mentions rel="next".
func scanLinkHeader(value string) (links map[string]string, malformedNext bool) {
	links = make(map[string]string)
	p := &linkParser{s: value}
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return links, malformedNext
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}

		start := p.i
		target, rels, ok := p.entry()
		if ok {
			for _, rel := range rels {
				key := strings.ToLower(rel)
				if _, dup := links[key]; !dup {
					links[key] = target
				}
			}
			continue
		}

		// Skip the malformed entry and resume at the next "<".
		end := len(p.s)
		if idx := strings.IndexByte(p.s[start+1:], '<'); idx >= 0 {
			end = start + 1 + idx
		}
		if nextMention.MatchString(p.s[start:end]) {
			malformedNext = true
		}
		p.i = end
	}
}

// linkParser is a small tokenizer for RFC 8288 link-values.
type linkParser struct {
	s string
	i int
}

func (p *linkParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// token reads an RFC 7230 token at the current position.
func (p *linkParser) token() string {
	start := p.i
	for p.i < len(p.s) && isTokenChar(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i]
}

// entry parses one link-value at the current position. ok is false for a malformed entry.
func (p *linkParser) entry() (target string, rels []string, ok bool) {
	if p.i >= len(p.s) || p.s[p.i] != '<' {
		return "", nil, false
	}
	end := strings.IndexByte(p.s[p.i+1:], '>')
	if end < 0 {
		return "", nil, false
	}
	target = strings.TrimSpace(p.s[p.i+1 : p.i+1+end])
	p.i += end + 2
	if target == "" {
		return "", nil, false
	}

	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return target, rels, true
		}
		if p.s[p.i] == ',' {
			p.i++
			return target, rels, true
		}
		if p.s[p.i] != ';' {
			return "", nil, false
		}
		p.i++
		p.skipSpace()

		name := strings.ToLower(p.token())
		if name == "" {
			return "", nil, false
		}
		p.skipSpace()
		value, hasValue, valid := p.paramValue()
		if !valid {
			return "", nil, false
		}
		if name == "rel" && hasValue {
			rels = append(rels, strings.Fields(value)...)
		}
	}
}

// paramValue reads an optional "=value" part of a parameter. hasValue is false when there is no
// "=". valid is false for an empty or unterminated value.
func (p *linkParser) paramValue() (value string, hasValue, valid bool) {
	if p.i >= len(p.s) || p.s[p.i] != '=' {
		return "", false, true
	}
	p.i++
	p.skipSpace()

	if p.i < len(p.s) && p.s[p.i] == '"' {
		p.i++
		var b strings.Builder
		for p.i < len(p.s) {
			c := p.s[p.i]
			if c == '\\' && p.i+1 < len(p.s) {
				b.WriteByte(p.s[p.i+1])
				p.i += 2
				continue
			}
			p.i++
			if c == '"' {
				return b.String(), true, true
			}
			b.WriteByte(c)
		}
		return "", false, false
	}

	// Unquoted values are read up to the next separator. This also accepts media types such as
	// application/json, which RFC 7230 does not allow unquoted.
	start := p.i
	for p.i < len(p.s) && isBareValueChar(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return "", false, false
	}
	return p.s[start:p.i], true, true
}

func isTokenChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func isBareValueChar(c byte) bool {
	return c != ';' && c != ',' && c != '"' && c != ' ' && c != '\t' && c > ' ' && c != 0x7f
}
