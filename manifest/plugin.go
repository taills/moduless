package manifest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/taills/moduless/pathmatch"
)

// Runtime describes how Core launches the plugin process.
type Runtime struct {
	// Entrypoint is the executable's path inside the plugin package. It must
	// be a statically linked binary (CGO_ENABLED=0): a dynamically linked one
	// fails to exec in the musl-based runtime image.
	Entrypoint string `yaml:"entrypoint"`

	Args []string `yaml:"args"`

	// Replicas is how many processes to run. Traffic is spread across them by
	// smooth weighted round-robin. Defaults to 1.
	Replicas int `yaml:"replicas"`
}

// Permission names. Core rejects any permission it does not recognise at
// install time rather than ignoring it, so a typo fails loudly instead of
// silently leaving a plugin without the access it expects.
const (
	PermDB                 = "db"
	PermDBTx               = "db:tx"
	PermQueue              = "queue"
	PermCache              = "cache"
	PermLock               = "lock"
	PermCron               = "cron"
	PermEvents             = "events"
	PermFilesRead          = "files:read"
	PermFilesWrite         = "files:write"
	PermHTTPEgress         = "http:egress"
	PermFilterAuthenticate = "filter:authenticate"
)

var knownPermissions = map[string]struct{}{
	PermDB: {}, PermDBTx: {}, PermQueue: {}, PermCache: {}, PermLock: {},
	PermCron: {}, PermEvents: {}, PermFilesRead: {}, PermFilesWrite: {},
	PermHTTPEgress: {}, PermFilterAuthenticate: {},
}

// Filter lifecycle phase names, matching the Phase enum on the wire.
const (
	PhasePreRoute     = "pre_route"
	PhaseAuthenticate = "authenticate"
	PhaseAuthorize    = "authorize"
	PhasePreHandler   = "pre_handler"
	PhasePostHandler  = "post_handler"
	PhaseOnError      = "on_error"
	PhaseLog          = "log"
)

var knownPhases = map[string]struct{}{
	PhasePreRoute: {}, PhaseAuthenticate: {}, PhaseAuthorize: {},
	PhasePreHandler: {}, PhasePostHandler: {}, PhaseOnError: {}, PhaseLog: {},
}

// FilterMatch narrows which requests a filter sees. A filter with no paths
// matches nothing: an omission should make a filter inert, not global.
type FilterMatch struct {
	Paths []string `yaml:"paths"`
	// Methods is a list of HTTP methods, or ["*"] / empty for all.
	Methods []string `yaml:"methods"`
}

// FilterDecl is one subscription to a request lifecycle phase.
type FilterDecl struct {
	Name  string      `yaml:"name"`
	Phase string      `yaml:"phase"`
	Match FilterMatch `yaml:"match"`

	// Order sequences filters from different plugins within one phase.
	// Lower runs first; ties break on plugin key for determinism.
	Order int `yaml:"order"`

	// FailClosed makes a filter failure reject the request. The default is
	// fail-open, because most filters observe rather than guard; anything that
	// enforces a security decision must opt in to fail-closed, or an outage in
	// the plugin silently becomes an authorisation bypass.
	FailClosed bool `yaml:"fail_closed"`

	// TimeoutMS bounds one filter call. Zero uses Core's default.
	TimeoutMS int `yaml:"timeout_ms"`

	// NeedsRequestBody and NeedsResponseBody opt in to receiving bodies.
	// Bodies are withheld by default: they dominate the cost of a filter call
	// (a 64KB body is roughly four times the round-trip cost of an empty one)
	// and most filters only inspect method, path, headers and identity.
	NeedsRequestBody  bool `yaml:"needs_request_body"`
	NeedsResponseBody bool `yaml:"needs_response_body"`

	// MaxBodyBytes caps the body this filter accepts. Zero uses Core's default.
	MaxBodyBytes int `yaml:"max_body_bytes"`
}

// JobDecl is a scheduled task. Core owns the schedule so that jobs stop when a
// plugin is disabled, and so replicas do not each run the same occurrence.
type JobDecl struct {
	Name string `yaml:"name"`
	Cron string `yaml:"cron"`
}

// CompiledFilter is a FilterDecl with its patterns compiled, ready for the
// request path.
type CompiledFilter struct {
	Decl    FilterDecl
	Paths   pathmatch.Set
	methods map[string]struct{} // nil means all methods
}

// MatchesMethod reports whether this filter applies to an HTTP method.
func (c *CompiledFilter) MatchesMethod(method string) bool {
	if c.methods == nil {
		return true
	}
	_, ok := c.methods[method]
	return ok
}

// Matches reports whether the filter applies to a request.
func (c *CompiledFilter) Matches(method, path string) bool {
	return c.MatchesMethod(method) && c.Paths.Match(path)
}

