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
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/registry"
)

// otherFormat is a provider whose speech has another sample rate.
type otherFormat struct{ *textProvider }

func (otherFormat) OutputFormat() models.AudioFormat {
	return models.AudioFormat{Encoding: models.EncodingLinear16, SampleRateHertz: 16000, Channels: 1}
}

func TestPanelProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	built := map[string]*textProvider{}
	build := func(_ context.Context, s registry.Spec, _ []registry.Spec) (provider.Provider, error) {
		p := &textProvider{tn: provider.Tuning{Model: s.Model, Voice: s.Voice}, texts: make(chan string, 1)}
		mu.Lock()
		built[s.ID] = p
		mu.Unlock()
		if s.Kind == registry.KindOpenAIRealtime {
			return otherFormat{p}, nil
		}
		return p, nil
	}
	dir := t.TempDir()
	def := &textProvider{tn: provider.Tuning{Model: "m1", Voice: "Kore"}, texts: make(chan string, 1)}
	reg, err := registry.Open(filepath.Join(dir, "providers.json"), def, build, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		Providers: reg, ProvidersFile: "providers.json", PanelPassword: "pw",
		SettingsFile: filepath.Join(dir, "settings.json"), Language: "ru-RU",
		Logger: slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(s)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	e := &panelEnv{t: t, srv: s, ts: ts, client: &http.Client{Jar: jar}, prov: def}
	e.login()

	code, st := e.do("GET", "/panel/api/providers", nil, false)
	if code != 200 || st["enabled"] != true || st["active"] != registry.DefaultID || len(st["providers"].([]any)) != 1 {
		t.Fatalf("providers = %d %v", code, st)
	}

	code, st = e.do("PUT", "/panel/api/providers/router", map[string]any{
		"name": "OpenRouter", "kind": "openai", "api_key": "sk-secret", "model": "m2", "voice": "nova",
	}, true)
	if code != 200 {
		t.Fatalf("put = %d %v", code, st)
	}
	p := st["providers"].([]any)[1].(map[string]any)
	if p["has_key"] != true || p["api_key"] != nil {
		t.Fatalf("saved provider = %v", p)
	}
	if code, out := e.do("PUT", "/panel/api/providers/bad", map[string]any{"name": "x", "kind": "nope"}, true); code != 400 {
		t.Fatalf("invalid provider accepted: %d %v", code, out)
	}

	conn := e.dial(ctx)
	e.waitDevice()

	// Same audio format: the speaker stays connected and moves over.
	if code, out := e.do("POST", "/panel/api/providers/router/activate", nil, true); code != 200 || out["active"] != "router" {
		t.Fatalf("activate = %d %v", code, out)
	}
	if got := s.Settings(); got.Model != "m2" || got.Voice != "nova" {
		t.Fatalf("settings after the switch = %+v", got)
	}
	mu.Lock()
	router := built["router"]
	mu.Unlock()
	for i := 0; router.sessions() == 0; i++ {
		if i > 200 {
			t.Fatal("the speaker did not open a session with the new provider")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := e.do("DELETE", "/panel/api/providers/router", nil, true); code != 400 {
		t.Fatal("deleted the active provider")
	}

	// Model changes in the settings are kept in the provider.
	set := s.Settings()
	set.Model = "m3"
	if _, err := s.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	if views, _ := reg.List(); views[1].Model != "m3" {
		t.Fatalf("provider model = %q, want m3", views[1].Model)
	}

	// Another audio format: the speaker must reconnect.
	if code, out := e.do("PUT", "/panel/api/providers/rt", map[string]any{"name": "Realtime", "kind": "openai-realtime"}, true); code != 200 {
		t.Fatalf("put = %d %v", code, out)
	}
	if code, out := e.do("POST", "/panel/api/providers/rt/activate", nil, true); code != 200 {
		t.Fatalf("activate = %d %v", code, out)
	}
	for {
		if _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the speaker was not disconnected")
			}
			if !strings.Contains(err.Error(), "provider changed") {
				t.Logf("closed: %v", err)
			}
			break
		}
	}
}

func TestPanelProvidersDisabled(t *testing.T) {
	e := newPanelEnv(t, "")
	e.login()
	code, st := e.do("GET", "/panel/api/providers", nil, false)
	if code != 200 || st["enabled"] != false {
		t.Fatalf("providers = %d %v", code, st)
	}
	if code, _ := e.do("POST", "/panel/api/providers/x/activate", nil, true); code != 404 {
		t.Fatalf("activate without a registry = %d", code)
	}
}
