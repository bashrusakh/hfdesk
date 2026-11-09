// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyConfig holds proxy configuration settings.
// Supports HTTP, HTTPS, and SOCKS5 proxies with optional authentication.
type ProxyConfig struct {
	// URL is the proxy server URL.
	// Supported schemes:
	//   - http://host:port
	//   - https://host:port
	//   - socks5://host:port
	//   - socks5h://host:port (DNS resolution via proxy)
	//
	// Authentication can be embedded in the URL:
	//   - http://user:pass@host:port
	//   - socks5://user:pass@host:port
	//
	// If empty, falls back to environment variables (HTTP_PROXY, HTTPS_PROXY)
	// unless NoEnvProxy is set.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`

	// Username for proxy authentication (alternative to embedding in URL).
	Username string `json:"username,omitempty" yaml:"username,omitempty"`

	// Password for proxy authentication (alternative to embedding in URL).
	Password string `json:"password,omitempty" yaml:"password,omitempty"`

	// NoProxy is a comma-separated list of hosts that should bypass the proxy.
	// Supports:
	//   - Exact hostnames: "localhost"
	//   - Domain suffixes: ".internal.com"
	//   - CIDR ranges: "10.0.0.0/8"
	//   - Wildcard: "*" (bypass all)
	//
	// If empty, falls back to NO_PROXY environment variable.
	NoProxy string `json:"no_proxy,omitempty" yaml:"no_proxy,omitempty"`

	// NoEnvProxy disables reading proxy settings from environment variables.
	// When true, only explicit ProxyConfig settings are used.
	NoEnvProxy bool `json:"no_env_proxy,omitempty" yaml:"no_env_proxy,omitempty"`

	// InsecureSkipVerify disables TLS certificate verification for HTTPS proxies.
	// WARNING: Only use for testing or with trusted proxies.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
}

// IsConfigured returns true if explicit proxy settings are provided.
func (p *ProxyConfig) IsConfigured() bool {
	return p != nil && p.URL != ""
}

// GetProxyURL returns the parsed proxy URL with authentication applied.
// Returns nil if no proxy is configured.
//
// A parse failure is reported without the proxy URL: the URL can embed the
// proxy credentials, and this error travels to the API responses and to the
// log records of the callers.
func (p *ProxyConfig) GetProxyURL() (*url.URL, error) {
	if p == nil || p.URL == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %s", proxyParseFailureReason(p.URL))
	}

	// Apply username/password if not already in URL
	if proxyURL.User == nil && p.Username != "" {
		if p.Password != "" {
			proxyURL.User = url.UserPassword(p.Username, p.Password)
		} else {
			proxyURL.User = url.User(p.Username)
		}
	}

	return proxyURL, nil
}

// proxyURLShape classifies the authority of a raw proxy URL.
//
// The authority is the only place a proxy URL can carry a credential, so its
// shape decides what may be shown and what may not.
type proxyURLShape int

const (
	// proxyURLUnlocatable means that the authority of the URL cannot be
	// located: the URL has no scheme separator, or the part before it is
	// not a scheme. That part can hold anything, a credential included,
	// so no part of such a URL may be shown.
	proxyURLUnlocatable proxyURLShape = iota

	// proxyURLHost means that the authority is a host with an optional
	// port. It carries no credential.
	proxyURLHost

	// proxyURLUserinfo means that the authority carried a userinfo: the
	// part before the last "@" is the credential of the URL.
	proxyURLUserinfo

	// proxyURLAmbiguous means that the authority is neither a host nor a
	// userinfo that ends with an "@". It may be a credential that was not
	// terminated by an "@", so no part of it may be shown.
	proxyURLAmbiguous
)

// redactedProxyAuthority replaces the authority of a proxy URL that cannot
// be shown. It is a display marker, not a host.
const redactedProxyAuthority = "<redacted>"

