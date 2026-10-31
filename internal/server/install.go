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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// the agent from this server on every start. Optionally also the agent's
// D-Bus service, the AirPlay 2 receiver and spotifyd. The password is used once and never stored.
type installRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	// Server is the stream URL as the speaker reaches this server, e.g.
	// ws://192.168.1.10:8080/v1/stream.
	Server string `json:"server"`
	// Name is how the speaker shows up in AirPlay and Spotify.
	Name string `json:"name"`
	// DBus registers ru.marusya.Agent on the speaker's system bus.
	DBus    bool `json:"dbus"`
	AirPlay bool `json:"airplay"`
	Spotify bool `json:"spotify"`
}

type installResult struct {
	// HostKey is the SHA256 fingerprint of the speaker's SSH key.
	HostKey string `json:"host_key"`
	Output  string `json:"output"`
	// SpotifyLogin, if set, waits for the Spotify sign-in, see
	// [Server.handleSpotifyLogin].
	SpotifyLogin *spotifyLoginInfo `json:"spotify_login,omitempty"`
}

type spotifyLoginInfo struct {
	ID  string `json:"id"`
	URL string `json:"url"` // Spotify's sign-in page
}

const (
	installTimeout = 5 * time.Minute // apk add on a slow speaker
	// spotifyLoginTimeout is how long "spotifyd authenticate" waits for the
	// address the browser was sent to after the sign-in.
	spotifyLoginTimeout = 10 * time.Minute
	// spotifyOAuthPort is where "spotifyd authenticate" listens for the
	// redirect after the sign-in (its default).
	spotifyOAuthPort = 8000
)

// airplayBundle is the AirPlay 2 receiver for VK Капсула Мини (shairport-sync,
// nqptp, OpenRC services, install.sh), pinned to a release commit.
const airplayBundle = "https://github.com/ryysel/capsula-mini-airplay2/archive/526959ea5687dddc5aff07ab39d15c0df7f86a0b.tar.gz"

const maxBundle = 32 << 20

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

// dbusScript registers the agent's service on the system bus: the D-Bus
// policy from stdin, the "marusya" group it lets drive the ring and the
// display, and the dbus service, which then rereads its policies. It runs
// before [installScript], whose agent restart then takes the bus name.
const dbusScript = `set -e
r="${INSTALL_ROOT:-}"
command -v dbus-daemon >/dev/null 2>&1 || apk add --no-cache dbus
mkdir -p "$r/etc/dbus-1/system.d"
cat > "$r/etc/dbus-1/system.d/ru.marusya.Agent.conf"
grep -q '^marusya:' "$r/etc/group" 2>/dev/null || addgroup -S marusya
sync
rc-update add dbus default >/dev/null 2>&1 || true
rc-service dbus status >/dev/null 2>&1 || rc-service dbus start
dbus-send --system --type=signal / org.freedesktop.DBus.ReloadConfig >/dev/null 2>&1 || true
echo "ru.marusya.Agent: правила D-Bus установлены"
`

// airplayScript unpacks the bundle from stdin and runs its installer, which
// keeps backups and is safe to run again.
func airplayScript(name string) string {
	return `set -e
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
cd "$d"
tar xzf -
cd capsula-mini-airplay2-*
sh install.sh --upgrade --name ` + shellQuote(name) + "\n" + airplayTune
}

// airplayTune adjusts the receiver's config for the airplay plugin: the
// metadata pipe it reads (what plays, the phone's volume), and no software
// volume in shairport-sync, so the phone's slider moves the speaker's
// volume, which the display and the panel show, instead of attenuating the
// stream behind them. A config already tuned is left alone.
const airplayTune = `c="${INSTALL_ROOT:-}/etc/shairport-sync.conf"
[ -f "$c" ] || exit 0
changed=
if ! grep -q 'ignore_volume_control' "$c"; then
	awk '/^[ \t]*general[ \t]*=/ && !done {print; print "\tignore_volume_control = \"yes\";"; done=1; next} {print}' "$c" > "$d/conf"
	cat "$d/conf" > "$c"
	changed=1
fi
if ! grep -q '^[ \t]*metadata[ \t]*=' "$c"; then
	printf '\nmetadata = {\n\tenabled = "yes";\n\tinclude_cover_art = "no";\n\tpipe_name = "/tmp/shairport-sync-metadata";\n};\n' >> "$c"
	changed=1
fi
if [ -n "$changed" ]; then
	sync
	rc-service shairport-sync restart
	echo "AirPlay: метаданные и громкость колонки включены"
fi
`

