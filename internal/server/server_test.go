// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// toolProvider requests a volume change at the end of every turn and then
// completes the turn once the result arrives.
type toolProvider struct {
	results  chan []provider.ToolResult
	call     string // function to call; volume_set by default
	connects atomic.Int32
}

func (p *toolProvider) Name() string                     { return "fake" }
func (p *toolProvider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }
func (p *toolProvider) Connect(context.Context, provider.SessionOptions) (provider.Session, error) {
	p.connects.Add(1)
	return &toolSession{p: p, events: make(chan provider.Event, 8)}, nil
}

type toolSession struct {
	p      *toolProvider
	events chan provider.Event
}

func (s *toolSession) StartTurn() error              { return nil }
func (s *toolSession) SendAudio([]byte) error        { return nil }
func (s *toolSession) Events() <-chan provider.Event { return s.events }
func (s *toolSession) Err() error                    { return nil }
func (s *toolSession) Close() error                  { return nil }
func (s *toolSession) EndTurn() error {
	call := provider.ToolCall{ID: "call-1", Name: "volume_set", Args: map[string]any{"level": 30.0}}
	if s.p.call != "" {
		call = provider.ToolCall{ID: "call-1", Name: s.p.call}
	}
	s.events <- provider.Event{Kind: provider.EventToolCall, ToolCalls: []provider.ToolCall{call}}
	return nil
}
func (s *toolSession) SendToolResults(r []provider.ToolResult) error {
	s.p.results <- r
	s.events <- provider.Event{Kind: provider.EventAudio, Audio: []byte{1, 2, 3, 4}}
	s.events <- provider.Event{Kind: provider.EventTurnComplete}
	return nil
}

func TestToolCallRoundTrip(t *testing.T) {
	p := &toolProvider{results: make(chan []provider.ToolResult, 1)}
	srv := httptest.NewServer(New(Config{Provider: p, Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")

	send := func(m *models.Message) {
		if err := conn.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	read := func() transport.Frame {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	send(&models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "d", InputFormat: models.DeviceInputFormat,
	}})
	if f := read(); f.Message == nil || f.Message.Type != models.TypeReady {
		t.Fatalf("want ready, got %+v", f)
	}
	send(&models.Message{Type: models.TypeTurnStart, TurnID: "t1"})
	conn.SendAudio(ctx, make([]byte, 640))
	send(&models.Message{Type: models.TypeTurnEnd, TurnID: "t1"})

	f := read()
	if f.Message == nil || f.Message.Type != models.TypeCommand || f.Message.Command.Action != models.ActionVolumeSet {
		t.Fatalf("want volume.set command, got %+v", f.Message)
	}
	cmd := f.Message.Command
	if lvl, _ := cmd.Int("level"); lvl != 30 {
		t.Errorf("level = %d, want 30", lvl)
	}
	send(&models.Message{Type: models.TypeCommandResult, CommandResult: cmd.Success(map[string]any{"level": 30})})

	select {
	case r := <-p.results:
		if len(r) != 1 || r[0].ID != "call-1" || r[0].Err != "" {
			t.Errorf("tool results = %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no tool results")
	}
	if f := read(); len(f.Audio) != 4 {
		t.Errorf("want audio frame, got %+v", f)
	}
	if f := read(); f.Message == nil || f.Message.Type != models.TypeTurnComplete || f.Message.TurnID != "t1" {
		t.Errorf("want turn_complete for t1, got %+v", f.Message)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(New(Config{Provider: &toolProvider{}, AuthToken: "x", Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()
	_, err := transport.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+transport.Path, "wrong")
	if err == nil {
		t.Fatal("dial with a wrong token succeeded")
	}
}

func TestAgentDownload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marusya-agent-linux-arm64"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Provider: &toolProvider{}, AuthToken: "x", AgentDir: dir, Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()

	get := func(path, token string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/v1/agent/arm64", "x"); code != 200 || body != "ELF" {
		t.Errorf("download = %d %q, want 200 ELF", code, body)
	}
	if code, _ := get("/v1/agent/arm64", "wrong"); code != 401 {
		t.Errorf("wrong token = %d, want 401", code)
	}
	if code, _ := get("/v1/agent/riscv64", "x"); code != 404 {
		t.Errorf("missing arch = %d, want 404", code)
	}
}

// TestConversationEndDropsContext checks that a conversation ended by the
// user starts afresh: the model session is replaced after the goodbye.
func TestConversationEndDropsContext(t *testing.T) {
	p := &toolProvider{results: make(chan []provider.ToolResult, 1), call: "conversation_end"}
	srv := httptest.NewServer(New(Config{Provider: p, Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "d", InputFormat: models.DeviceInputFormat,
		Capabilities: []models.Action{models.ActionConversationEnd},
	}})
	conn.Read(ctx) // ready
	conn.Send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: "t1"})
	conn.Send(ctx, &models.Message{Type: models.TypeTurnEnd, TurnID: "t1"})
	for {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Message == nil {
			continue
		}
		if f.Message.Type == models.TypeCommand {
			if f.Message.Command.Action != models.ActionConversationEnd {
				t.Fatalf("command = %s", f.Message.Command.Action)
			}
			conn.Send(ctx, &models.Message{Type: models.TypeCommandResult, CommandResult: f.Message.Command.Success(nil)})
		}
		if f.Message.Type == models.TypeTurnComplete {
			break
		}
	}
	// One session for the conversation, a fresh one opened after it ended.
	deadline := time.Now().Add(2 * time.Second)
	for p.connects.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.connects.Load(); n != 2 {
		t.Errorf("%d provider sessions, want 2", n)
	}
}
