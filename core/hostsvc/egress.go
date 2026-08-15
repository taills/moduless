package hostsvc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPEgress proxies outbound requests on a plugin's behalf.
//
// Plugins have no direct network access, so this is the only way out, and it
// is where the allow-list is enforced. Two things matter here beyond matching
// hostnames:
//
//   - The address actually dialled is checked, not just the hostname. A
//     hostname on the allow-list can resolve to 127.0.0.1 or to the cloud
//     metadata endpoint, either by misconfiguration or deliberately (DNS
//     rebinding). Checking at dial time is the only point where the real
//     destination is known.
//   - Redirects are not followed. A permitted host answering with a redirect
//     to an internal address would otherwise walk straight past the
//     allow-list.
type HTTPEgress struct {
	// AllowFor returns the hostname patterns a plugin may reach. Patterns are
	// exact hostnames or a leading "*." wildcard.
	AllowFor func(pluginKey string) []string

	// MaxResponseBytes caps what is read back, so a plugin cannot be handed an
	// unbounded stream that Core has to buffer.
	MaxResponseBytes int64

	// DefaultTimeout applies when a request does not set one.
	DefaultTimeout time.Duration

	// RatePerMinute bounds how many outbound requests one plugin may make.
	RatePerMinute int

	// OnRequest, when set, receives every attempt for audit purposes,
	// including the ones that were refused.
	OnRequest func(pluginKey, method, url string, status int, err error)

	mu     sync.Mutex
	counts map[string]*rateWindow
	client *http.Client
	once   sync.Once
}

type rateWindow struct {
	windowStart time.Time
	count       int
}

const (
	defaultEgressTimeout  = 15 * time.Second
	defaultMaxResponse    = 8 << 20
	defaultRatePerMinute  = 120
	egressDialTimeout     = 5 * time.Second
	egressHandshakeTimout = 5 * time.Second
)

func NewHTTPEgress(allowFor func(string) []string) *HTTPEgress {
	return &HTTPEgress{
		AllowFor:         allowFor,
		MaxResponseBytes: defaultMaxResponse,
		DefaultTimeout:   defaultEgressTimeout,
		RatePerMinute:    defaultRatePerMinute,
		counts:           map[string]*rateWindow{},
	}
}

func (e *HTTPEgress) init() {
	e.once.Do(func() {
		if e.counts == nil {
			e.counts = map[string]*rateWindow{}
		}
		transport := &http.Transport{
			DialContext:         guardedDial,
			TLSHandshakeTimeout: egressHandshakeTimout,
			DisableKeepAlives:   false,
			MaxIdleConnsPerHost: 4,
		}
		e.client = &http.Client{
			Transport: transport,
			// Redirects are refused rather than followed: a permitted host
			// could otherwise redirect to an internal address and step around
			// the allow-list entirely.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
}

// Fetch performs an allow-listed outbound request.
func (e *HTTPEgress) Fetch(ctx context.Context, pluginKey string, req EgressRequest) (EgressResponse, error) {
	e.init()

	target, err := parseTarget(req.URL)
	if err != nil {
		e.audit(pluginKey, req.Method, req.URL, 0, err)
		return EgressResponse{}, err
	}

	allowed := e.AllowFor(pluginKey)
	if !hostAllowed(target.Hostname(), allowed) {
		err := fmt.Errorf("host %q is not in this plugin's egress_allow list", target.Hostname())
		e.audit(pluginKey, req.Method, req.URL, 0, err)
		return EgressResponse{}, err
	}

	if !e.allowRate(pluginKey) {
		err := fmt.Errorf("outbound request rate limit exceeded (%d/minute)", e.rateLimit())
		e.audit(pluginKey, req.Method, req.URL, 0, err)
		return EgressResponse{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.timeout()
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = strings.NewReader(string(req.Body))
	}

	httpReq, err := http.NewRequestWithContext(callCtx, method, req.URL, body)
	if err != nil {
		e.audit(pluginKey, method, req.URL, 0, err)
		return EgressResponse{}, err
	}
	for k, vs := range req.Headers {
		// Hop-by-hop headers are the caller's business only within its own
		// connection; forwarding them corrupts ours.
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		e.audit(pluginKey, method, req.URL, 0, err)
		return EgressResponse{}, err
	}
	defer resp.Body.Close()

	limit := e.MaxResponseBytes
	if limit <= 0 {
		limit = defaultMaxResponse
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		e.audit(pluginKey, method, req.URL, resp.StatusCode, err)
		return EgressResponse{}, err
	}

	e.audit(pluginKey, method, req.URL, resp.StatusCode, nil)
	return EgressResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       payload,
	}, nil
}

func (e *HTTPEgress) timeout() time.Duration {
	if e.DefaultTimeout > 0 {
		return e.DefaultTimeout
	}
	return defaultEgressTimeout
}

func (e *HTTPEgress) rateLimit() int {
	if e.RatePerMinute > 0 {
		return e.RatePerMinute
	}
	return defaultRatePerMinute
}

// allowRate is a fixed-window counter. A sliding window would be smoother, but
// this is a guardrail against a runaway plugin rather than a billing meter.
func (e *HTTPEgress) allowRate(pluginKey string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	w, ok := e.counts[pluginKey]
	if !ok || now.Sub(w.windowStart) >= time.Minute {
		e.counts[pluginKey] = &rateWindow{windowStart: now, count: 1}
		return true
	}
	if w.count >= e.rateLimit() {
		return false
	}
	w.count++
	return true
}

func (e *HTTPEgress) audit(pluginKey, method, url string, status int, err error) {
	if e.OnRequest != nil {
		e.OnRequest(pluginKey, method, url, status, err)
	}
}

// parseTarget validates the URL shape before anything is dialled.
func parseTarget(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		// file://, gopher:// and friends are how an SSRF turns into local file
		// disclosure, so only the two schemes this is for are accepted.
		return nil, fmt.Errorf("scheme %q is not allowed; use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	return u, nil
}

// hostAllowed matches a hostname against the plugin's allow-list. A leading
// "*." matches one or more leading labels.
func hostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// guardedDial refuses connections to addresses that are not publicly routable.
//
// This runs after DNS resolution, which is the only place the real destination
// is known: an allow-listed hostname can resolve to a loopback, private or
// link-local address either by accident or by design, and the cloud metadata
// endpoint at 169.254.169.254 is the classic target.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: egressDialTimeout}
	var lastErr error
	for _, ip := range ips {
		if blocked, why := blockedIP(ip.IP); blocked {
			lastErr = fmt.Errorf("refusing to connect to %s: %s", ip.IP, why)
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable address for %s", host)
	}
	return nil, lastErr
}

// blockedIP reports whether an address is off-limits for plugin egress.
func blockedIP(ip net.IP) (bool, string) {
	switch {
	case ip.IsLoopback():
		return true, "loopback address"
	case ip.IsPrivate():
		return true, "private address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// Covers 169.254.169.254, the cloud instance metadata endpoint.
		return true, "link-local address"
	case ip.IsUnspecified():
		return true, "unspecified address"
	case ip.IsMulticast():
		return true, "multicast address"
	case ip.IsInterfaceLocalMulticast():
		return true, "interface-local address"
	}
	return false, ""
}

func isHopByHop(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "host":
		return true
	}
	return false
}