// spotifydScript installs spotifyd from apk and its service. Stdin carries
// the init script, the default config and the D-Bus policy after __INIT__,
// __CONF__ and __DBUS__ lines. An existing /etc/spotifyd.conf is kept.
const spotifydScript = `set -e
r="${INSTALL_ROOT:-}"
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
awk -v d="$d" '/^__INIT__$/{f=d"/init";next} /^__CONF__$/{f=d"/conf";next} /^__DBUS__$/{f=d"/dbus";next} {print > f}'
command -v spotifyd >/dev/null 2>&1 || apk add --no-cache spotifyd playerctl dbus
spotifyd --version
mkdir -p "$r/etc/dbus-1/system.d" "$r/var/cache/spotifyd"
cat "$d/init" > "$r/etc/init.d/spotifyd"
chmod 0755 "$r/etc/init.d/spotifyd"
[ -f "$r/etc/spotifyd.conf" ] || cat "$d/conf" > "$r/etc/spotifyd.conf"
cat "$d/dbus" > "$r/etc/dbus-1/system.d/spotifyd.conf"
sync
rc-update add dbus default >/dev/null 2>&1 || true
rc-update add spotifyd default >/dev/null 2>&1 || true
rc-service dbus status >/dev/null 2>&1 || rc-service dbus start
dbus-send --system --type=signal / org.freedesktop.DBus.ReloadConfig >/dev/null 2>&1 || true
rc-service spotifyd restart
`

var spotifyAuthCommand = "spotifyd authenticate --config-path /etc/spotifyd.conf --oauth-port " + strconv.Itoa(spotifyOAuthPort) + " 2>&1"

func (req *installRequest) validate() error {
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.Server = strings.TrimSpace(req.Server)
	req.Name = strings.TrimSpace(req.Name)
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
	if req.Name == "" {
		req.Name = "Маруся"
	}
	// The limits of the AirPlay installer; the name also goes into a TOML
	// string for spotifyd.
	if len(req.Name) > 50 || strings.ContainsFunc(req.Name, func(r rune) bool { return r < ' ' || r == 0x7f || r == '"' || r == '\\' }) {
		return errors.New("имя колонки: до 50 байт, без кавычек и обратной косой черты")
	}
	return nil
}

// installer runs the install over SSH.
type installer struct {
	http       *http.Client // downloads the AirPlay bundle
	airplayURL string
	token      string // MARUSYA_AUTH_TOKEN for the speaker
}

func (s *Server) installer() *installer {
	return &installer{http: s.cfg.HTTPClient, airplayURL: airplayBundle, token: s.cfg.AuthToken}
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
	log := s.cfg.Logger.With("host", req.Host, "remote", r.RemoteAddr, "dbus", req.DBus, "airplay", req.AirPlay, "spotify", req.Spotify)
	log.Info("installing on a speaker")
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	res, login, err := s.installer().run(ctx, req)
	if err != nil {
		log.Warn("install failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "output": res.Output, "host_key": res.HostKey})
		return
	}
	if login != nil {
		res.SpotifyLogin = &spotifyLoginInfo{ID: s.logins.add(login), URL: login.url}
	}
	log.Info("speaker set up", "host_key", res.HostKey)
	writeJSON(w, http.StatusOK, res)
}

// run connects to the speaker and runs the install steps. With
// req.Spotify it returns the started Spotify sign-in, which owns the SSH
// connection from then on.
func (in *installer) run(ctx context.Context, req installRequest) (res installResult, _ *spotifyLogin, _ error) {
	var out lockedBuffer
	defer func() { res.Output = strings.TrimSpace(out.String()) }()

	var bundle []byte
	if req.AirPlay {
		var err error
		if bundle, err = in.download(ctx, in.airplayURL); err != nil {
			return res, nil, fmt.Errorf("не удалось скачать AirPlay: %w", err)
		}
	}

	client, stop, err := dialSpeaker(ctx, req, &res.HostKey)
	if err != nil {
		return res, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			client.Close()
		}
	}()

	if req.DBus {
		fmt.Fprintln(&out, "== D-Bus ==")
		if err := runRemote(ctx, client, dbusScript, strings.NewReader(agentdeploy.AgentDBus), &out); err != nil {
			return res, nil, fmt.Errorf("регистрация в D-Bus не удалась: %w", err)
		}
		fmt.Fprintln(&out)
	}
	fmt.Fprintln(&out, "== Агент ==")
	if err := runRemote(ctx, client, installScript, strings.NewReader(installInput(req.Server, in.token, req.DBus)), &out); err != nil {
		return res, nil, fmt.Errorf("установка агента не удалась: %w", err)
	}
	if req.AirPlay {
		fmt.Fprintln(&out, "\n== AirPlay ==")
		if err := runRemote(ctx, client, airplayScript(req.Name), bytes.NewReader(bundle), &out); err != nil {
			return res, nil, fmt.Errorf("установка AirPlay не удалась: %w", err)
		}
	}
	if !req.Spotify {
		return res, nil, nil
	}
	fmt.Fprintln(&out, "\n== Spotify ==")
	if err := runRemote(ctx, client, spotifydScript, strings.NewReader(spotifydInput(req.Name)), &out); err != nil {
		return res, nil, fmt.Errorf("установка spotifyd не удалась: %w", err)
	}
	login, err := startSpotifyLogin(ctx, client)
	if err != nil {
		return res, nil, fmt.Errorf("вход в Spotify: %w", err)
	}
	// The sign-in outlives this request.
	stop()
	keep = true
	return res, login, nil
}

