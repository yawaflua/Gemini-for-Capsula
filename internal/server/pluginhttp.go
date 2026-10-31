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

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

const (
	// PluginHTTPPrefix is where server plugins serve HTTP:
	// /plugins/<id>/<path>.
	PluginHTTPPrefix = "/plugins/"
	maxPluginBody    = 4 << 20
	pluginHTTPTime   = time.Minute
)

// hopHeaders are not passed between the client and the plugin.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Te", "Trailer"}

// handlePluginHTTP passes a request to /plugins/<id>/... to the plugin.
// Routes that are not public need the panel login (and, except for GET,
// the X-Marusya-Panel header) or the server token.
func (s *Server) handlePluginHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.plugins == nil {
		http.NotFound(w, r)
		return
	}
	spec := s.plugins.HTTPSpec(id)
	if spec == nil {
		http.NotFound(w, r)
		return
	}
	if !spec.Public && !s.pluginHTTPAllowed(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="marusya"`)
		http.Error(w, "нужно войти в панель или передать Authorization: Bearer <MARUSYA_AUTH_TOKEN>", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPluginBody))
	if err != nil {
		http.Error(w, "тело запроса больше 4 МБ", http.StatusRequestEntityTooLarge)
		return
	}
	header := r.Header.Clone()
	for _, h := range append(hopHeaders, "Cookie", "Authorization") {
		// The panel cookie and the server token stay on the server.
		if h == "Authorization" && spec.Public {
			continue // a public route may check its own token
		}
		header.Del(h)
	}
	req := pluginsdk.HTTPRequest{
		Method: r.Method, Path: "/" + r.PathValue("path"), Query: r.URL.RawQuery,
		Header: header, Body: body, RemoteAddr: r.RemoteAddr,
	}
	ctx, cancel := context.WithTimeout(r.Context(), pluginHTTPTime)
	defer cancel()
	res, err := s.plugins.HTTP(ctx, id, req)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
		}
		s.cfg.Logger.Warn("plugin HTTP", "plugin", id, "path", req.Path, "err", err)
		http.Error(w, "плагин не ответил: "+err.Error(), code)
		return
	}
	for k, vs := range res.Header {
		if isHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" && len(res.Body) > 0 {
		w.Header().Set("Content-Type", http.DetectContentType(res.Body))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	status := res.Status
	if status < 100 || status > 999 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	w.Write(res.Body)
}

func isHop(h string) bool {
	for _, x := range hopHeaders {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}

// pluginHTTPAllowed reports whether r may use a route that is not public:
// it comes from the panel or carries the server token.
func (s *Server) pluginHTTPAllowed(r *http.Request) bool {
	if s.authorizedToken(bearerToken(r)) {
		return true
	}
	// With the panel cookie, a change needs the panel header, as in the
	// panel API, so that other sites cannot send it.
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	return s.panel != nil && s.panel.valid(r) && (safe || r.Header.Get(csrfHeader) == "1")
}
