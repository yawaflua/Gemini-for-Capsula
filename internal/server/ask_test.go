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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

type fakeSpeech struct{ said chan string }

func (f *fakeSpeech) Format() models.AudioFormat { return models.DefaultOutputFormat }

func (f *fakeSpeech) Synthesize(_ context.Context, text, _ string) ([]byte, error) {
	f.said <- text
	return make([]byte, models.DefaultOutputFormat.FrameSize(250*time.Millisecond)), nil
}

func (f *fakeSpeech) Transcribe(_ context.Context, pcm []byte, _ models.AudioFormat, _ string) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	return "да, продолжай", nil
}

type askEnv struct {
	t  *testing.T
	ts *httptest.Server
	sp *fakeSpeech
}

func newAskEnv(t *testing.T) *askEnv {
	sp := &fakeSpeech{said: make(chan string, 4)}
	s := New(Config{
		Provider: &textProvider{texts: make(chan string, 1)}, AuthToken: "tok", Language: "ru-RU",
		Synthesizer: sp, Recognizer: sp, Logger: slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &askEnv{t: t, ts: ts, sp: sp}
}

func (e *askEnv) dial(ctx context.Context, features ...models.Feature) *transport.Conn {
	e.t.Helper()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+transport.Path, "tok")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close("done") })
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "kitchen", InputFormat: models.DeviceInputFormat,
		Features: features,
	}})
	if f, err := conn.Read(ctx); err != nil || f.Message == nil || f.Message.Type != models.TypeReady {
		e.t.Fatalf("want ready, got %+v %v", f, err)
	}
	return conn
}

// post calls the ask API in the background.
func (e *askEnv) post(token string, req AskRequest) <-chan [2]any {
	out := make(chan [2]any, 1)
	go func() {
		b, _ := json.Marshal(req)
		r, _ := http.NewRequest("POST", e.ts.URL+"/v1/ask", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			out <- [2]any{0, AskResponse{Error: err.Error()}}
			return
		}
		defer resp.Body.Close()
		var ar AskResponse
		json.NewDecoder(resp.Body).Decode(&ar)
		out <- [2]any{resp.StatusCode, ar}
	}()
	return out
}

// next reads frames until a message of type typ, counting audio bytes.
func next(t *testing.T, ctx context.Context, conn *transport.Conn, typ models.MessageType) (*models.Message, int) {
	t.Helper()
	audio := 0
	for {
		f, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if f.Message == nil {
			audio += len(f.Audio)
			continue
		}
		if f.Message.Type == typ {
			return f.Message, audio
		}
	}
}

func TestAskAnswered(t *testing.T) {
	e := newAskEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := e.dial(ctx, models.FeatureAnnounce)

	res := e.post("tok", AskRequest{Text: "Клод спрашивает: продолжать?", Listen: true, Source: "claude"})
	ann, _ := next(t, ctx, conn, models.TypeAnnounce)
	if ann.Announce == nil || !ann.Announce.Listen || !strings.Contains(ann.Announce.Text, "продолжать") {
		t.Fatalf("announce = %+v", ann.Announce)
	}
	done, audio := next(t, ctx, conn, models.TypeTurnComplete)
	if done.TurnID != ann.TurnID || audio == 0 {
		t.Fatalf("announcement %s: complete for %s with %d audio bytes", ann.TurnID, done.TurnID, audio)
	}

	// The device records the answer.
	conn.Send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: "r1", ReplyTo: ann.TurnID})
	conn.SendAudio(ctx, make([]byte, 3200))
	conn.Send(ctx, &models.Message{Type: models.TypeTurnEnd, TurnID: "r1"})
	if m, _ := next(t, ctx, conn, models.TypeTurnComplete); m.TurnID != "r1" {
		t.Errorf("answer turn completed as %s", m.TurnID)
	}
	r := <-res
	if ar := r[1].(AskResponse); r[0] != 200 || !ar.Answered || ar.Answer != "да, продолжай" || ar.Device != "kitchen" {
		t.Errorf("ask = %v %+v", r[0], ar)
	}
}

func TestAskBusyThenNoAnswer(t *testing.T) {
	e := newAskEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := e.dial(ctx, models.FeatureAnnounce)

	res := e.post("tok", AskRequest{Text: "Вопрос", Listen: true})
	first, _ := next(t, ctx, conn, models.TypeAnnounce)
	conn.Send(ctx, &models.Message{Type: models.TypeTurnCancel, TurnID: first.TurnID}) // busy

	second, _ := next(t, ctx, conn, models.TypeAnnounce) // retried
	next(t, ctx, conn, models.TypeTurnComplete)
	conn.Send(ctx, &models.Message{Type: models.TypeTurnCancel, ReplyTo: second.TurnID}) // silence
	r := <-res
	if ar := r[1].(AskResponse); r[0] != 200 || ar.Answered {
		t.Errorf("ask = %v %+v, want unanswered", r[0], ar)
	}

	// A late answer to the expired question is not taken for a request.
	conn.Send(ctx, &models.Message{Type: models.TypeTurnStart, TurnID: "late", ReplyTo: second.TurnID})
	if m, _ := next(t, ctx, conn, models.TypeTurnComplete); m.TurnID != "late" {
		t.Errorf("late answer completed as %s", m.TurnID)
	}
}

func TestAskRejected(t *testing.T) {
	e := newAskEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if r := <-e.post("tok", AskRequest{Text: "x"}); r[0] != http.StatusServiceUnavailable {
		t.Errorf("no speaker = %v", r[0])
	}
	e.dial(ctx) // an old agent without announcements
	for range 100 {
		if r := <-e.post("bad", AskRequest{Text: "x"}); r[0] != http.StatusUnauthorized {
			t.Fatalf("wrong token = %v", r[0])
		}
		r := <-e.post("tok", AskRequest{Text: "x"})
		if r[0] == http.StatusServiceUnavailable {
			time.Sleep(10 * time.Millisecond) // not registered yet
			continue
		}
		if r[0] != http.StatusNotImplemented {
			t.Errorf("old agent = %v %+v", r[0], r[1])
		}
		return
	}
	t.Fatal("device did not register")
}

var _ provider.Provider = (*textProvider)(nil)
