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
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

type fakeSkill struct{ dev chan skills.Device }

func (fakeSkill) Tool() tools.Tool { return tools.Tool{Action: "time.now", Description: "time"} }
func (s fakeSkill) Run(_ context.Context, dev skills.Device, _ map[string]any) (map[string]any, error) {
	s.dev <- dev
	return map[string]any{"time": "21:05"}, nil
}

// vadProvider detects the end of speech on the first audio chunk and calls
// time_now, like Gemini with automatic activity detection.
type vadProvider struct {
	opts    chan provider.SessionOptions
	results chan []provider.ToolResult
}

func (p *vadProvider) Name() string                     { return "fake-vad" }
func (p *vadProvider) SupportsServerVAD() bool          { return true }
func (p *vadProvider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }
func (p *vadProvider) Connect(_ context.Context, o provider.SessionOptions) (provider.Session, error) {
	p.opts <- o
	return &vadSession{p: p, events: make(chan provider.Event, 8)}, nil
}

type vadSession struct {
	p      *vadProvider
	events chan provider.Event
	sent   bool
}

func (s *vadSession) StartTurn() error              { return nil }
func (s *vadSession) EndTurn() error                { return nil }
func (s *vadSession) Events() <-chan provider.Event { return s.events }
func (s *vadSession) Err() error                    { return nil }
func (s *vadSession) Close() error                  { return nil }
func (s *vadSession) SendAudio([]byte) error {
	if !s.sent {
		s.sent = true
		s.events <- provider.Event{Kind: provider.EventSpeechEnd}
		s.events <- provider.Event{Kind: provider.EventToolCall, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "time_now"}}}
	}
	return nil
}
func (s *vadSession) SendToolResults(r []provider.ToolResult) error {
	s.p.results <- r
	s.events <- provider.Event{Kind: provider.EventTurnComplete}
	return nil
}

func TestServerVADAndSkills(t *testing.T) {
	p := &vadProvider{opts: make(chan provider.SessionOptions, 1), results: make(chan []provider.ToolResult, 1)}
	sk := fakeSkill{dev: make(chan skills.Device, 1)}
	srv := httptest.NewServer(New(Config{
		Provider:        p,
		ServerVAD:       true,
		Skills:          skills.Set{sk},
		DefaultLocation: "Ашдод",
		DefaultTimeZone: "Asia/Jerusalem",
		Logger:          slog.New(slog.DiscardHandler),
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")

	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "test",
		InputFormat: models.DeviceInputFormat, Language: "ru-RU",
	}})
	f, err := conn.Read(ctx)
	if err != nil || f.Message.Type != models.TypeReady || !f.Message.Ready.ServerVAD {
		t.Fatalf("ready: %+v %v", f.Message, err)
	}
	conn.Send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: "t1"})
	conn.SendAudio(ctx, make([]byte, 640))

	if f, err := conn.Read(ctx); err != nil || f.Message.Type != models.TypeListenEnd || f.Message.TurnID != "t1" {
		t.Fatalf("want listen_end, got %+v %v", f.Message, err)
	}
	o := <-p.opts
	if !o.ServerVAD || !strings.Contains(o.SystemInstruction, "Ашдод") {
		t.Errorf("session options: vad=%v prompt=%q", o.ServerVAD, o.SystemInstruction)
	}
	if !hasTool(o.Tools, "time_now") {
		t.Errorf("time_now not declared: %v", o.Tools)
	}
	if dev := <-sk.dev; dev.Location != "Ашдод" || dev.TimeZone != "Asia/Jerusalem" {
		t.Errorf("skill device = %+v", dev)
	}
	r := <-p.results
	if len(r) != 1 || r[0].Err != "" || r[0].Output["time"] != "21:05" {
		t.Errorf("results = %+v", r)
	}
	if f, err := conn.Read(ctx); err != nil || f.Message.Type != models.TypeTurnComplete {
		t.Fatalf("want turn_complete, got %+v %v", f.Message, err)
	}
}

func hasTool(ts []tools.Tool, name string) bool {
	for _, t := range ts {
		if t.Name() == name {
			return true
		}
	}
	return false
}