// proxyParseFailureReason explains why a proxy URL failed to parse without
// quoting the URL or any part of it that could be a credential.
//
// url.Parse reports a failure as a *url.Error whose Error() embeds the whole
// URL — including the proxy credentials — and whose Err carries the reason.
// The reason is not safe to quote either: it can repeat a fragment of a
// malformed credential (an invalid percent escape inside the userinfo). The
// reason is therefore taken from a parse of the URL with its credentials
// removed. A URL whose authority cannot be located is reported without
// naming a part of it, and a URL whose credentials alone made it
// unparseable is reported with a reason that names no part of the URL
// either.
//
// A URL that parses has no failure to explain: the reason of such a URL is
// the empty string.
func proxyParseFailureReason(raw string) string {
	_, parseErr := url.Parse(raw)
	if parseErr == nil {
		// The URL parses, so there is no failure to explain.
		return ""
	}

	prefix, host, rest, shape := splitProxyURL(raw)

	switch shape {
	case proxyURLUnlocatable:
		// No part of the URL may be shown, so the reason names no part
		// of it either.
		return "the scheme of the proxy URL is missing or invalid"
	case proxyURLAmbiguous:
		// The authority may be a credential, so it is not part of the URL
		// that is parsed for the reason. The rest of the URL can still
		// be at fault.
		if _, err := url.Parse(prefix + rest); err != nil {
			return ProxyErrorReason(err)
		}
		return "the host or the port of the proxy URL is invalid"
	case proxyURLUserinfo:
		// The credential is dropped: what is left of the URL is a host
		// with an optional port, and it carries no credential.
		if _, err := url.Parse(prefix + host + rest); err != nil {
			return ProxyErrorReason(err)
		}
		// The URL parses without its credential, so the credential is
		// what made it fail.
		return "invalid proxy credentials or URL syntax"
	default:
		// The remaining shape is proxyURLHost: the authority is a host,
		// so the failure is in the rest of the URL and the reason of it
		// carries no credential.
		return ProxyErrorReason(parseErr)
	}
}

// splitProxyURL splits a raw proxy URL into the parts that may be shown and
// classifies its authority.
//
// The scheme has to be a real scheme: the part before the "://" of a URL
// whose scheme is not a scheme can hold anything, a credential included, so
// such a URL is not locatable and none of its parts may be shown.
func splitProxyURL(raw string) (prefix, host, rest string, shape proxyURLShape) {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 || !isURLScheme(raw[:schemeEnd]) {
		return "", "", "", proxyURLUnlocatable
	}
	prefix = raw[:schemeEnd+3]
	remainder := raw[schemeEnd+3:]

	authorityEnd := len(remainder)
	for i := 0; i < len(remainder); i++ {
		if c := remainder[i]; c == '/' || c == '?' || c == '#' {
			authorityEnd = i
			break
		}
	}
	authority := remainder[:authorityEnd]
	rest = remainder[authorityEnd:]

	// The userinfo of an authority ends at its last "@": everything before
	// it is the credential of the URL and is dropped. What follows is the
	// host with its optional port.
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
		shape = proxyURLUserinfo
	} else {
		shape = proxyURLHost
	}

	if !isHostWithPort(authority) {
		// The authority is not a host with a valid port. It is the shape
		// of a credential that was not terminated by an "@": a parser
		// reads the user of "user:password" as the host and the password
		// as the port, and that is indistinguishable from a host with a
		// port that is not a number.
		//
		// The whole authority is dropped, because the host of that shape
		// can be the user of the credential: never leaking is preferred
		// over keeping a host that may be a user.
		return prefix, "", rest, proxyURLAmbiguous
	}
	return prefix, authority, rest, shape
}

