package core

import "context"

// RequestMeta describes the inbound HTTP request. The router stores it in the request
// context before any handler runs; audit and auth read it from there.
type RequestMeta struct {
	ID        string // request id (also sent as X-Request-Id)
	IP        string // client IP (X-Forwarded-For honoured only from trusted proxies)
	UserAgent string // truncated to 512 bytes
	// Secure is true when the client connection used TLS, either directly or as reported by a
	// trusted proxy via X-Forwarded-Proto. Use it for the cookie Secure flag.
	Secure bool
	// Origin is the server's own origin as seen by this request ("https://host:port").
	Origin string
}

type metaKey struct{}

// WithRequestMeta returns ctx carrying m.
func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

// RequestMetaFrom returns the request meta stored in ctx (zero value and false if none, e.g.
// in background jobs).
func RequestMetaFrom(ctx context.Context) (RequestMeta, bool) {
	m, ok := ctx.Value(metaKey{}).(RequestMeta)
	return m, ok
}

// ClientIP returns the client IP from ctx, or "".
func ClientIP(ctx context.Context) string { m, _ := RequestMetaFrom(ctx); return m.IP }

// UserAgent returns the user agent from ctx, or "".
func UserAgent(ctx context.Context) string { m, _ := RequestMetaFrom(ctx); return m.UserAgent }

// RequestID returns the request id from ctx, or "".
func RequestID(ctx context.Context) string { m, _ := RequestMetaFrom(ctx); return m.ID }
