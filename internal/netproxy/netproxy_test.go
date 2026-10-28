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

package netproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		err       bool
	}{
		{raw: "http://proxy", want: "http://proxy:80"},
		{raw: "HTTPS://user:secret@proxy/", want: "https://user:xxxxx@proxy:443"},
		{raw: "socks5://proxy", want: "socks5://proxy:1080"},
		{raw: "socks5h://proxy:9050", want: "socks5h://proxy:9050"},
		{raw: "direct", want: "direct"},
		{raw: "ftp://proxy", err: true},
		{raw: "proxy:3128", err: true},
		{raw: "http://", err: true},
	} {
		p, err := Parse(tc.raw, "")
		if tc.err {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", tc.raw, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.raw, err)
			continue
		}
		if got := p.String(); got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if p, err := Parse("", ""); p != nil || err != nil {
		t.Errorf(`Parse("") = %v, %v; want nil, nil`, p, err)
	}
	if _, err := Parse("ftp://u:secret@proxy", ""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestNoProxy(t *testing.T) {
	p, err := Parse("socks5://proxy:1080", DefaultNoProxy+",example.org")
	if err != nil {
		t.Fatal(err)
	}
	for host, proxied := range map[string]bool{
		"generativelanguage.googleapis.com:443": true,
		"8.8.8.8:443":                           true,
		"example.org:443":                       false,
		"api.example.org:443":                   false,
		"192.168.1.10:8123":                     false,
		"homeassistant.local:8123":              false,
		"homeassistant:8123":                    false,
		"127.0.0.1:8080":                        false,
		"localhost:8080":                        false,
	} {
		u, err := p.For(&url.URL{Scheme: "https", Host: host})
		if err != nil {
			t.Fatal(err)
		}
		if (u != nil) != proxied {
			t.Errorf("For(%s) = %v, want proxied %v", host, u, proxied)
		}
	}
}

// echoServer answers every line with the same line, upper-cased.
func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s := bufio.NewScanner(c)
				for s.Scan() {
					io.WriteString(c, strings.ToUpper(s.Text())+"\n")
				}
			}()
		}
	}()
	return l.Addr().String()
}

// roundTrip checks a connection to echoServer.
func roundTrip(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "PING\n" {
		t.Fatalf("read %q, %v; want PING", line, err)
	}
}

// connectProxy is an HTTP CONNECT proxy requiring user:pass.
func connectProxy(t *testing.T, tunnels *atomic.Int32) *httptest.Server {
	return httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			http.Error(w, "auth", http.StatusProxyAuthRequired)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tunnels.Add(1)
		w.WriteHeader(http.StatusOK)
		c, buf, _ := w.(http.Hijacker).Hijack()
		go func() { io.Copy(up, buf); up.Close() }()
		io.Copy(c, up)
		c.Close()
	}))
}

func TestDialHTTPProxies(t *testing.T) {
	target := echoServer(t)
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			var tunnels atomic.Int32
			srv := connectProxy(t, &tunnels)
			if scheme == "https" {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()

			u, _ := url.Parse(srv.URL)
			u.User = url.UserPassword("user", "pass")
			p, err := Parse(u.String(), "")
			if err != nil {
				t.Fatal(err)
			}
			if scheme == "https" {
				// The test certificate is for 127.0.0.1 but signed by
				// httptest's own CA: dial it the way the code does,
				// trusting that CA.
				orig := tlsConfig
				tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
				defer func() { tlsConfig = orig }()
			}
			// A loopback target is never proxied: name it differently.
			_, port, _ := net.SplitHostPort(target)
			p.fn = func(*url.URL) (*url.URL, error) { return p.url, nil }
			c, err := p.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, c)
			if tunnels.Load() != 1 {
				t.Errorf("%d tunnels, want 1", tunnels.Load())
			}

			u.User = url.UserPassword("user", "wrong")
			bad, _ := Parse(u.String(), "")
			bad.fn = func(*url.URL) (*url.URL, error) { return bad.url, nil }
			if _, err := bad.DialContext(context.Background(), "tcp", target); err == nil || !strings.Contains(err.Error(), "407") {
				t.Errorf("wrong password: %v, want 407", err)
			}
		})
	}
}

// socksProxy is a minimal SOCKS5 server without authentication.
func socksProxy(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				hdr := make([]byte, 2)
				io.ReadFull(r, hdr)
				io.ReadFull(r, make([]byte, hdr[1]))
				c.Write([]byte{5, 0})
				req := make([]byte, 4)
				io.ReadFull(r, req)
				var host string
				switch req[3] {
				case 1:
					ip := make([]byte, 4)
					io.ReadFull(r, ip)
					host = net.IP(ip).String()
				case 3:
					n, _ := r.ReadByte()
					name := make([]byte, n)
					io.ReadFull(r, name)
					host = string(name)
				}
				pb := make([]byte, 2)
				io.ReadFull(r, pb)
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))))
				if err != nil {
					c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, r)
				io.Copy(c, up)
			}()
		}
	}()
	return l.Addr().String()
}

func TestDialSOCKS5(t *testing.T) {
	target := echoServer(t)
	p, err := Parse("socks5h://"+socksProxy(t), "")
	if err != nil {
		t.Fatal(err)
	}
	p.fn = func(*url.URL) (*url.URL, error) { return p.url, nil }
	c, err := p.DialContext(context.Background(), "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)
}

func TestDialDirect(t *testing.T) {
	p, _ := Parse(Direct, "")
	c, err := p.DialContext(context.Background(), "tcp", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)
}