// isURLScheme reports whether the text is a URL scheme.
//
// A scheme starts with a letter and continues with letters, digits, "+", "-"
// and "." (RFC 3986).
func isURLScheme(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isHostWithPort reports whether the authority of a proxy URL is a host with
// an optional port.
//
// The standard library is the authority: a port that is not a number is not
// a port, and a host literal that does not close is not a host. The test
// uses a fixed scheme, because the grammar of a host does not depend on the
// scheme of the URL.
func isHostWithPort(authority string) bool {
	_, err := url.Parse("http://" + authority)
	return err == nil
}

// sanitizeProxyURL removes the credentials from a proxy URL so that it can be
// shown or reported.
//
// The userinfo of the URL is its credential and is dropped. An authority
// that is not a host is dropped as a whole, because it may be a credential
// that was not terminated by an "@": its host can be the user and its port
// can be the password of that credential.
//
// A URL whose authority cannot be located is replaced by a fixed
// description: no part of it may be shown.
func sanitizeProxyURL(raw string) string {
	prefix, host, rest, shape := splitProxyURL(raw)
	switch shape {
	case proxyURLUnlocatable:
		return "invalid proxy URL"
	case proxyURLAmbiguous:
		// The authority may be a credential, so it is replaced by a
		// marker instead of being shown.
		return prefix + redactedProxyAuthority + rest
	default:
		return prefix + host + rest
	}
}

// ProxyErrorReason renders the reason of a proxy configuration failure
// without the proxy URL.
//
// A proxy URL parse failure carries the whole proxy URL, and that URL can
// carry the proxy credentials, so only the reason of the failure may reach a
// log record or an API response.
//
// The reason of a *url.Error is the cause of the failure, which never quotes
// the URL itself. A failure that is not a URL parse failure is reported with
// its own message.
//
// A nil error has no reason: the empty string is returned for it.
//
// This is the single implementation of the rule: the callers in this package
// and the server package use it, so the reason can never grow a second
// spelling that quotes the URL.
func ProxyErrorReason(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// IsSocks returns true if the proxy uses SOCKS5 protocol.
func (p *ProxyConfig) IsSocks() bool {
	if p == nil || p.URL == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(p.URL), "socks5://") ||
		strings.HasPrefix(strings.ToLower(p.URL), "socks5h://")
}

// BuildHTTPClient creates an HTTP client with proxy support.
// If proxy is nil or not configured, falls back to environment proxy settings.
func BuildHTTPClient(proxy *ProxyConfig) (*http.Client, error) {
	if proxy != nil && proxy.IsSocks() {
		return buildSOCKS5Client(proxy)
	}
	return buildHTTPProxyClient(proxy)
}

// Transport timeouts shared by every client HFDesk builds. Each bounds a
// phase that can otherwise block forever: a connect that never completes, a
// server that accepts the request but never sends response headers, and an
// HTTP/2 connection that stays open while going silent (health-checked with a
// ping). These are deliberately generous: the stall watchdog covers the
// body-transfer phase, so these only need to break true hangs.
const (
	// defaultDialTimeout bounds establishing the TCP connection.
	defaultDialTimeout = 30 * time.Second
	// defaultResponseHeaderTimeout bounds waiting for response headers after
	// the request is written.
	defaultResponseHeaderTimeout = 60 * time.Second
	// defaultHTTP2ReadIdleTimeout is how long an HTTP/2 connection may be
	// read-idle before a health-check ping is sent.
	defaultHTTP2ReadIdleTimeout = 30 * time.Second
	// defaultHTTP2PingTimeout bounds waiting for the health-check ping reply;
	// a missed reply closes the connection.
	defaultHTTP2PingTimeout = 15 * time.Second
)

// socksHandshakeNanos holds an override for the SOCKS5 dial+handshake timeout,
// in nanoseconds. 0 means "use defaultDialTimeout". It is atomic because a
// transport's DialContext goroutine may outlive the code that reads or changes
// it (for example a test that lowers it, or an in-flight dial during shutdown).
var socksHandshakeNanos atomic.Int64

// socksHandshakeTimeout returns the effective SOCKS5 dial+handshake bound.
func socksHandshakeTimeout() time.Duration {
	if d := socksHandshakeNanos.Load(); d > 0 {
		return time.Duration(d)
	}
	return defaultDialTimeout
}

// newBaseTransport returns an http.Transport with dial, response-header and
// HTTP/2 health-check deadlines on every phase that can block. It is the
// shared base for both the HTTP-proxy and direct paths.
func newBaseTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
		// Enable HTTP/2 even though DialContext/TLSClientConfig are set, and
		// make it health-check a connection that goes silent.
		ForceAttemptHTTP2: true,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: defaultHTTP2ReadIdleTimeout,
			PingTimeout:     defaultHTTP2PingTimeout,
		},
	}
}

