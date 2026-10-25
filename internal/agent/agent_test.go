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

package agent_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider/echo"
	"github.com/yawaflua/marusya-gemini/internal/server"
	"github.com/yawaflua/marusya-gemini/internal/transport"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// fakeMic emits silence, or a tone while speechFrames > 0.
type fakeMic struct{ speechFrames atomic.Int32 }

func (m *fakeMic) Format() models.AudioFormat { return models.DeviceInputFormat }

func (m *fakeMic) Run(ctx context.Context, out chan<- []byte) error {
	f := m.Format()
	silence := make([]byte, f.FrameSize(20*time.Millisecond))
	tone := audio.Tone(f, 300, 20, 0.3)
	t := time.NewTicker(2 * time.Millisecond) // 10x real time
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			frame := silence
			if m.speechFrames.Load() > 0 {
				m.speechFrames.Add(-1)
				frame = tone
			}
			select {
			case out <- frame:
			default:
			}
		}
	}
}

type fakePlayer struct {
	mu     sync.Mutex
	played int
	done   chan struct{}
	once   sync.Once
}

func (p *fakePlayer) Open(context.Context, models.AudioFormat) (audio.Stream, error) {
	return &fakeStream{p: p}, nil
}

type fakeStream struct{ p *fakePlayer }

func (s *fakeStream) Write(pcm []byte) error {
	s.p.mu.Lock()
	s.p.played += len(pcm)
	s.p.mu.Unlock()
	return nil
}
func (s *fakeStream) Drain() error { s.p.once.Do(func() { close(s.p.done) }); return nil }
func (s *fakeStream) Abort() error { return nil }

type fakeWake chan wakeword.Detection

func (fakeWake) WantsAudio() bool { return false }
func (w fakeWake) Run(ctx context.Context, _ <-chan []byte, out chan<- wakeword.Detection) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case d := <-w:
			out <- d
		}
	}
}

func TestAgentEndToEndWithEchoProvider(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(server.New(server.Config{Provider: echo.Provider{}, AuthToken: "secret", Logger: log}))
	defer srv.Close()

	mic := &fakeMic{}
	player := &fakePlayer{done: make(chan struct{})}
	wake := make(fakeWake, 1)
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
		Token:     "secret",
	}, log, agent.Deps{Capture: mic, Player: player, Detector: wake, Dispatcher: &controller.Dispatcher{}, Indicator: ind})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	time.Sleep(300 * time.Millisecond) // connect and learn the noise floor
	wake <- wakeword.Detection{Source: "test"}
	time.Sleep(20 * time.Millisecond)
	mic.speechFrames.Store(25) // 500 ms of "speech"

	select {
	case <-player.done:
	case <-ctx.Done():
		t.Fatal("no reply was played")
	}
	player.mu.Lock()
	defer player.mu.Unlock()
	time.Sleep(50 * time.Millisecond) // let the agent return to idle
	want := []indicator.State{indicator.Idle, indicator.Listening, indicator.Thinking, indicator.Speaking, indicator.Idle}
	if got := ind.get(); !slices.Equal(got, want) {
		t.Errorf("indicator states = %v, want %v", got, want)
	}
	if min := models.DeviceInputFormat.FrameSize(500 * time.Millisecond); player.played < min {
		t.Errorf("played %d bytes, want at least %d", player.played, min)
	}
}

type fakeIndicator struct {
	mu     sync.Mutex
	states []indicator.State
}

func (f *fakeIndicator) Show(s indicator.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.states); n == 0 || f.states[n-1] != s {
		f.states = append(f.states, s)
	}
}

func (f *fakeIndicator) get() []indicator.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.states)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

