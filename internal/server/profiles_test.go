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
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// optsProvider records the options of every session it opens.
type optsProvider struct{ opts chan provider.SessionOptions }

func (p *optsProvider) Name() string                     { return "opts" }
func (p *optsProvider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }
func (p *optsProvider) Connect(_ context.Context, o provider.SessionOptions) (provider.Session, error) {
	p.opts <- o
	return &slowSession{events: make(chan provider.Event, 8)}, nil
}

func dialDevice(t *testing.T, ctx context.Context, url, token string, h *models.Hello) (*transport.Conn, *models.Message, error) {
	t.Helper()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+transport.Path, token)
	if err != nil {
		return nil, nil, err
	}
	h.ProtocolVersion, h.InputFormat = models.ProtocolVersion, models.DeviceInputFormat
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: h})
	f, err := conn.Read(ctx)
	if err != nil {
		conn.Close("")
		return nil, nil, err
	}
	return conn, f.Message, nil
}

func TestProfileOverridesHelloAndSettings(t *testing.T) {
	p := &optsProvider{opts: make(chan provider.SessionOptions, 4)}
	file := filepath.Join(t.TempDir(), "devices.json")
	s := New(Config{Provider: p, Language: "ru-RU", DefaultLocation: "Ашдод", DefaultTimeZone: "Asia/Jerusalem",
		ProfilesFile: file, Logger: slog.New(slog.DiscardHandler)})
	if _, err := s.SetProfile("berlin-1", Profile{Name: "Анна", Language: "de-DE", ExtraLanguages: "en-US, de-de",
		Location: "Берлин", TimeZone: "Europe/Berlin"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The profile wins over what the speaker reports.
	conn, ready, err := dialDevice(t, ctx, srv.URL, "", &models.Hello{DeviceID: "berlin-1", Language: "ru-RU", Location: "Москва"})
	if err != nil || ready.Type != models.TypeReady {
		t.Fatalf("connect: %+v %v", ready, err)
	}
	o := <-p.opts
	if o.Language != "de-DE" || strings.Join(o.ExtraLanguages, ",") != "en-US" ||
		!strings.Contains(o.SystemInstruction, "Берлин") || !strings.Contains(o.SystemInstruction, "Europe/Berlin") {
		t.Errorf("berlin options: lang=%s extra=%v prompt=%q", o.Language, o.ExtraLanguages, o.SystemInstruction)
	}
	conn.Close("")

	// A speaker without a profile keeps its hello over the global settings.
	conn, _, err = dialDevice(t, ctx, srv.URL, "", &models.Hello{DeviceID: "other", Language: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	o = <-p.opts
	if o.Language != "en-US" || !strings.Contains(o.SystemInstruction, "Ашдод") {
		t.Errorf("other options: lang=%s prompt=%q", o.Language, o.SystemInstruction)
	}
	conn.Close("")

	// Profiles survive a restart.
	s2 := New(Config{Provider: p, ProfilesFile: file, Logger: slog.New(slog.DiscardHandler)})
	if got, ok := s2.Profile("berlin-1"); !ok || got.Location != "Берлин" || got.ExtraLanguages != "en-US" {
		t.Errorf("reloaded profile = %+v %v", got, ok)
	}
}

func TestProfileTokens(t *testing.T) {
	p := &optsProvider{opts: make(chan provider.SessionOptions, 8)}
	s := New(Config{Provider: p, AuthToken: "shared-token", Logger: slog.New(slog.DiscardHandler)})
	own := NewDeviceToken()
	if _, err := s.SetProfile("sold-1", Profile{Token: own}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProfile("sold-2", Profile{Token: own}); err == nil {
		t.Error("the same token on two speakers must be rejected")
	}
	if _, err := s.SetProfile("sold-3", Profile{Token: "short"}); err == nil {
		t.Error("a short token must be rejected")
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name, token, device string
		ok                  bool
	}{
		{"own token", own, "sold-1", true},
		{"own token, other ID", own, "home", false},
		{"shared token, owner", "shared-token", "sold-1", true},
		{"shared token, no profile", "shared-token", "home", true},
		{"unknown token", "nope-nope-nope-nope", "sold-1", false},
	} {
		conn, m, err := dialDevice(t, ctx, srv.URL, tc.token, &models.Hello{DeviceID: tc.device})
		ok := err == nil && m.Type == models.TypeReady
		if ok != tc.ok {
			t.Errorf("%s: connected=%v (%+v %v), want %v", tc.name, ok, m, err, tc.ok)
		}
		if conn != nil {
			conn.Close("")
		}
	}

	// Requests without a token are refused.
	if s.authorizedDevice(httptest.NewRequest("GET", "/", nil)) {
		t.Error("no token must not be authorized")
	}
}