// buildHTTPProxyClient creates a client for HTTP/HTTPS proxies.
func buildHTTPProxyClient(proxyCfg *ProxyConfig) (*http.Client, error) {
	tr := newBaseTransport()

	// Configure proxy
	if proxyCfg != nil && proxyCfg.URL != "" {
		proxyURL, err := proxyCfg.GetProxyURL()
		if err != nil {
			return nil, err
		}

		// Build no_proxy function
		noProxyList := proxyCfg.NoProxy
		if noProxyList == "" && !proxyCfg.NoEnvProxy {
			noProxyList = os.Getenv("NO_PROXY")
			if noProxyList == "" {
				noProxyList = os.Getenv("no_proxy")
			}
		}

		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			if shouldBypassProxy(req.URL.Host, noProxyList) {
				return nil, nil
			}
			return proxyURL, nil
		}

		// Handle TLS settings for HTTPS proxy
		if proxyCfg.InsecureSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	} else if proxyCfg != nil && proxyCfg.NoEnvProxy {
		// Explicit no proxy
		tr.Proxy = nil
	} else {
		// Use environment proxy
		tr.Proxy = http.ProxyFromEnvironment
	}

	return &http.Client{Transport: tr}, nil
}

// buildSOCKS5Client creates a client for SOCKS5 proxies.
func buildSOCKS5Client(proxyCfg *ProxyConfig) (*http.Client, error) {
	proxyURL, err := proxyCfg.GetProxyURL()
	if err != nil {
		return nil, err
	}

	// Parse SOCKS5 address
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		host = net.JoinHostPort(proxyURL.Hostname(), "1080") // Default SOCKS5 port
	}

	// Setup authentication
	var auth *proxy.Auth
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		auth = &proxy.Auth{
			User:     proxyURL.User.Username(),
			Password: pass,
		}
	}

	// Create SOCKS5 dialer. Passing a ContextDialer as the forward dialer lets
	// the SOCKS5 handshake (both the TCP connect to the proxy and the protocol
	// exchange) inherit the request context and deadline, so an unresponsive
	// proxy cannot hang the download. proxy.ContextDialer is asserted below.
	forward := &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	dialer, err := proxy.SOCKS5("tcp", host, auth, forward)
	if err != nil {
		return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	// Prefer the context-aware dial path when the dialer supports it. The
	// proxy package always produces one, but keep the assertion explicit so a
	// future change that returns a non-context dialer cannot silently
	// reintroduce the hang.
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 dialer does not support context deadlines")
	}

	// Build no_proxy list
	noProxyList := proxyCfg.NoProxy
	if noProxyList == "" && !proxyCfg.NoEnvProxy {
		noProxyList = os.Getenv("NO_PROXY")
		if noProxyList == "" {
			noProxyList = os.Getenv("no_proxy")
		}
	}

	// Create transport with SOCKS5 dialer
	tr := newBaseTransport()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Bound the entire dial+handshake with a deadline derived here (at the
		// transport owner), so it holds even when the caller's request context
		// has no deadline (and cannot be removed by http.Transport wrapping it
		// with context.WithoutCancel). Deriving from the incoming ctx keeps
		// ordinary cancellation working.
		dialCtx, cancel := context.WithTimeout(ctx, socksHandshakeTimeout())
		defer cancel()

		// Check if we should bypass proxy
		bypassHost, _, _ := net.SplitHostPort(addr)
		if shouldBypassProxy(bypassHost, noProxyList) {
			return forward.DialContext(dialCtx, network, addr)
		}
		// Use SOCKS5 proxy; dialCtx bounds both the TCP connect to the proxy
		// and the SOCKS5 protocol exchange, so a stalled proxy yields a
		// deadline error instead of blocking forever.
		return ctxDialer.DialContext(dialCtx, network, addr)
	}

	if proxyCfg.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &http.Client{Transport: tr}, nil
}

// shouldBypassProxy checks if a host should bypass the proxy.
func shouldBypassProxy(host, noProxy string) bool {
	if noProxy == "" {
		return false
	}

	host = strings.ToLower(strings.TrimSpace(host))
	// Remove port if present
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	for _, pattern := range strings.Split(noProxy, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}

		// Wildcard - bypass all
		if pattern == "*" {
			return true
		}

		// Exact match
		if pattern == host {
			return true
		}

		// Domain suffix match (pattern starts with .)
		if strings.HasPrefix(pattern, ".") && strings.HasSuffix(host, pattern) {
			return true
		}

		// Domain suffix match (pattern without leading .)
		if strings.HasSuffix(host, "."+pattern) {
			return true
		}

		// CIDR match
		if strings.Contains(pattern, "/") {
			_, cidr, err := net.ParseCIDR(pattern)
			if err == nil {
				if ip := net.ParseIP(host); ip != nil && cidr.Contains(ip) {
					return true
				}
			}
		}
	}

	return false
}

