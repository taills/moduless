package pipeline

import (
	"fmt"
	"sort"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// phaseCount is one past the highest Phase enum value, so a phase can index
// directly into an array instead of hashing a map on every request.
const phaseCount = int(pb.Phase_PHASE_LOG) + 1

// Defaults fill in what a filter declaration leaves unset.
type Defaults struct {
	// Timeout bounds a single filter call.
	Timeout time.Duration

	// MaxBodyBytes caps a body handed to a filter that asked for one.
	MaxBodyBytes int
}

// DefaultDefaults are calibrated against the measured cross-process round trip
// (~37 microseconds empty, ~157 with a 64KB body). 50 ms leaves a filter three
// orders of magnitude of headroom over the transport, so a timeout means the
// plugin is genuinely stuck rather than merely slow.
func DefaultDefaults() Defaults {
	return Defaults{
		Timeout:      50 * time.Millisecond,
		MaxBodyBytes: 1 << 20,
	}
}

// Filter is one plugin's subscription to one phase, compiled and ready to run.
//
// It stores the plugin key rather than an instance pointer, and resolves the
// instance per request. That is what lets a hot upgrade take effect without
// rebuilding the chain: the next request simply resolves to the new process.
type Filter struct {
	PluginKey string
	Name      string
	Order     int
	Decl      manifest.FilterDecl

	match   manifest.CompiledFilter
	Timeout time.Duration
	MaxBody int

	// AllowIdentityMutation mirrors the plugin's filter:authenticate
	// permission. Without it, a set_identity mutation is discarded.
	AllowIdentityMutation bool
}

// Matches reports whether this filter applies to a request.
func (f *Filter) Matches(method, path string) bool { return f.match.Matches(method, path) }

// Label identifies the filter in logs and errors.
func (f *Filter) Label() string {
	if f.Name != "" {
		return f.PluginKey + "/" + f.Name
	}
	return f.PluginKey + "/" + f.Decl.Phase
}

// PluginFilters is one plugin's contribution to the chain.
type PluginFilters struct {
	Key     string
	Filters []manifest.CompiledFilter

	// AllowIdentityMutation is set when the plugin holds filter:authenticate.
	AllowIdentityMutation bool
}

// Chain is the immutable, per-phase filter table.
//
// It lives inside the registry snapshot, so swapping it is one atomic pointer
// store, and a request that already loaded a snapshot keeps seeing a
// consistent chain even if a plugin is enabled or disabled mid-flight.
type Chain struct {
	phases [phaseCount][]*Filter

	// anyNeedsRequestBody lets the gateway skip buffering a request body
	// entirely when no filter anywhere asked for one.
	anyNeedsRequestBody bool
	maxRequestBody      int
}

// EmptyChain is the chain used when no plugin declares a filter.
func EmptyChain() *Chain { return &Chain{} }

// BuildChain compiles every plugin's declarations into a per-phase table.
//
// Ordering is by the declared Order, then by plugin key, then by filter name.
// The tie-breakers matter: without them the chain order would depend on map
// iteration and two identical deployments could behave differently.
func BuildChain(plugins []PluginFilters, defaults Defaults) (*Chain, error) {
	if defaults.Timeout <= 0 {
		defaults.Timeout = DefaultDefaults().Timeout
	}
	if defaults.MaxBodyBytes <= 0 {
		defaults.MaxBodyBytes = DefaultDefaults().MaxBodyBytes
	}

	c := &Chain{}
	for _, p := range plugins {
		for _, cf := range p.Filters {
			phase, err := phaseFromName(cf.Decl.Phase)
			if err != nil {
				return nil, fmt.Errorf("plugin %s: %w", p.Key, err)
			}

			f := &Filter{
				PluginKey:             p.Key,
				Name:                  cf.Decl.Name,
				Order:                 cf.Decl.Order,
				Decl:                  cf.Decl,
				match:                 cf,
				Timeout:               defaults.Timeout,
				MaxBody:               defaults.MaxBodyBytes,
				AllowIdentityMutation: p.AllowIdentityMutation,
			}
			if cf.Decl.TimeoutMS > 0 {
				f.Timeout = time.Duration(cf.Decl.TimeoutMS) * time.Millisecond
			}
			if cf.Decl.MaxBodyBytes > 0 {
				f.MaxBody = cf.Decl.MaxBodyBytes
			}

			if cf.Decl.NeedsRequestBody {
				c.anyNeedsRequestBody = true
				if f.MaxBody > c.maxRequestBody {
					c.maxRequestBody = f.MaxBody
				}
			}
			c.phases[phase] = append(c.phases[phase], f)
		}
	}

	for i := range c.phases {
		fs := c.phases[i]
		sort.SliceStable(fs, func(a, b int) bool {
			if fs[a].Order != fs[b].Order {
				return fs[a].Order < fs[b].Order
			}
			if fs[a].PluginKey != fs[b].PluginKey {
				return fs[a].PluginKey < fs[b].PluginKey
			}
			return fs[a].Name < fs[b].Name
		})
	}
	return c, nil
}

// HasPhase reports whether any filter subscribed to a phase. This is the
// cheapest possible check — an array index and a length — and it is what makes
// an unfiltered deployment cost nothing.
func (c *Chain) HasPhase(phase pb.Phase) bool {
	i := int(phase)
	return i >= 0 && i < phaseCount && len(c.phases[i]) > 0
}

// Filters returns the ordered filters for a phase.
func (c *Chain) Filters(phase pb.Phase) []*Filter {
	i := int(phase)
	if i < 0 || i >= phaseCount {
		return nil
	}
	return c.phases[i]
}

// NeedsRequestBody reports whether any filter asked to see request bodies. The
// gateway uses it to decide whether to buffer at all.
func (c *Chain) NeedsRequestBody() bool { return c.anyNeedsRequestBody }

// MaxRequestBodyBytes is the largest body any filter is willing to receive.
func (c *Chain) MaxRequestBodyBytes() int { return c.maxRequestBody }

// Len reports the total number of filters across all phases.
func (c *Chain) Len() int {
	n := 0
	for i := range c.phases {
		n += len(c.phases[i])
	}
	return n
}

func phaseFromName(name string) (pb.Phase, error) {
	switch name {
	case manifest.PhasePreRoute:
		return pb.Phase_PHASE_PRE_ROUTE, nil
	case manifest.PhaseAuthenticate:
		return pb.Phase_PHASE_AUTHENTICATE, nil
	case manifest.PhaseAuthorize:
		return pb.Phase_PHASE_AUTHORIZE, nil
	case manifest.PhasePreHandler:
		return pb.Phase_PHASE_PRE_HANDLER, nil
	case manifest.PhasePostHandler:
		return pb.Phase_PHASE_POST_HANDLER, nil
	case manifest.PhaseOnError:
		return pb.Phase_PHASE_ON_ERROR, nil
	case manifest.PhaseLog:
		return pb.Phase_PHASE_LOG, nil
	default:
		return pb.Phase_PHASE_UNSPECIFIED, fmt.Errorf("unknown filter phase %q", name)
	}
}