// TestAgentRemoteTurn checks that a reply to a question typed in the web
// panel is played although the speaker recorded nothing, and that the agent
// does not start listening for a follow-up afterwards.
func TestAgentRemoteTurn(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	features := make(chan []models.Feature, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		f, err := conn.Read(ctx)
		if err != nil || f.Message == nil || f.Message.Type != models.TypeHello {
			return
		}
		features <- f.Message.Hello.Features
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		time.Sleep(200 * time.Millisecond)
		conn.Send(ctx, &models.Message{Type: models.TypeRemoteTurn, TurnID: "panel-1"})
		conn.SendAudio(ctx, make([]byte, 4800))
		conn.Send(ctx, &models.Message{Type: models.TypeTurnComplete, TurnID: "panel-1"})
		<-r.Context().Done()
	}))
	defer srv.Close()

	player := &fakePlayer{done: make(chan struct{})}
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
		FollowUp:  true,
	}, log, agent.Deps{Capture: &fakeMic{}, Player: player, Detector: make(fakeWake), Dispatcher: &controller.Dispatcher{}, Indicator: ind})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	select {
	case fs := <-features:
		if !slices.Contains(fs, models.FeatureRemoteTurn) {
			t.Errorf("hello features = %v, want remote_turn", fs)
		}
	case <-ctx.Done():
		t.Fatal("no hello")
	}
	select {
	case <-player.done:
	case <-ctx.Done():
		t.Fatal("the remote reply was not played")
	}
	time.Sleep(100 * time.Millisecond)
	want := []indicator.State{indicator.Idle, indicator.Thinking, indicator.Speaking, indicator.Idle}
	if got := ind.get(); !slices.Equal(got, want) {
		t.Errorf("indicator states = %v, want %v", got, want)
	}
}

// fakeTurnServer accepts one device and answers its first spoken turn with
// conversation.end, a short reply and turn_complete.
func fakeTurnServer(t *testing.T, ctx context.Context, caps chan<- []models.Action) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		f, err := conn.Read(ctx)
		if err != nil || f.Message == nil || f.Message.Type != models.TypeHello {
			return
		}
		caps <- f.Message.Hello.Capabilities
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		var turn string
		for {
			f, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if f.Message != nil && f.Message.Type == models.TypeTurnEnd {
				turn = f.Message.TurnID
				break
			}
		}
		cmd := &models.Command{ID: "c1", Action: models.ActionConversationEnd}
		conn.Send(ctx, &models.Message{Type: models.TypeCommand, TurnID: turn, Command: cmd})
		for {
			f, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if f.Message != nil && f.Message.Type == models.TypeCommandResult {
				if !f.Message.CommandResult.OK {
					t.Errorf("conversation.end failed: %s", f.Message.CommandResult.Error)
				}
				break
			}
		}
		conn.SendAudio(ctx, make([]byte, 4800))
		conn.Send(ctx, &models.Message{Type: models.TypeTurnComplete, TurnID: turn})
		<-r.Context().Done()
	}))
}

// TestAgentConversationEnd checks that after "пока" the agent goes idle
// instead of listening for a follow-up.
func TestAgentConversationEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caps := make(chan []models.Action, 1)
	srv := fakeTurnServer(t, ctx, caps)
	defer srv.Close()

	mic := &fakeMic{}
	player := &fakePlayer{done: make(chan struct{})}
	wake := make(fakeWake, 1)
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
		FollowUp:  true,
	}, log, agent.Deps{Capture: mic, Player: player, Detector: wake, Dispatcher: &controller.Dispatcher{}, Indicator: ind})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	if c := <-caps; !slices.Contains(c, models.ActionConversationEnd) {
		t.Errorf("capabilities = %v, want conversation.end", c)
	}
	time.Sleep(300 * time.Millisecond) // learn the noise floor
	wake <- wakeword.Detection{Source: "test"}
	time.Sleep(20 * time.Millisecond)
	mic.speechFrames.Store(25)

	select {
	case <-player.done:
	case <-ctx.Done():
		t.Fatal("no reply was played")
	}
	time.Sleep(100 * time.Millisecond)
	want := []indicator.State{indicator.Idle, indicator.Listening, indicator.Thinking, indicator.Speaking, indicator.Idle}
	if got := ind.get(); !slices.Equal(got, want) {
		t.Errorf("indicator states = %v, want %v (no follow-up)", got, want)
	}
}