// ProxyTestResult holds the result of a proxy connectivity test.
type ProxyTestResult struct {
	Success    bool          `json:"success"`
	StatusCode int           `json:"status_code"`
	Status     string        `json:"status"`
	Duration   time.Duration `json:"duration"`
	RemoteAddr string        `json:"remote_addr,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// TestProxy tests connectivity through the configured proxy.
// Returns a test result and error if the test failed.
func TestProxy(ctx context.Context, proxyCfg *ProxyConfig, testURL string) (*ProxyTestResult, error) {
	if testURL == "" {
		testURL = "https://huggingface.co/api/whoami-v2"
	}

	client, err := BuildHTTPClient(proxyCfg)
	if err != nil {
		return &ProxyTestResult{
			Success: false,
			Error:   fmt.Sprintf("failed to build client: %v", err),
		}, fmt.Errorf("failed to build client: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "HEAD", testURL, nil)
	if err != nil {
		return &ProxyTestResult{
			Success: false,
			Error:   fmt.Sprintf("failed to create request: %v", err),
		}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "hfdownloader/2")

	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start)

	if err != nil {
		return &ProxyTestResult{
			Success:  false,
			Duration: duration,
			Error:    fmt.Sprintf("proxy connection failed: %v", err),
		}, fmt.Errorf("proxy connection failed: %w", err)
	}
	defer resp.Body.Close()

	// Any response (even 401/403) means the proxy is working
	return &ProxyTestResult{
		Success:    true,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Duration:   duration,
	}, nil
}

// ProxyInfo contains information about proxy configuration.
type ProxyInfo struct {
	HTTPProxy      string `json:"http_proxy,omitempty"`
	HTTPSProxy     string `json:"https_proxy,omitempty"`
	NoProxy        string `json:"no_proxy,omitempty"`
	AllProxy       string `json:"all_proxy,omitempty"`
	EffectiveProxy string `json:"effective_proxy,omitempty"`
	Source         string `json:"source,omitempty"` // "config", "environment", "none"
}

// GetProxyInfo returns detailed proxy configuration information.
func GetProxyInfo() *ProxyInfo {
	info := &ProxyInfo{}

	// Collect environment variables
	info.HTTPProxy = os.Getenv("HTTP_PROXY")
	if info.HTTPProxy == "" {
		info.HTTPProxy = os.Getenv("http_proxy")
	}

	info.HTTPSProxy = os.Getenv("HTTPS_PROXY")
	if info.HTTPSProxy == "" {
		info.HTTPSProxy = os.Getenv("https_proxy")
	}

	info.NoProxy = os.Getenv("NO_PROXY")
	if info.NoProxy == "" {
		info.NoProxy = os.Getenv("no_proxy")
	}

	info.AllProxy = os.Getenv("ALL_PROXY")
	if info.AllProxy == "" {
		info.AllProxy = os.Getenv("all_proxy")
	}

	// Determine effective proxy
	if info.HTTPSProxy != "" {
		info.EffectiveProxy = info.HTTPSProxy
		info.Source = "environment"
	} else if info.HTTPProxy != "" {
		info.EffectiveProxy = info.HTTPProxy
		info.Source = "environment"
	} else if info.AllProxy != "" {
		info.EffectiveProxy = info.AllProxy
		info.Source = "environment"
	} else {
		info.Source = "none"
	}

	return info
}

// GetProxyInfoString returns a human-readable description of the proxy configuration.
//
// The description never carries the proxy credentials: the userinfo of the
// URL is dropped, and the separate credentials of the configuration are not
// shown either. The description is shown to the user and reaches the API
// responses, so the user of the proxy is as sensitive as its password.
func GetProxyInfoString(proxyCfg *ProxyConfig) string {
	if proxyCfg == nil || proxyCfg.URL == "" {
		// Check environment
		for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			if value := os.Getenv(name); value != "" {
				// The environment value can carry the credentials of
				// the proxy just like the configured URL does.
				return fmt.Sprintf("Environment: %s", sanitizeProxyURL(value))
			}
		}
		return "None (direct connection)"
	}

	return sanitizeProxyURL(proxyCfg.URL)
}
