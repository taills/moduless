// Package pathmatch compiles the glob patterns plugins use to declare which
// request paths their filters apply to.
//
// Matching runs on every request that reaches the gateway, so Match performs
// no allocation: it neither splits the path nor uses regexp, walking the
// string by index instead. There is a zero-allocation regression test for this.
package pathmatch

import (
	"fmt"
	"slices"
	"strings"
)

type kind uint8

const (
	kindAll      kind = iota // "**" — every path
	kindExact                // "/api/health"
	kindPrefix               // "/api/**"
	kindSegments             // "/api/*/items" or "/api/*/items/**"
)

// Pattern is a compiled path glob.
//
// Supported syntax:
//
//	/exact/path     matches only that path
//	/prefix/**      matches the prefix and anything below it
//	/a/*/c          '*' matches exactly one path segment
//	**              matches everything
//
// '**' is only valid as the final segment. Allowing it in the middle would
// require backtracking, which buys very little for gateway routing and costs
// both clarity and predictable performance.
type Pattern struct {
	raw  string
	kind kind

	// exact holds the literal path for kindExact.
	// prefix holds the path up to (not including) the trailing "/**" for
	// kindPrefix, so "/api/**" stores "/api".
	literal string

	segs        []string // kindSegments; "*" marks a single-segment wildcard
	trailingAll bool     // kindSegments ending in "/**"
}

// Compile parses a pattern. An empty pattern is rejected: silently treating it
// as match-everything would make a typo in a manifest apply a filter to the
// entire gateway.
func Compile(pattern string) (Pattern, error) {
	if pattern == "" {
		return Pattern{}, fmt.Errorf("pathmatch: empty pattern")
	}
	if pattern == "**" || pattern == "/**" {
		return Pattern{raw: pattern, kind: kindAll}, nil
	}
	if !strings.HasPrefix(pattern, "/") {
		return Pattern{}, fmt.Errorf("pathmatch: pattern %q must start with '/'", pattern)
	}

	body := pattern
	trailingAll := false
	if strings.HasSuffix(body, "/**") {
		trailingAll = true
		body = strings.TrimSuffix(body, "/**")
	}
	if strings.Contains(body, "**") {
		return Pattern{}, fmt.Errorf("pathmatch: '**' is only allowed as the final segment in %q", pattern)
	}

	segs := splitSegments(body)
	for _, s := range segs {
		if s != "*" && strings.Contains(s, "*") {
			return Pattern{}, fmt.Errorf(
				"pathmatch: '*' must be a whole segment in %q (partial wildcards like 'foo*' are not supported)", pattern)
		}
	}

	hasWildcard := slices.Contains(segs, "*")

	switch {
	case !hasWildcard && trailingAll:
		return Pattern{raw: pattern, kind: kindPrefix, literal: body}, nil
	case !hasWildcard:
		return Pattern{raw: pattern, kind: kindExact, literal: body}, nil
	default:
		return Pattern{raw: pattern, kind: kindSegments, segs: segs, trailingAll: trailingAll}, nil
	}
}

// MustCompile is Compile for patterns known good at build time.
func MustCompile(pattern string) Pattern {
	p, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return p
}

func (p Pattern) String() string { return p.raw }

// Match reports whether path is covered by the pattern. It allocates nothing.
func (p Pattern) Match(path string) bool {
	switch p.kind {
	case kindAll:
		return true

	case kindExact:
		return path == p.literal

	case kindPrefix:
		// "/api/**" covers "/api" itself as well as everything beneath it, but
		// must not match "/apifoo".
		if !strings.HasPrefix(path, p.literal) {
			return false
		}
		rest := path[len(p.literal):]
		return rest == "" || rest == "/" || rest[0] == '/'

	case kindSegments:
		return p.matchSegments(path)

	default:
		return false
	}
}

func (p Pattern) matchSegments(path string) bool {
	rest := path
	for _, want := range p.segs {
		var got string
		got, rest = nextSegment(rest)
		if got == "" {
			return false // path ran out of segments
		}
		if want != "*" && want != got {
			return false
		}
	}
	if p.trailingAll {
		return true // anything below the matched prefix is included
	}
	return rest == "" || rest == "/"
}

// nextSegment returns the first path segment and the remainder, using slicing
// so nothing is allocated. Leading slashes are skipped.
func nextSegment(path string) (seg, rest string) {
	for len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if path == "" {
		return "", ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i], path[i:]
	}
	return path, ""
}

// splitSegments is used only at compile time, so allocating here is fine.
func splitSegments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// Set is an ordered group of patterns; a path matches if any member does.
type Set struct {
	patterns []Pattern
	// matchAll short-circuits when any member is "**", which is common for
	// security filters that inspect every request.
	matchAll bool
}

// CompileSet compiles several patterns. An empty list matches nothing, so a
// filter that declares no paths is inert rather than global.
func CompileSet(patterns []string) (Set, error) {
	var s Set
	for _, raw := range patterns {
		p, err := Compile(raw)
		if err != nil {
			return Set{}, err
		}
		if p.kind == kindAll {
			s.matchAll = true
		}
		s.patterns = append(s.patterns, p)
	}
	return s, nil
}

// Match reports whether any pattern in the set covers path.
func (s Set) Match(path string) bool {
	if s.matchAll {
		return true
	}
	for _, p := range s.patterns {
		if p.Match(path) {
			return true
		}
	}
	return false
}

// Len reports how many patterns the set holds.
func (s Set) Len() int { return len(s.patterns) }
