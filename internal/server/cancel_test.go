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
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// slowProvider answers only the second session's turns; the first session's
// reply is released by the test after the turn was cancelled.
type slowProvider struct{ sessions chan *slowSession }

func (p *slowProvider) Name() string                     { return "slow" }
func (p *slowProvider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }
func (p *slowProvider) Connect(context.Context, provider.SessionOptions) (provider.Session, error) {
	s := &slowSession{events: make(chan provider.Event, 8)}
	p.sessions <- s
	return s, nil
}

type slowSession struct {
	events chan provider.Event
	reply  bool
}

func (s *slowSession) StartTurn() error                            { return nil }
func (s *slowSession) SendAudio([]byte) error                      { return nil }
func (s *slowSession) SendToolResults([]provider.ToolResult) error { return nil }
func (s *slowSession) Events() <-chan provider.Event               { return s.events }
func (s *slowSession) Err() error                                  { return nil }
func (s *slowSession) Close() error                                { return nil }
func (s *slowSession) EndTurn() error {
	if s.reply {
		s.events <- provider.Event{Kind: provider.EventAudio, Audio: []byte{1, 2}}
		s.events <- provider.Event{Kind: provider.EventTurnComplete}
	}
	return nil
}

// Regression: after a barge-in the completion of the abandoned turn was
// taken for the (empty) answer to the new question.
func TestCancelledTurnDoesNotCompleteNextTurn(t *testing.T) {
	p := &slowProvider{sessions: make(chan *slowSession, 2)}
	srv := httptest.NewServer(New(Config{Provider: p, Logger: slog.New(slog.DiscardHandler)}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")
	send := func(typ models.MessageType, turn string) {
		conn.Send(ctx, &models.Message{Type: typ, TurnID: turn})
	}
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "d", InputFormat: models.DeviceInputFormat}})
	conn.Read(ctx) // ready

	send(models.TypeTurnStart, "t1")
	conn.SendAudio(ctx, make([]byte, 640))
	send(models.TypeTurnEnd, "t1")
	old := <-p.sessions

	// The user barges in while the first reply is still pending.
	send(models.TypeTurnCancel, "t1")
	send(models.TypeTurnStart, "t2")
	select {
	case s := <-p.sessions:
		s.reply = true
	case <-ctx.Done():
		t.Fatal("no fresh provider session after cancel")
	}
	old.events <- provider.Event{Kind: provider.EventTurnComplete} // late, stale
	time.Sleep(50 * time.Millisecond)
	conn.SendAudio(ctx, make([]byte, 640))
	send(models.TypeTurnEnd, "t2")

	f, err := conn.Read(ctx)
	if err != nil || len(f.Audio) != 2 {
		t.Fatalf("want the reply audio first, got %+v %v", f.Message, err)
	}
	f, err = conn.Read(ctx)
	if err != nil || f.Message == nil || f.Message.Type != models.TypeTurnComplete || f.Message.TurnID != "t2" {
		t.Fatalf("want turn_complete t2, got %+v %v", f.Message, err)
	}
}

// The provider session is opened as soon as the device connects, so the
// first question does not wait for it.
func TestSessionPrewarmedOnConnect(t *testing.T) {
	p := &slowProvider{sessions: make(chan *slowSession, 2)}
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
		ProtocolVersion: models.ProtocolVersion, DeviceID: "d", InputFormat: models.DeviceInputFormat}})
	conn.Read(ctx) // ready
	select {
	case <-p.sessions:
	case <-ctx.Done():
		t.Fatal("no session opened before the first turn")
	}
	// The first turn reuses it.
	conn.Send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: "t1"})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-p.sessions:
		t.Error("first turn opened a second session")
	default:
	}
}
