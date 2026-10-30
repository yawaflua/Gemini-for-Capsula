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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yawaflua/marusya-gemini/configs"
	agentdeploy "github.com/yawaflua/marusya-gemini/deploy/agent"
)

// installRequest sets up a speaker over SSH from the web panel, the same as
// "make deploy-agent": the OpenRC service and its config, which download
// the agent from this server on every start. The password is used once and
// never stored.
type installRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	// Server is the stream URL as the speaker reaches this server, e.g.
	// ws://192.168.1.10:8080/v1/stream.
	Server string `json:"server"`
}

type installResult struct {
	// HostKey is the SHA256 fingerprint of the speaker's SSH key.
	HostKey string `json:"host_key"`
	Output  string `json:"output"`
}

const installTimeout = 2 * time.Minute

// installScript runs on the speaker. Stdin carries the init script, the
// default config and the settings to set, after __INIT__, __CONF__ and
// __SET__ lines. An existing config is kept, only the __SET__ keys change.
// Files are rewritten with cat: the stock rootfs may not allow renames.
// INSTALL_ROOT is for tests.
const installScript = `set -e
r="${INSTALL_ROOT:-}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
awk -v d="$d" '/^__INIT__$/{f=d"/init";next} /^__CONF__$/{f=d"/conf";next} /^__SET__$/{f=d"/set";next} {print > f}'
cat "$d/init" > "$r/etc/init.d/marusya-agent"
chmod 0755 "$r/etc/init.d/marusya-agent"
[ -f "$r/etc/conf.d/marusya-agent" ] || cat "$d/conf" > "$r/etc/conf.d/marusya-agent"
awk 'NR==FNR{k=$0; sub(/=.*/,"",k); v[k]=$0; next}
	{k=$0; sub(/=.*/,"",k); if (k in v) {print v[k]; delete v[k]} else print}
	END{for (k in v) print v[k]}' "$d/set" "$r/etc/conf.d/marusya-agent" > "$d/new"
cat "$d/new" > "$r/etc/conf.d/marusya-agent"
sync
rc-update add marusya-agent default >/dev/null 2>&1 || true
rc-service marusya-agent restart
`

func (req *installRequest) validate() error {
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.Server = strings.TrimSpace(req.Server)
	if req.Host == "" || strings.ContainsAny(req.Host, " /\t\r\n") {
		return errors.New("укажите адрес колонки, например 192.168.1.50")
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return errors.New("неверный порт SSH")
	}
	if req.User == "" {
		req.User = "root"
	}
	u, err := url.Parse(req.Server)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return errors.New("адрес сервера должен быть вида ws://<ip>:8080/v1/stream")
	}
	if strings.ContainsAny(req.Server, " \t\r\n") {
		return errors.New("адрес сервера не должен содержать пробелов")
	}
	return nil
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.ContainsAny(s.cfg.AuthToken, "\r\n") {
		writeError(w, http.StatusInternalServerError, errors.New("токен сервера содержит перевод строки"))
		return
	}
	log := s.cfg.Logger.With("host", req.Host, "remote", r.RemoteAddr)
	log.Info("installing the agent service on a speaker")
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	res, err := installAgent(ctx, req, s.cfg.AuthToken)
	if err != nil {
		log.Warn("install failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "output": res.Output, "host_key": res.HostKey})
		return
	}
	log.Info("agent service installed", "host_key", res.HostKey)
	writeJSON(w, http.StatusOK, res)
}

// installAgent connects to the speaker and runs [installScript].
func installAgent(ctx context.Context, req installRequest, token string) (installResult, error) {
	var res installResult
	cfg := &ssh.ClientConfig{
		User: req.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(req.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = req.Password
				}
				return answers, nil
			}),
		},
		// Speakers have no known host key yet; the fingerprint is shown
		// in the panel instead.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			res.HostKey = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 10 * time.Second,
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return res, fmt.Errorf("не удалось подключиться к %s: %w", addr, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return res, errors.New("неверный логин или пароль SSH")
		}
		return res, fmt.Errorf("SSH: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return res, fmt.Errorf("SSH: %w", err)
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(installInput(req.Server, token))
	var out lockedBuffer
	sess.Stdout = &out
	sess.Stderr = &out
	err = sess.Run(installScript)
	res.Output = strings.TrimSpace(out.String())
	if ctx.Err() != nil {
		return res, errors.New("колонка не ответила вовремя")
	}
	if err != nil {
		return res, fmt.Errorf("установка на колонке не удалась: %w", err)
	}
	return res, nil
}

// lockedBuffer is a buffer for both stdout and stderr, which an SSH session
// copies from two goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// installInput is the stdin of [installScript].
func installInput(server, token string) string {
	var b strings.Builder
	b.WriteString("__INIT__\n")
	b.WriteString(strings.TrimSuffix(agentdeploy.OpenRC, "\n") + "\n")
	b.WriteString("__CONF__\n")
	b.WriteString(strings.TrimSuffix(configs.AgentConf, "\n") + "\n")
	b.WriteString("__SET__\n")
	b.WriteString("MARUSYA_SERVER=" + shellQuote(server) + "\n")
	b.WriteString("MARUSYA_AUTH_TOKEN=" + shellQuote(token) + "\n")
	return b.String()
}

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+=%-]*$`)

// shellQuote quotes v for /etc/conf.d, which OpenRC reads as shell.
func shellQuote(v string) string {
	if shellSafeRE.MatchString(v) {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