func (in *installer) download(ctx context.Context, u string) ([]byte, error) {
	hc := in.http
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBundle {
		return nil, errors.New("архив слишком большой")
	}
	return b, nil
}

// dialSpeaker opens an SSH connection, closed when ctx ends until stop is
// called. hostKey receives the speaker's key fingerprint.
func dialSpeaker(ctx context.Context, req installRequest, hostKey *string) (_ *ssh.Client, stop func() bool, _ error) {
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
			*hostKey = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 10 * time.Second,
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось подключиться к %s: %w", addr, err)
	}
	stop = context.AfterFunc(ctx, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		stop()
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, nil, errors.New("неверный логин или пароль SSH")
		}
		return nil, nil, fmt.Errorf("SSH: %w", err)
	}
	return ssh.NewClient(c, chans, reqs), stop, nil
}

// runRemote runs script on the speaker with stdin, appending its output.
func runRemote(ctx context.Context, client *ssh.Client, script string, stdin io.Reader, out io.Writer) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH: %w", err)
	}
	defer sess.Close()
	sess.Stdin = stdin
	sess.Stdout = out
	sess.Stderr = out
	err = sess.Run(script)
	if ctx.Err() != nil {
		return errors.New("колонка не ответила вовремя")
	}
	return err
}

// spotifyLogin is a running "spotifyd authenticate" on a speaker. It
// waits for the address of the page Spotify redirected the browser to after
// the sign-in (http://127.0.0.1:8000/login?code=...), which only the
// speaker can open.
type spotifyLogin struct {
	client *ssh.Client
	sess   *ssh.Session
	url    string
	out    *lockedBuffer
	done   chan error // the result of sess.Wait
	timer  *time.Timer
}

func startSpotifyLogin(ctx context.Context, client *ssh.Client) (*spotifyLogin, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	pr, pw := io.Pipe()
	sess.Stdout = pw
	if err := sess.Start(spotifyAuthCommand); err != nil {
		sess.Close()
		return nil, err
	}
	l := &spotifyLogin{client: client, sess: sess, out: new(lockedBuffer), done: make(chan error, 1)}
	go func() {
		err := sess.Wait()
		pw.Close()
		l.done <- err
	}()
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(l.out, line)
			if _, u, ok := strings.Cut(line, "Browse to: "); ok {
				select {
				case found <- strings.TrimSpace(u):
				default:
				}
			}
		}
		io.Copy(io.Discard, pr)
	}()
	wait := time.NewTimer(30 * time.Second)
	defer wait.Stop()
	select {
	case l.url = <-found:
		return l, nil
	case err := <-l.done:
		return nil, fmt.Errorf("spotifyd authenticate завершился (%v): %s", err, strings.TrimSpace(l.out.String()))
	case <-wait.C:
	case <-ctx.Done():
	}
	sess.Close()
	return nil, fmt.Errorf("spotifyd authenticate не дал ссылку для входа: %s", strings.TrimSpace(l.out.String()))
}

func (l *spotifyLogin) close() {
	l.sess.Close()
	l.client.Close()
}

