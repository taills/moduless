package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func baseManifest() *Manifest {
	return &Manifest{Key: "hello", Version: "1.0.0"}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr bool
	}{
		{
			name:   "minimal manifest is valid",
			mutate: func(*Manifest) {},
		},
		{
			name:    "key is required",
			mutate:  func(m *Manifest) { m.Key = "" },
			wantErr: true,
		},
		{
			name:    "version is required",
			mutate:  func(m *Manifest) { m.Version = "" },
			wantErr: true,
		},
		{
			// The key becomes a URL prefix and a directory name, so a
			// separator in it would let a package escape its own directory.
			name:    "key rejects path separators",
			mutate:  func(m *Manifest) { m.Key = "../etc" },
			wantErr: true,
		},
		{
			// max_body_bytes is read only when the request body is attached, so
			// declaring it alongside needs_response_body alone caps nothing.
			// Measured: a filter declaring 1 KiB was handed the whole 64 KiB
			// response. Enforcing it there instead would be worse — a large
			// response would skip a fail-open redaction filter and go out
			// unredacted — so the declaration is the thing that has to go.
			name: "max_body_bytes without needs_request_body is rejected",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{
					Name: "scan", Phase: PhasePostHandler,
					Match:             FilterMatch{Paths: []string{"/**"}},
					NeedsResponseBody: true,
					MaxBodyBytes:      262144,
				}}
			},
			wantErr: true,
		},
		{
			name: "max_body_bytes with needs_request_body is accepted",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{
					Name: "scan", Phase: PhasePreRoute,
					Match:            FilterMatch{Paths: []string{"/**"}},
					NeedsRequestBody: true,
					MaxBodyBytes:     262144,
				}}
			},
		},
		{
			name:    "unknown permission is rejected",
			mutate:  func(m *Manifest) { m.Permissions = []string{"db", "root:everything"} },
			wantErr: true,
		},
		{
			name:   "known permissions pass",
			mutate: func(m *Manifest) { m.Permissions = []string{PermDB, PermQueue, PermCache} },
		},
		{
			name: "unknown filter phase is rejected",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{Phase: "whenever", Match: FilterMatch{Paths: []string{"/**"}}}}
			},
			wantErr: true,
		},
		{
			// A filter with no paths would never fire; that is almost always a
			// manifest mistake rather than an intent.
			name: "filter without paths is rejected",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{Phase: PhasePreRoute}}
			},
			wantErr: true,
		},
		{
			name: "filter with a bad path pattern is rejected",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{Phase: PhasePreRoute, Match: FilterMatch{Paths: []string{"/a/**/b"}}}}
			},
			wantErr: true,
		},
		{
			name: "duplicate filter names are rejected",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{
					{Name: "f", Phase: PhasePreRoute, Match: FilterMatch{Paths: []string{"/**"}}},
					{Name: "f", Phase: PhaseLog, Match: FilterMatch{Paths: []string{"/**"}}},
				}
			},
			wantErr: true,
		},
		{
			// Setting identity is an authentication decision. Without the
			// permission gate any plugin could escalate its own privileges.
			name: "authenticate phase requires the permission",
			mutate: func(m *Manifest) {
				m.Filters = []FilterDecl{{Phase: PhaseAuthenticate, Match: FilterMatch{Paths: []string{"/**"}}}}
			},
			wantErr: true,
		},
		{
			name: "authenticate phase passes with the permission",
			mutate: func(m *Manifest) {
				m.Permissions = []string{PermFilterAuthenticate}
				m.Filters = []FilterDecl{{Phase: PhaseAuthenticate, Match: FilterMatch{Paths: []string{"/**"}}}}
			},
		},
		{
			name: "jobs require the cron permission",
			mutate: func(m *Manifest) {
				m.Jobs = []JobDecl{{Name: "nightly", Cron: "0 3 * * *"}}
			},
			wantErr: true,
		},
		{
			name: "jobs pass with the cron permission",
			mutate: func(m *Manifest) {
				m.Permissions = []string{PermCron}
				m.Jobs = []JobDecl{{Name: "nightly", Cron: "0 3 * * *"}}
			},
		},
		{
			name: "job without a cron expression is rejected",
			mutate: func(m *Manifest) {
				m.Permissions = []string{PermCron}
				m.Jobs = []JobDecl{{Name: "nightly"}}
			},
			wantErr: true,
		},
		{
			name: "duplicate job names are rejected",
			mutate: func(m *Manifest) {
				m.Permissions = []string{PermCron}
				m.Jobs = []JobDecl{{Name: "j", Cron: "* * * * *"}, {Name: "j", Cron: "* * * * *"}}
			},
			wantErr: true,
		},
		{
			name: "egress allow-list requires the permission",
			mutate: func(m *Manifest) {
				m.EgressAllow = []string{"api.example.com"}
			},
			wantErr: true,
		},
		{
			name:    "negative replicas rejected",
			mutate:  func(m *Manifest) { m.Runtime.Replicas = -1 },
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := baseManifest()
			tc.mutate(m)

			err := m.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate succeeded, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestCompileFiltersMatching(t *testing.T) {
	m := baseManifest()
	m.Filters = []FilterDecl{
		{
			Name:  "writes-only",
			Phase: PhasePreRoute,
			Match: FilterMatch{Paths: []string{"/api/**"}, Methods: []string{"post", "put"}},
		},
		{
			Name:  "everything",
			Phase: PhaseLog,
			Match: FilterMatch{Paths: []string{"**"}},
		},
	}

	compiled, err := m.CompileFilters()
	if err != nil {
		t.Fatalf("CompileFilters: %v", err)
	}
	if len(compiled) != 2 {
		t.Fatalf("compiled %d filters, want 2", len(compiled))
	}

	writes := &compiled[0]
	tests := []struct {
		method, path string
		want         bool
	}{
		{method: "POST", path: "/api/plugins/x", want: true},
		{method: "PUT", path: "/api/x", want: true},
		{method: "GET", path: "/api/x", want: false},     // method not subscribed
		{method: "POST", path: "/static/x", want: false}, // path not subscribed
	}
	for _, tc := range tests {
		if got := writes.Matches(tc.method, tc.path); got != tc.want {
			t.Errorf("Matches(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}

	// An empty method list means every method.
	all := &compiled[1]
	for _, method := range []string{"GET", "POST", "DELETE", "PATCH"} {
		if !all.Matches(method, "/anything") {
			t.Errorf("catch-all filter did not match %s", method)
		}
	}
}

// "*" in the method list means all methods, the same as omitting it.
func TestCompileFiltersStarMethodMeansAll(t *testing.T) {
	m := baseManifest()
	m.Filters = []FilterDecl{{
		Phase: PhasePreRoute,
		Match: FilterMatch{Paths: []string{"/**"}, Methods: []string{"*"}},
	}}

	compiled, err := m.CompileFilters()
	if err != nil {
		t.Fatalf("CompileFilters: %v", err)
	}
	if !compiled[0].Matches("DELETE", "/x") {
		t.Error(`Methods: ["*"] did not match DELETE`)
	}
}

func TestReplicaCountDefaultsToOne(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		want     int
	}{
		{name: "unset", replicas: 0, want: 1},
		{name: "explicit one", replicas: 1, want: 1},
		{name: "three", replicas: 3, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := baseManifest()
			m.Runtime.Replicas = tc.replicas
			if got := m.ReplicaCount(); got != tc.want {
				t.Errorf("ReplicaCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Parsing the full plugin-model manifest end to end guards the yaml tags,
// which are easy to get wrong and fail silently.
func TestLoadParsesPluginFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")

	content := `
key: hello
display_name: Hello Plugin
version: 1.2.0
runtime:
  entrypoint: bin/plugin-linux-amd64
  replicas: 2
permissions:
  - db
  - queue
  - http:egress
filters:
  - name: rate-limit
    phase: pre_route
    order: 10
    fail_closed: true
    timeout_ms: 50
    match:
      paths: ["/api/**"]
      methods: ["POST", "PUT"]
  - name: audit
    phase: log
    needs_request_body: true
    max_body_bytes: 65536
    match:
      paths: ["**"]
jobs:
  - name: nightly-rollup
    cron: "17 3 * * *"
egress_allow:
  - api.example.com
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if m.Runtime.Entrypoint != "bin/plugin-linux-amd64" {
		t.Errorf("entrypoint = %q", m.Runtime.Entrypoint)
	}
	if got := m.ReplicaCount(); got != 2 {
		t.Errorf("replicas = %d, want 2", got)
	}
	if !m.HasPermission(PermQueue) || !m.HasPermission(PermHTTPEgress) {
		t.Errorf("permissions = %v", m.Permissions)
	}
	if len(m.Filters) != 2 {
		t.Fatalf("parsed %d filters, want 2", len(m.Filters))
	}
	if !m.Filters[0].FailClosed || m.Filters[0].TimeoutMS != 50 {
		t.Errorf("first filter = %+v", m.Filters[0])
	}
	if !m.Filters[1].NeedsRequestBody || m.Filters[1].MaxBodyBytes != 65536 {
		t.Errorf("second filter = %+v", m.Filters[1])
	}
	if len(m.Jobs) != 1 || m.Jobs[0].Cron != "17 3 * * *" {
		t.Errorf("jobs = %+v", m.Jobs)
	}
	if len(m.EgressAllow) != 1 {
		t.Errorf("egress_allow = %v", m.EgressAllow)
	}

	// Jobs without the cron permission must fail validation even though the
	// YAML parsed cleanly.
	if err := m.Validate(); err == nil {
		t.Error("Validate accepted jobs without the cron permission")
	}
}

// Limits on what a manifest may declare.
//
// Plugins are trusted, so these are not about malice. They are about a mistake
// — a generated manifest, a loop that ran too long — producing an error at
// install time rather than a Core that is mysteriously slow, a database with a
// thousand unexpected tables, or a menu no browser can render.
func TestDeclarationLimits(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{
			Key:         "p",
			Version:     "1.0.0",
			Runtime:     Runtime{Entrypoint: "bin/p"},
			Permissions: []string{PermDB, PermCron},
		}
	}

	t.Run("filters", func(t *testing.T) {
		m := base()
		for i := range MaxFilters + 1 {
			m.Filters = append(m.Filters, FilterDecl{
				Name:  fmt.Sprintf("f%d", i),
				Phase: PhasePreRoute,
				Match: FilterMatch{Paths: []string{"/**"}},
			})
		}
		if err := m.Validate(); err == nil {
			t.Errorf("%d filters were accepted", len(m.Filters))
		}

		m.Filters = m.Filters[:MaxFilters]
		if err := m.Validate(); err != nil {
			t.Errorf("exactly the limit was rejected: %v", err)
		}
	})

	t.Run("collections", func(t *testing.T) {
		m := base()
		for i := range MaxCollections + 1 {
			m.Database.Collections = append(m.Database.Collections,
				Collection{Name: fmt.Sprintf("c%d", i)})
		}
		if err := m.Validate(); err == nil {
			t.Error("a plugin was allowed to ask Core for more tables than the limit")
		}
	})

	t.Run("jobs", func(t *testing.T) {
		m := base()
		for i := range MaxJobs + 1 {
			m.Jobs = append(m.Jobs, JobDecl{Name: fmt.Sprintf("j%d", i), Cron: "0 3 * * *"})
		}
		if err := m.Validate(); err == nil {
			t.Error("more jobs than the limit were accepted")
		}
	})

	t.Run("config keys", func(t *testing.T) {
		m := base()
		for i := range MaxConfigKeys + 1 {
			m.Config = append(m.Config, ConfigDecl{Key: fmt.Sprintf("k%d", i)})
		}
		if err := m.Validate(); err == nil {
			t.Error("more config keys than the limit were accepted")
		}
	})

	t.Run("menu depth", func(t *testing.T) {
		// Build a chain one level deeper than allowed.
		leaf := MenuItem{Path: "/deep", Title: "leaf"}
		node := leaf
		for i := range MaxMenuDepth {
			node = MenuItem{
				Path:     fmt.Sprintf("/level%d", i),
				Title:    "n",
				Children: []MenuItem{node},
			}
		}

		m := base()
		m.Menus = []MenuItem{node}
		if err := m.Validate(); err == nil {
			t.Errorf("a menu nested deeper than %d levels was accepted", MaxMenuDepth)
		}
	})

	t.Run("menu breadth", func(t *testing.T) {
		m := base()
		for i := range MaxMenuNodes + 1 {
			m.Menus = append(m.Menus, MenuItem{Path: fmt.Sprintf("/n%d", i), Title: "n"})
		}
		if err := m.Validate(); err == nil {
			t.Errorf("more than %d menu nodes were accepted; the whole tree is sent to every browser", MaxMenuNodes)
		}
	})

	t.Run("an ordinary manifest is unaffected", func(t *testing.T) {
		m := base()
		m.Filters = []FilterDecl{{Name: "f", Phase: PhaseLog, Match: FilterMatch{Paths: []string{"/**"}}}}
		m.Database.Collections = []Collection{{Name: "notes"}}
		m.Jobs = []JobDecl{{Name: "nightly", Cron: "17 3 * * *"}}
		m.Config = []ConfigDecl{{Key: "retention_days", Default: "30"}}
		m.Menus = []MenuItem{{
			Path: "/notes", Title: "Notes",
			Children: []MenuItem{{Path: "/notes/archive", Title: "Archive"}},
		}}
		if err := m.Validate(); err != nil {
			t.Errorf("a realistic manifest was rejected: %v", err)
		}
	})
}
