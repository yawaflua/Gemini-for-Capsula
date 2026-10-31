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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// textProvider answers typed text with one audio chunk and a transcript,
// and records the sessions' options and tuning.
type textProvider struct {
	mu    sync.Mutex
	tn    provider.Tuning
	opts  []provider.SessionOptions
	texts chan string
	// emptyFirst makes the first text request complete without a reply.
	emptyFirst atomic.Bool
}

func (p *textProvider) Name() string                     { return "text" }
func (p *textProvider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }
func (p *textProvider) AcceptsText() bool                { return true }
func (p *textProvider) Tuning() provider.Tuning {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tn
}
func (p *textProvider) Tune(t provider.Tuning) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.Model != "" {
		p.tn.Model = t.Model
	}
	if t.Voice != "" {
		p.tn.Voice = t.Voice
	}
}
func (p *textProvider) Connect(_ context.Context, o provider.SessionOptions) (provider.Session, error) {
	p.mu.Lock()
	p.opts = append(p.opts, o)
	p.mu.Unlock()
	return &textSession{p: p, events: make(chan provider.Event, 8)}, nil
}
func (p *textProvider) sessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.opts)
}

type textSession struct {
	p      *textProvider
	events chan provider.Event
	once   sync.Once
}

func (s *textSession) StartTurn() error                            { return nil }
func (s *textSession) SendAudio([]byte) error                      { return nil }
func (s *textSession) EndTurn() error                              { return nil }
func (s *textSession) SendToolResults([]provider.ToolResult) error { return nil }
func (s *textSession) Events() <-chan provider.Event               { return s.events }
func (s *textSession) Err() error                                  { return nil }
func (s *textSession) Close() error {
	s.once.Do(func() { close(s.events) })
	return nil
}
func (s *textSession) SendText(text string) error {
	if s.p.emptyFirst.CompareAndSwap(true, false) {
		s.events <- provider.Event{Kind: provider.EventTurnComplete}
		return nil
	}
	s.p.texts <- text
	s.events <- provider.Event{Kind: provider.EventAudio, Audio: []byte{9, 9}}
	s.events <- provider.Event{Kind: provider.EventOutputTranscript, Text: "Ответ"}
	s.events <- provider.Event{Kind: provider.EventTurnComplete}
	return nil
}

type panelEnv struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	client *http.Client
	prov   *textProvider
}