// TestAgentCancelListeningByButton checks that pressing the button again
// while listening abandons the request.
func TestAgentCancelListeningByButton(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(server.New(server.Config{Provider: echo.Provider{}, Logger: log}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mic := &fakeMic{}
	wake := make(fakeWake, 1)
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
	}, log, agent.Deps{Capture: mic, Player: &fakePlayer{done: make(chan struct{})}, Detector: wake, Dispatcher: &controller.Dispatcher{}, Indicator: ind})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	time.Sleep(300 * time.Millisecond)
	wake <- wakeword.Detection{Source: "exec"}
	time.Sleep(50 * time.Millisecond)
	wake <- wakeword.Detection{Source: "exec"} // the spotter hearing itself: ignored
	time.Sleep(50 * time.Millisecond)
	if got := ind.get(); got[len(got)-1] != indicator.Listening {
		t.Fatalf("a repeated keyword stopped listening: %v", got)
	}
	wake <- wakeword.Detection{Source: "touch"}
	time.Sleep(50 * time.Millisecond)
	want := []indicator.State{indicator.Idle, indicator.Listening, indicator.Idle}
	if got := ind.get(); !slices.Equal(got, want) {
		t.Errorf("indicator states = %v, want %v", got, want)
	}
}

// TestAgentServerWake checks that the agent streams idle audio when asked
// and starts listening when the server hears the wake word.
func TestAgentServerWake(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gotAudio := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		f, err := conn.Read(ctx)
		if err != nil || f.Message == nil || !slices.Contains(f.Message.Hello.Features, models.FeatureServerWake) {
			t.Errorf("hello without server_wake: %+v %v", f.Message, err)
			return
		}
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		conn.Send(ctx, &models.Message{Type: models.TypeWakeStream, WakeStream: &models.WakeStream{On: true}})
		for {
			f, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if f.Message == nil {
				close(gotAudio)
				break
			}
		}
		conn.Send(ctx, &models.Message{Type: models.TypeWake, Wake: &models.Wake{Keyword: "антон"}})
		<-r.Context().Done()
	}))
	defer srv.Close()

	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
	}, log, agent.Deps{Capture: &fakeMic{}, Player: &fakePlayer{done: make(chan struct{})}, Detector: make(fakeWake), Dispatcher: &controller.Dispatcher{}, Indicator: ind})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	select {
	case <-gotAudio:
	case <-ctx.Done():
		t.Fatal("no idle audio was streamed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := ind.get(); got[len(got)-1] == indicator.Listening {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("indicator states = %v, want listening after the wake", ind.get())
}

func TestAgentAnnounceAndAnswer(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mic := &fakeMic{}
	answer := make(chan *models.Message, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		if f, err := conn.Read(ctx); err != nil || f.Message == nil || !slices.Contains(f.Message.Hello.Features, models.FeatureAnnounce) {
			t.Error("hello without the announce feature")
			return
		}
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		time.Sleep(200 * time.Millisecond)
		conn.Send(ctx, &models.Message{Type: models.TypeAnnounce, TurnID: "ask-1", Announce: &models.Announce{Text: "продолжать?", Listen: true}})
		conn.SendAudio(ctx, make([]byte, 4800))
		conn.Send(ctx, &models.Message{Type: models.TypeTurnComplete, TurnID: "ask-1"})
		for {
			f, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if m := f.Message; m != nil && (m.Type == models.TypeTurnStart || m.Type == models.TypeTurnEnd) {
				answer <- m
				if m.Type == models.TypeTurnEnd {
					conn.Send(ctx, &models.Message{Type: models.TypeTurnComplete, TurnID: m.TurnID})
				}
			}
		}
	}))
	defer srv.Close()

	player := &fakePlayer{done: make(chan struct{})}
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
		FollowUp:  true,
	}, log, agent.Deps{Capture: mic, Player: player, Detector: make(fakeWake), Dispatcher: &controller.Dispatcher{}, Indicator: ind})
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	select {
	case <-player.done:
	case <-ctx.Done():
		t.Fatal("the announcement was not played")
	}
	time.Sleep(50 * time.Millisecond)
	mic.speechFrames.Store(40) // the user answers
	for _, typ := range []models.MessageType{models.TypeTurnStart, models.TypeTurnEnd} {
		select {
		case m := <-answer:
			if m.Type != typ || (typ == models.TypeTurnStart && m.ReplyTo != "ask-1") {
				t.Fatalf("got %s reply_to=%q, want %s replying to ask-1", m.Type, m.ReplyTo, typ)
			}
		case <-ctx.Done():
			t.Fatalf("no %s", typ)
		}
	}
	time.Sleep(200 * time.Millisecond)
	want := []indicator.State{indicator.Idle, indicator.Announcing, indicator.Listening, indicator.Thinking, indicator.Idle}
	if got := ind.get(); !slices.Equal(got, want) {
		t.Errorf("indicator states = %v, want %v", got, want)
	}
}