// finish passes the redirect to the waiting spotifyd and restarts the
// service, which then signs in with the saved credentials.
func (l *spotifyLogin) finish(ctx context.Context, redirect string) (string, error) {
	u, err := spotifyRedirect(redirect)
	if err != nil {
		return "", err
	}
	var out lockedBuffer
	// The listener answers once and spotifyd then exits; wget may report
	// an error for the closed connection, which does not matter.
	runRemote(ctx, l.client, "wget -q -O /dev/null -T 15 "+shellQuote(u)+" 2>&1 || true", nil, &out)
	select {
	case err := <-l.done:
		fmt.Fprint(&out, l.out.String())
		if err != nil {
			return strings.TrimSpace(out.String()), fmt.Errorf("spotifyd не принял вход: %w", err)
		}
	case <-time.After(30 * time.Second):
		return strings.TrimSpace(l.out.String()), errors.New("spotifyd не ответил после входа")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := runRemote(ctx, l.client, "rc-service spotifyd restart", nil, &out); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("перезапуск spotifyd: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// spotifyRedirect checks the address pasted from the browser and rebuilds
// it for the speaker's loopback, so only the query of the pasted text gets
// through.
func spotifyRedirect(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	bad := errors.New("вставьте адрес страницы, на которую Spotify перенаправил браузер: http://127.0.0.1:" + strconv.Itoa(spotifyOAuthPort) + "/login?code=…")
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/login" {
		return "", bad
	}
	q := u.Query()
	if q.Get("code") == "" {
		if e := q.Get("error"); e != "" {
			return "", fmt.Errorf("Spotify отказал во входе: %s", e)
		}
		return "", bad
	}
	clean := url.Values{}
	for _, k := range []string{"code", "state"} {
		if v := q.Get(k); v != "" {
			clean.Set(k, v)
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(spotifyOAuthPort) + "/login?" + clean.Encode(), nil
}

// spotifyLogins are the sign-ins waiting for the panel, by ID.
type spotifyLogins struct {
	mu sync.Mutex
	m  map[string]*spotifyLogin
}

func (ls *spotifyLogins) add(l *spotifyLogin) string {
	b := make([]byte, 12)
	rand.Read(b)
	id := hex.EncodeToString(b)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.m == nil {
		ls.m = map[string]*spotifyLogin{}
	}
	ls.m[id] = l
	l.timer = time.AfterFunc(spotifyLoginTimeout, func() {
		if l := ls.take(id); l != nil {
			l.close()
		}
	})
	return id
}

// take removes and returns the sign-in, nil if there is none.
func (ls *spotifyLogins) take(id string) *spotifyLogin {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l := ls.m[id]
	if l != nil {
		delete(ls.m, id)
		l.timer.Stop()
	}
	return l
}

type spotifyLoginRequest struct {
	ID       string `json:"id"`
	Redirect string `json:"redirect"`
}

func (s *Server) handleSpotifyLogin(w http.ResponseWriter, r *http.Request) {
	var req spotifyLoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := spotifyRedirect(req.Redirect); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	l := s.logins.take(req.ID)
	if l == nil {
		writeError(w, http.StatusNotFound, errors.New("вход устарел, запустите установку Spotify ещё раз"))
		return
	}
	defer l.close()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := l.finish(ctx, req.Redirect)
	if err != nil {
		s.cfg.Logger.Warn("spotify sign-in failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "output": out})
		return
	}
	s.cfg.Logger.Info("spotifyd signed in on a speaker")
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
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

// installInput is the stdin of [installScript]. With dbus it also turns
// the agent's D-Bus service on, in case the config had it off.
func installInput(server, token string, dbus bool) string {
	var b strings.Builder
	b.WriteString("__INIT__\n")
	b.WriteString(strings.TrimSuffix(agentdeploy.OpenRC, "\n") + "\n")
	b.WriteString("__CONF__\n")
	b.WriteString(strings.TrimSuffix(configs.AgentConf, "\n") + "\n")
	b.WriteString("__SET__\n")
	b.WriteString("MARUSYA_SERVER=" + shellQuote(server) + "\n")
	b.WriteString("MARUSYA_AUTH_TOKEN=" + shellQuote(token) + "\n")
	if dbus {
		b.WriteString("MARUSYA_DBUS=true\n")
	}
	return b.String()
}

// spotifydInput is the stdin of [spotifydScript].
func spotifydInput(name string) string {
	conf := strings.Replace(agentdeploy.SpotifydConf, `device_name = "Marusya"`, `device_name = "`+name+`"`, 1)
	var b strings.Builder
	b.WriteString("__INIT__\n")
	b.WriteString(strings.TrimSuffix(agentdeploy.SpotifydOpenRC, "\n") + "\n")
	b.WriteString("__CONF__\n")
	b.WriteString(strings.TrimSuffix(conf, "\n") + "\n")
	b.WriteString("__DBUS__\n")
	b.WriteString(strings.TrimSuffix(agentdeploy.SpotifydDBus, "\n") + "\n")
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