func newPanelEnv(t *testing.T, settingsFile string) *panelEnv {
	t.Helper()
	p := &textProvider{tn: provider.Tuning{Model: "m1", Voice: "Kore"}, texts: make(chan string, 1)}
	s := New(Config{
		Provider: p, PanelPassword: "pw", SettingsFile: settingsFile, Language: "ru-RU",
		Logger: slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &panelEnv{t: t, srv: s, ts: ts, client: &http.Client{Jar: jar}, prov: p}
}

func (e *panelEnv) do(method, path string, body any, csrf bool) (int, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, r)
	if csrf {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *panelEnv) login() {
	e.t.Helper()
	if code, out := e.do("POST", "/panel/api/login", map[string]string{"password": "pw"}, true); code != 200 {
		e.t.Fatalf("login = %d %v", code, out)
	}
}

// dial connects a fake speaker and completes the handshake.
func (e *panelEnv) dial(ctx context.Context, features ...models.Feature) *transport.Conn {
	e.t.Helper()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+transport.Path, "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close("done") })
	err = conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "kitchen", InputFormat: models.DeviceInputFormat,
		Capabilities: []models.Action{models.ActionVolumeGet, models.ActionVolumeSet},
		Features:     features,
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	if f, err := conn.Read(ctx); err != nil || f.Message == nil || f.Message.Type != models.TypeReady {
		e.t.Fatalf("want ready, got %+v %v", f, err)
	}
	return conn
}

func (e *panelEnv) waitDevice() DeviceInfo {
	e.t.Helper()
	for range 100 {
		if ds := e.srv.Devices(); len(ds) == 1 {
			return ds[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("device did not register")
	return DeviceInfo{}
}

func TestPanelAuth(t *testing.T) {
	e := newPanelEnv(t, "")
	if code, _ := e.do("GET", "/panel/api/state", nil, false); code != 401 {
		t.Errorf("state without login = %d, want 401", code)
	}
	if code, _ := e.do("POST", "/panel/api/login", map[string]string{"password": "pw"}, false); code != 403 {
		t.Errorf("login without CSRF header = %d, want 403", code)
	}
	if code, _ := e.do("POST", "/panel/api/login", map[string]string{"password": "nope"}, true); code != 401 {
		t.Errorf("wrong password = %d, want 401", code)
	}
	e.login()
	code, st := e.do("GET", "/panel/api/state", nil, false)
	if code != 200 || st["provider"] != "text" || st["tunable"] != true || st["text"] != true {
		t.Errorf("state = %d %v", code, st)
	}
	if code, _ := e.do("PUT", "/panel/api/settings", map[string]string{"language": "en-US"}, false); code != 403 {
		t.Errorf("settings without CSRF header = %d, want 403", code)
	}
	e.do("POST", "/panel/api/logout", nil, true)
	if code, _ := e.do("GET", "/panel/api/state", nil, false); code != 401 {
		t.Errorf("state after logout = %d, want 401", code)
	}
}

func TestPanelDisabledWithoutPassword(t *testing.T) {
	ts := httptest.NewServer(New(Config{Provider: &toolProvider{}, Logger: slog.New(slog.DiscardHandler)}))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("panel without password = %d, want 404", resp.StatusCode)
	}
}

func TestPanelStatic(t *testing.T) {
	e := newPanelEnv(t, "")
	resp, err := e.client.Get(e.ts.URL + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "app.js") {
		t.Errorf("index = %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestPanelCommand(t *testing.T) {
	e := newPanelEnv(t, "")
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := e.dial(ctx)
	d := e.waitDevice()

	type reply struct {
		code int
		out  map[string]any
	}
	done := make(chan reply, 1)
	go func() {
		code, out := e.do("POST", "/panel/api/devices/"+d.Session+"/command",
			map[string]any{"action": "volume.set", "args": map[string]any{"level": 42}}, true)
		done <- reply{code, out}
	}()
	var cmd *models.Command
	for cmd == nil {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Message != nil && f.Message.Type == models.TypeCommand {
			cmd = f.Message.Command
		}
	}
	if lvl, _ := cmd.Int("level"); cmd.Action != models.ActionVolumeSet || lvl != 42 {
		t.Fatalf("command = %+v", cmd)
	}
	conn.Send(ctx, &models.Message{Type: models.TypeCommandResult, CommandResult: cmd.Success(map[string]any{"level": 42})})
	r := <-done
	if r.code != 200 || r.out["ok"] != true {
		t.Errorf("command reply = %d %v", r.code, r.out)
	}

	if code, _ := e.do("POST", "/panel/api/devices/"+d.Session+"/command",
		map[string]any{"action": "light.night", "args": map[string]any{"on": true}}, true); code != 400 {
		t.Errorf("unsupported action = %d, want 400", code)
	}
	if code, _ := e.do("POST", "/panel/api/devices/nope/command",
		map[string]any{"action": "volume.get"}, true); code != 404 {
		t.Errorf("unknown device = %d, want 404", code)
	}

	var cmds int
	for _, ev := range e.srv.events.history() {
		if ev.Kind == EventCommand {
			cmds++
			if ev.Source != SourcePanel || ev.Name != "volume.set" {
				t.Errorf("command event = %+v", ev)
			}
		}
	}
	if cmds != 1 {
		t.Errorf("%d command events, want 1", cmds)
	}
}

func TestPanelSay(t *testing.T) {
	e := newPanelEnv(t, "")
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// An old agent cannot play a turn it did not start.
	old := e.dial(ctx)
	d := e.waitDevice()
	if d.CanSay {
		t.Error("CanSay for a device without remote_turn")
	}
	if code, _ := e.do("POST", "/panel/api/devices/"+d.Session+"/say", map[string]string{"text": "привет"}, true); code != 501 {
		t.Errorf("say to an old agent = %d, want 501", code)
	}
	old.Close("bye")
	for len(e.srv.Devices()) != 0 {
		time.Sleep(10 * time.Millisecond)
	}

	conn := e.dial(ctx, models.FeatureRemoteTurn)
	d = e.waitDevice()
	e.prov.emptyFirst.Store(true) // an unanswered first request is retried
	if !d.CanSay {
		t.Fatal("CanSay is false")
	}
	if code, out := e.do("POST", "/panel/api/devices/"+d.Session+"/say", map[string]string{"text": "какая погода?"}, true); code != 202 {
		t.Fatalf("say = %d %v", code, out)
	}
	if got := <-e.prov.texts; got != "какая погода?" {
		t.Errorf("provider got %q", got)
	}

	var turnID string
	var audio bool
	for {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Message == nil {
			audio = true
			continue
		}
		switch f.Message.Type {
		case models.TypeRemoteTurn:
			turnID = f.Message.TurnID
		case models.TypeTurnComplete:
			if turnID == "" || f.Message.TurnID != turnID {
				t.Fatalf("turn_complete %q before remote_turn %q", f.Message.TurnID, turnID)
			}
			if !audio {
				t.Error("no audio before turn_complete")
			}
			goto done
		}
	}
done:
	for range 100 {
		for _, ev := range e.srv.events.history() {
			if ev.Kind == EventTurn {
				if ev.Source != SourcePanel || ev.User != "какая погода?" || ev.Model != "Ответ" {
					t.Errorf("turn event = %+v", ev)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no turn event")
}

func TestPanelSettings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	e := newPanelEnv(t, file)
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.dial(ctx)
	e.waitDevice()

	bad := map[string]string{"language": "ru-RU", "default_timezone": "Mars/Olympus", "model": "m1", "voice": "Kore"}
	if code, _ := e.do("PUT", "/panel/api/settings", bad, true); code != 400 {
		t.Errorf("bad time zone = %d, want 400", code)
	}

	before := e.prov.sessions()
	set := map[string]string{
		"system_instruction": "Ты пират.", "language": "ru-RU", "default_location": "Хайфа",
		"default_timezone": "Asia/Jerusalem", "model": "m2", "voice": "Puck",
	}
	if code, out := e.do("PUT", "/panel/api/settings", set, true); code != 200 {
		t.Fatalf("settings = %d %v", code, out)
	}
	if tn := e.prov.Tuning(); tn.Model != "m2" || tn.Voice != "Puck" {
		t.Errorf("tuning = %+v", tn)
	}
	// The idle session is replaced and the new one uses the new prompt.
	deadline := time.Now().Add(2 * time.Second)
	for e.prov.sessions() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	e.prov.mu.Lock()
	last := e.prov.opts[len(e.prov.opts)-1]
	e.prov.mu.Unlock()
	if !strings.HasPrefix(last.SystemInstruction, "Ты пират.") || !strings.Contains(last.SystemInstruction, "Хайфа") {
		t.Errorf("new session prompt = %q", last.SystemInstruction)
	}

	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var saved Settings
	json.Unmarshal(b, &saved)
	if saved.Language != "" || saved.DefaultLocation != "Хайфа" || saved.Voice != "Puck" {
		t.Errorf("saved overrides = %+v, want only the changed fields", saved)
	}

	// A restarted server picks the saved settings up.
	e2 := newPanelEnv(t, file)
	if got := e2.srv.Settings(); got.DefaultLocation != "Хайфа" || got.SystemInstruction != "Ты пират." {
		t.Errorf("settings after restart = %+v", got)
	}
	if tn := e2.prov.Tuning(); tn.Voice != "Puck" {
		t.Errorf("tuning after restart = %+v", tn)
	}

	if code, _ := e.do("DELETE", "/panel/api/settings", nil, true); code != 200 {
		t.Errorf("reset = %d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("settings file still exists after reset: %v", err)
	}
	if got := e.srv.Settings(); got.SystemInstruction != DefaultSystemInstruction || got.Voice != "Kore" {
		t.Errorf("settings after reset = %+v", got)
	}
}

// TestAgentVersion checks that the panel flags an agent that is not the
// server's build.
func TestAgentVersion(t *testing.T) {
	for _, tc := range []struct {
		server, agent string
		outdated      bool
	}{
		{"c01c399", "c01c399", false},
		{"c01c399", "f6e2cf3", true},
		{"c01c399", "", true}, // too old to report it
		{"dev", "f6e2cf3", false},
	} {
		e := newPanelEnv(t, "")
		e.srv.cfg.Version = tc.server
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+transport.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		err = conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
			ProtocolVersion: models.ProtocolVersion, DeviceID: "kitchen", InputFormat: models.DeviceInputFormat,
			AgentVersion: tc.agent, AutoUpdate: true,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
		d := e.waitDevice()
		if d.Outdated != tc.outdated || d.AgentVersion != tc.agent || !d.AutoUpdate {
			t.Errorf("server %q, agent %q: %+v", tc.server, tc.agent, d)
		}
		conn.Close("done")
		cancel()
	}
}
