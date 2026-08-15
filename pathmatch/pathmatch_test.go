package pathmatch

import "testing"

func TestCompileRejectsBadPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{name: "empty", pattern: ""},
		{name: "no leading slash", pattern: "api/**"},
		{name: "doublestar in the middle", pattern: "/api/**/items"},
		{name: "partial wildcard", pattern: "/api/foo*/items"},
		{name: "partial wildcard suffix", pattern: "/api/*bar"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(tc.pattern); err == nil {
				t.Errorf("Compile(%q) succeeded, want an error", tc.pattern)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// exact
		{name: "exact hit", pattern: "/api/health", path: "/api/health", want: true},
		{name: "exact miss", pattern: "/api/health", path: "/api/healthz", want: false},
		{name: "exact rejects subpath", pattern: "/api/health", path: "/api/health/live", want: false},

		// prefix
		{name: "prefix covers subpath", pattern: "/api/**", path: "/api/plugins/x", want: true},
		{name: "prefix covers itself", pattern: "/api/**", path: "/api", want: true},
		{name: "prefix covers trailing slash", pattern: "/api/**", path: "/api/", want: true},
		{name: "prefix rejects sibling", pattern: "/api/**", path: "/apifoo", want: false},
		{name: "prefix rejects unrelated", pattern: "/api/**", path: "/static/app.js", want: false},

		// match-all
		{name: "doublestar matches anything", pattern: "**", path: "/anything/at/all", want: true},
		{name: "slash doublestar matches root", pattern: "/**", path: "/", want: true},

		// single-segment wildcard
		{name: "wildcard one segment", pattern: "/api/*/items", path: "/api/hello/items", want: true},
		{name: "wildcard does not span segments", pattern: "/api/*/items", path: "/api/a/b/items", want: false},
		{name: "wildcard needs a segment", pattern: "/api/*/items", path: "/api/items", want: false},
		{name: "wildcard rejects longer path", pattern: "/api/*/items", path: "/api/x/items/1", want: false},

		// wildcard plus trailing doublestar
		{name: "wildcard and subtree", pattern: "/api/*/items/**", path: "/api/x/items/1/detail", want: true},
		{name: "wildcard and subtree at root", pattern: "/api/*/items/**", path: "/api/x/items", want: true},
		{name: "wildcard and subtree wrong middle", pattern: "/api/*/items/**", path: "/api/x/orders/1", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tc.pattern, err)
			}
			if got := p.Match(tc.path); got != tc.want {
				t.Errorf("Compile(%q).Match(%q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestSetMatchesAnyMember(t *testing.T) {
	s, err := CompileSet([]string{"/api/a/**", "/api/b/**"})
	if err != nil {
		t.Fatalf("CompileSet: %v", err)
	}

	for _, path := range []string{"/api/a/x", "/api/b/y"} {
		if !s.Match(path) {
			t.Errorf("Match(%q) = false, want true", path)
		}
	}
	if s.Match("/api/c/z") {
		t.Error(`Match("/api/c/z") = true, want false`)
	}
}

// A filter that declares no paths must be inert. Treating an empty list as
// match-everything would turn a manifest omission into a gateway-wide filter.
func TestEmptySetMatchesNothing(t *testing.T) {
	s, err := CompileSet(nil)
	if err != nil {
		t.Fatalf("CompileSet: %v", err)
	}
	if s.Match("/anything") {
		t.Error("an empty pattern set matched a path")
	}
}

// The gateway consults these matchers on every request, so any allocation here
// becomes garbage-collector pressure proportional to traffic. This test is the
// guard that keeps the "unmatched paths cost nothing" claim honest.
func TestMatchDoesNotAllocate(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
	}{
		{name: "exact", pattern: "/api/health", path: "/api/health"},
		{name: "prefix hit", pattern: "/api/**", path: "/api/plugins/hello/items"},
		{name: "prefix miss", pattern: "/api/**", path: "/static/app.js"},
		{name: "segments hit", pattern: "/api/*/items/**", path: "/api/hello/items/42"},
		{name: "segments miss", pattern: "/api/*/items/**", path: "/api/hello/orders/42"},
		{name: "match all", pattern: "**", path: "/whatever"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := MustCompile(tc.pattern)
			got := testing.AllocsPerRun(200, func() {
				p.Match(tc.path)
			})
			if got != 0 {
				t.Errorf("Match allocated %.1f times per call, want 0", got)
			}
		})
	}
}

func TestSetMatchDoesNotAllocate(t *testing.T) {
	s, err := CompileSet([]string{"/api/a/**", "/api/*/items", "/exact"})
	if err != nil {
		t.Fatalf("CompileSet: %v", err)
	}
	got := testing.AllocsPerRun(200, func() {
		s.Match("/static/nothing/here.js")
	})
	if got != 0 {
		t.Errorf("Set.Match allocated %.1f times per call, want 0", got)
	}
}

func BenchmarkMatch(b *testing.B) {
	benches := []struct {
		name    string
		pattern string
		path    string
	}{
		{name: "prefix_miss", pattern: "/api/**", path: "/static/app.js"},
		{name: "prefix_hit", pattern: "/api/**", path: "/api/plugins/hello/items/42"},
		{name: "segments_hit", pattern: "/api/*/items/**", path: "/api/hello/items/42"},
	}
	for _, bc := range benches {
		p := MustCompile(bc.pattern)
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p.Match(bc.path)
			}
		})
	}
}
