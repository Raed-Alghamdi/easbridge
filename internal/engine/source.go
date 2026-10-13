package engine

import (
	"context"
	"net"
	"sync"
	"time"
)

type sourceKey struct{}

// WithSource tells Login where a login comes from (the remote IP address of the client connection,
// without the port). The servers set it; a login without one is counted under "unknown".
func WithSource(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, sourceKey{}, ip)
}

func sourceOf(ctx context.Context) string {
	if s, ok := ctx.Value(sourceKey{}).(string); ok && s != "" {
		return s
	}
	return "unknown"
}

// Limits of the unverified names one source may try (DomainEntries only).
const (
	defaultSourceNames  = 10
	defaultSourceWindow = 10 * time.Minute
	maxSources          = 4096
)

// sourceLimiter limits how many distinct login names one source address may present before one of them is
// verified, so that a client that invents names (an allow-list with a domain entry accepts every name of
// the domain) cannot make the bridge ask Exchange for each. It is in memory, bounded, and entries leave
// when the window is over.
type sourceLimiter struct {
	mu      sync.Mutex
	names   int
	window  time.Duration
	sources map[string]map[string]time.Time // ip -> name -> first attempt
}

func newSourceLimiter(names int, window time.Duration) *sourceLimiter {
	if names <= 0 {
		names = defaultSourceNames
	}
	if window <= 0 {
		window = defaultSourceWindow
	}
	return &sourceLimiter{names: names, window: window, sources: map[string]map[string]time.Time{}}
}

// allow records that ip presents name and says whether it may: a name already in the list is allowed again
// (the per-user rules govern it); a new one is refused once the source has names unverified names in the window.
func (l *sourceLimiter) allow(ip, name string, now time.Time) (ok bool, retryAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)
	m := l.sources[ip]
	if m == nil {
		if len(l.sources) >= maxSources {
			l.evictOldest()
		}
		m = map[string]time.Time{}
		l.sources[ip] = m
	}
	if _, known := m[name]; known {
		return true, time.Time{}
	}
	if len(m) >= l.names {
		oldest := now
		for _, t := range m {
			if t.Before(oldest) {
				oldest = t
			}
		}
		return false, oldest.Add(l.window)
	}
	m[name] = now
	return true, time.Time{}
}

// verified forgets name for ip: it proved to be a real login, which does not count against the source.
func (l *sourceLimiter) verified(ip, name string) {
	l.mu.Lock()
	if m := l.sources[ip]; m != nil {
		delete(m, name)
		if len(m) == 0 {
			delete(l.sources, ip)
		}
	}
	l.mu.Unlock()
}

func (l *sourceLimiter) gc(now time.Time) {
	for ip, m := range l.sources {
		for n, t := range m {
			if now.Sub(t) >= l.window {
				delete(m, n)
			}
		}
		if len(m) == 0 {
			delete(l.sources, ip)
		}
	}
}

func (l *sourceLimiter) evictOldest() {
	var oldIP string
	var at time.Time
	first := true
	for ip, m := range l.sources {
		for _, t := range m {
			if first || t.Before(at) {
				oldIP, at, first = ip, t, false
			}
		}
	}
	delete(l.sources, oldIP)
}

func (l *sourceLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sources)
}

// SourceFromAddr is the IP address of a network address, for WithSource ("" when there is none).
func SourceFromAddr(a net.Addr) string {
	if a == nil {
		return ""
	}
	return SourceFromString(a.String())
}

// SourceFromString is the host part of "ip:port" (or the string itself when it has no port).
func SourceFromString(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}
