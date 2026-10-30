// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package netproxy routes outgoing connections of the server through an
// HTTP, HTTPS or SOCKS5 proxy.
package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

// Direct is the proxy address that disables proxying.
const Direct = "direct"

// DefaultNoProxy lists the hosts reached directly by default: private
// networks and local domains, where Home Assistant and the like live.
const DefaultNoProxy = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7,fe80::/10,.local,.lan,.home,.internal,.home.arpa,.localhost"

// tlsConfig is the base TLS configuration for https:// proxies (tests).
var tlsConfig *tls.Config

// Proxy decides which proxy, if any, a connection goes through.
type Proxy struct {
	url *url.URL // nil: direct
	fn  func(*url.URL) (*url.URL, error)
}

// Parse parses a proxy address such as "http://host:3128",
// "https://user:pass@host", "socks5://host:1080" or "socks5h://host".
// [Direct] returns a Proxy that connects directly; "" returns nil, which
// means "not configured".
//
// noProxy is a comma separated list of hosts, domains (".example.com") and
// CIDR ranges reached directly, as in $NO_PROXY. Loopback addresses and
// single-label hosts (Docker service names, "homeassistant") are always
// reached directly.
func Parse(raw, noProxy string) (*Proxy, error) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		return nil, nil
	case Direct, "none", "off":
		return &Proxy{fn: func(*url.URL) (*url.URL, error) { return nil, nil }}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", redact(raw), err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	var port string
	switch u.Scheme {
	case "http":
		port = "80"
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	default:
		return nil, fmt.Errorf("proxy %q: scheme must be http, https, socks5 or socks5h", redact(raw))
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("proxy %q: no host", redact(raw))
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""

	cfg := httpproxy.Config{HTTPProxy: u.String(), HTTPSProxy: u.String(), NoProxy: noProxy}
	byEnv := cfg.ProxyFunc()
	return &Proxy{url: u, fn: func(target *url.URL) (*url.URL, error) {
		if h := target.Hostname(); h != "" && !strings.ContainsAny(h, ".:") {
			return nil, nil
		}
		return byEnv(target)
	}}, nil
}

// URL returns the proxy address, nil for a direct connection.
func (p *Proxy) URL() *url.URL { return p.url }

// String returns the proxy address without the password.
func (p *Proxy) String() string {
	if p.url == nil {
		return Direct
	}
	return p.url.Redacted()
}

// For returns the proxy for a request to target, nil to connect directly.
func (p *Proxy) For(target *url.URL) (*url.URL, error) { return p.fn(target) }

// Transport returns a copy of [http.DefaultTransport] that uses the proxy.
func (p *Proxy) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(r *http.Request) (*url.URL, error) { return p.fn(r.URL) }
	return t
}

// Client returns an HTTP client that uses the proxy.
func (p *Proxy) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: p.Transport(), Timeout: timeout}
}

// DialContext opens a TCP connection to addr through the proxy: HTTP and
// HTTPS proxies are asked to CONNECT, SOCKS5 ones to connect. It suits
// clients that dial themselves, such as WebSocket libraries.
func (p *Proxy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	pu, err := p.fn(&url.URL{Scheme: "https", Host: addr})
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	if pu == nil {
		return d.DialContext(ctx, network, addr)
	}
	switch pu.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if pu.User != nil {
			pw, _ := pu.User.Password()
			auth = &proxy.Auth{User: pu.User.Username(), Password: pw}
		}
		sd, err := proxy.SOCKS5("tcp", pu.Host, auth, &d)
		if err != nil {
			return nil, err
		}
		conn, err := sd.(proxy.ContextDialer).DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %s: %w", pu.Host, err)
		}
		return conn, nil
	}
	return connect(ctx, pu, addr)
}

// connect opens a tunnel to addr with an HTTP CONNECT request.
func connect(ctx context.Context, pu *url.URL, addr string) (_ net.Conn, err error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", pu.Host, err)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	// Unblock the handshake when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()

	if pu.Scheme == "https" {
		cfg := &tls.Config{}
		if tlsConfig != nil {
			cfg = tlsConfig.Clone()
		}
		cfg.ServerName = pu.Hostname()
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("proxy %s: %w", pu.Host, err)
		}
		conn = tc
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if pu.User != nil {
		pw, _ := pu.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)))
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("proxy %s: %w", pu.Host, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", pu.Host, err)
	}
	// A 200 response to CONNECT has no body, the tunnel follows: closing
	// the body would drain the tunnel until EOF on Go < 1.26.
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("proxy %s: CONNECT %s: %s", pu.Host, addr, resp.Status)
	}
	if !stop() {
		return nil, ctx.Err()
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn returns the bytes read ahead with the CONNECT response first.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// redact hides the password of a proxy address that failed to parse.
func redact(raw string) string {
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			return raw[:j+3] + "***" + raw[i:]
		}
		return "***" + raw[i:]
	}
	return raw
}