// CompileFilters compiles every filter declaration, returning the first error.
func (m *Manifest) CompileFilters() ([]CompiledFilter, error) {
	out := make([]CompiledFilter, 0, len(m.Filters))
	for i, f := range m.Filters {
		set, err := pathmatch.CompileSet(f.Match.Paths)
		if err != nil {
			return nil, fmt.Errorf("filter %s: %w", filterLabel(f, i), err)
		}
		cf := CompiledFilter{Decl: f, Paths: set}
		if ms := normalizeMethods(f.Match.Methods); ms != nil {
			cf.methods = ms
		}
		out = append(out, cf)
	}
	return out, nil
}

// normalizeMethods returns nil for "all methods", otherwise an uppercase set.
func normalizeMethods(methods []string) map[string]struct{} {
	if len(methods) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "*" || m == "" {
			return nil
		}
		set[m] = struct{}{}
	}
	return set
}

// HasPermission reports whether the manifest requests a permission.
func (m *Manifest) HasPermission(perm string) bool {
	return slices.Contains(m.Permissions, perm)
}

// Validate checks everything Core must be sure of before it will run a plugin.
// It is deliberately strict: an unrecognised permission or phase is a hard
// error, because the alternative is a plugin that quietly does not do what its
// author intended.
func (m *Manifest) Validate() error {
	if m.Key == "" {
		return fmt.Errorf("manifest: key is required")
	}
	if strings.ContainsAny(m.Key, "/\\. ") {
		return fmt.Errorf("manifest: key %q must not contain path separators, dots or spaces", m.Key)
	}
	if m.Version == "" {
		return fmt.Errorf("manifest: version is required")
	}

	for _, p := range m.Permissions {
		if _, ok := knownPermissions[p]; !ok {
			return fmt.Errorf("manifest: unknown permission %q", p)
		}
	}

	seenFilter := map[string]struct{}{}
	for i, f := range m.Filters {
		label := filterLabel(f, i)
		if _, ok := knownPhases[f.Phase]; !ok {
			return fmt.Errorf("manifest: filter %s has unknown phase %q", label, f.Phase)
		}
		if f.Name != "" {
			if _, dup := seenFilter[f.Name]; dup {
				return fmt.Errorf("manifest: duplicate filter name %q", f.Name)
			}
			seenFilter[f.Name] = struct{}{}
		}
		if len(f.Match.Paths) == 0 {
			return fmt.Errorf("manifest: filter %s declares no paths, so it would never run", label)
		}
		if _, err := pathmatch.CompileSet(f.Match.Paths); err != nil {
			return fmt.Errorf("manifest: filter %s: %w", label, err)
		}
		// A filter that alters identity is effectively part of authentication,
		// so it must both hold the permission and run in a phase where Core
		// still acts on the result.
		if f.Phase == PhaseAuthenticate && !m.HasPermission(PermFilterAuthenticate) {
			return fmt.Errorf(
				"manifest: filter %s subscribes to the authenticate phase but does not request the %q permission",
				label, PermFilterAuthenticate)
		}
		if f.TimeoutMS < 0 {
			return fmt.Errorf("manifest: filter %s has a negative timeout", label)
		}
		if f.MaxBodyBytes < 0 {
			return fmt.Errorf("manifest: filter %s has a negative max_body_bytes", label)
		}
	}

	seenJob := map[string]struct{}{}
	for _, j := range m.Jobs {
		if j.Name == "" {
			return fmt.Errorf("manifest: job is missing a name")
		}
		if _, dup := seenJob[j.Name]; dup {
			return fmt.Errorf("manifest: duplicate job name %q", j.Name)
		}
		seenJob[j.Name] = struct{}{}
		if j.Cron == "" {
			return fmt.Errorf("manifest: job %q is missing a cron expression", j.Name)
		}
		// Parsed at install time so a bad expression is a rejected package
		// rather than a job that silently never runs.
		if _, err := ParseSchedule(j.Cron); err != nil {
			return fmt.Errorf("manifest: job %q: %w", j.Name, err)
		}
	}
	if len(m.Jobs) > 0 && !m.HasPermission(PermCron) {
		return fmt.Errorf("manifest: jobs are declared but the %q permission is not requested", PermCron)
	}
	if len(m.EgressAllow) > 0 && !m.HasPermission(PermHTTPEgress) {
		return fmt.Errorf("manifest: egress_allow is set but the %q permission is not requested", PermHTTPEgress)
	}

	if m.Runtime.Replicas < 0 {
		return fmt.Errorf("manifest: runtime.replicas must not be negative")
	}
	return nil
}

// ReplicaCount returns how many processes to run, defaulting to one.
func (m *Manifest) ReplicaCount() int {
	if m.Runtime.Replicas <= 0 {
		return 1
	}
	return m.Runtime.Replicas
}

func filterLabel(f FilterDecl, idx int) string {
	if f.Name != "" {
		return fmt.Sprintf("%q", f.Name)
	}
	return fmt.Sprintf("#%d (%s)", idx, f.Phase)
}
