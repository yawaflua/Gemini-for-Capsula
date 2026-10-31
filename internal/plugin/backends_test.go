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

package plugin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// capsPlugin implements a model provider, a speech engine, HTTP and
// instructions with the Go SDK.
func capsPlugin() int {
	p := pluginsdk.New()
	p.Instructions(context.Background(), "Будь кратка.")
	var mu sync.Mutex
	history := map[string][]string{}
	closed := map[string]bool{}
	p.Provider(pluginsdk.ProviderSpec{Name: "echo", Label: "Эхо"}, pluginsdk.ProviderHandler{
		Turn: func(ctx context.Context, t *pluginsdk.Turn) (pluginsdk.ProviderTurnResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if t.Text != "" {
				history[t.Session] = append(history[t.Session], t.Text)
			}
			if len(t.ToolResults) > 0 {
				r := t.ToolResults[0]
				return pluginsdk.ProviderTurnResult{Text: fmt.Sprintf("Громкость %v.", r.Output["level"])}, nil
			}
			if t.Text == "громкость" {
				return pluginsdk.ProviderTurnResult{ToolCalls: []pluginsdk.ProviderToolCall{{ID: "1", Name: "volume_get"}}}, nil
			}
			t.Write("Ты сказал: ")
			t.Write(t.Text + ".")
			return pluginsdk.ProviderTurnResult{Text: fmt.Sprintf(" Реплик: %d, функций: %d, инструкция: %s", len(history[t.Session]), len(t.Tools), t.System)}, nil
		},
		Close: func(session string) {
			mu.Lock()
			closed[session] = true
			mu.Unlock()
		},
	})
	p.Speech(pluginsdk.SpeechSpec{Name: "beep", SampleRate: 16000, Voices: []string{"a", "b"}}, pluginsdk.SpeechHandler{
		Synthesize: func(_ context.Context, s pluginsdk.SynthesizeParams) ([]byte, error) {
			return make([]byte, 2*len([]rune(s.Text))), nil
		},
		Transcribe: func(_ context.Context, t pluginsdk.TranscribeParams) (string, error) {
			return fmt.Sprintf("слышу %d", len(t.Audio)), nil
		},
	})
	p.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mood" {
			p.Instructions(r.Context(), "Отвечай как пират.")
		}
		mu.Lock()
		n := len(closed)
		mu.Unlock()
		fmt.Fprintf(w, "%s %s q=%s closed=%d", r.Method, r.URL.Path, r.URL.Query().Get("q"), n)
	}), true)
	if p.Run() != nil {
		return 1
	}
	return 0
}

func startCaps(t *testing.T) *Manager {
	t.Helper()
	exe, _ := os.Executable()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "caps"), 0o755)
	os.WriteFile(filepath.Join(root, "caps", ManifestFile), []byte(`{"id":"caps","server":{"command":`+
		mustJSON([]string{exe})+`,"env":{"MARUSYA_TEST_PLUGIN":"caps"}}}`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(Config{Dir: root, Handler: &fakeHandler{got: make(chan struct{}, 8)}})
	m.Start(ctx)
	return m
}

func TestPluginProviderAndSpeech(t *testing.T) {
	m := startCaps(t)
	if got := m.Instructions(); got != "Будь кратка." {
		t.Fatalf("Instructions = %q", got)
	}
	if ps := m.Providers(); len(ps) != 1 || ps[0].ID != "caps/echo" || ps[0].Label != "Эхо" {
		t.Fatalf("Providers = %+v", ps)
	}
	if ss := m.SpeechEngines(); len(ss) != 1 || ss[0].ID != "caps/beep" || !ss[0].Synthesize || !ss[0].Transcribe {
		t.Fatalf("SpeechEngines = %+v", ss)
	}

	b := &Backends{}
	b.Set(m)
	sp, err := b.Speech("caps/beep", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := sp.Format(); f.SampleRateHertz != 16000 {
		t.Errorf("Format = %+v", f)
	}
	if v := sp.Voices(); len(v) != 2 {
		t.Errorf("Voices = %v", v)
	}
	p, err := b.Provider("caps/echo", sp, sp)
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := tools.Lookup("volume_get")
	s, err := p.Connect(context.Background(), provider.SessionOptions{
		SystemInstruction: "СИСТЕМА", Language: "ru-RU", InputFormat: models.DeviceInputFormat, Tools: []tools.Tool{vol},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A spoken turn: recognized by the plugin, answered with streamed text.
	s.StartTurn()
	s.SendAudio(make([]byte, 320))
	s.EndTurn()
	heard, said := collect(t, s, nil)
	if heard != "слышу 320" {
		t.Errorf("heard %q", heard)
	}
	if !strings.Contains(said, "Ты сказал: слышу 320.") || !strings.Contains(said, "Реплик: 1, функций: 1, инструкция: СИСТЕМА") {
		t.Errorf("said %q", said)
	}

	// A typed turn with a function call.
	s.(provider.TextSender).SendText("громкость")
	_, said = collect(t, s, func(c provider.ToolCall) map[string]any {
		if c.Name != "volume_get" {
			t.Errorf("call %+v", c)
		}
		return map[string]any{"level": 42}
	})
	if !strings.Contains(said, "Громкость 42.") {
		t.Errorf("said %q", said)
	}
	s.Close()

	// The plugin heard about the closed conversation.
	for i := 0; ; i++ {
		res, err := m.HTTP(context.Background(), "caps", pluginsdk.HTTPRequest{Method: "GET", Path: "/x", Query: "q=1"})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Body) == "GET /x q=1 closed=1" {
			break
		}
		if i > 100 {
			t.Fatalf("HTTP = %d %q", res.Status, res.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Instructions change at runtime.
	if _, err := m.HTTP(context.Background(), "caps", pluginsdk.HTTPRequest{Method: "POST", Path: "/mood"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Instructions(); got != "Отвечай как пират." {
		t.Errorf("Instructions after change = %q", got)
	}
	if st := m.List(); st[0].HTTP != "public" || len(st[0].Providers) != 1 || st[0].Instructions == "" {
		t.Errorf("status = %+v", st[0])
	}
}

// collect reads the events of one turn, answering function calls with
// answer, and returns what was heard and said.
func collect(t *testing.T, s provider.Session, answer func(provider.ToolCall) map[string]any) (heard, said string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	audio := 0
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatal("session closed")
			}
			switch e.Kind {
			case provider.EventInputTranscript:
				heard += e.Text
			case provider.EventOutputTranscript:
				said += e.Text
			case provider.EventAudio:
				audio += len(e.Audio)
			case provider.EventToolCall:
				var rs []provider.ToolResult
				for _, c := range e.ToolCalls {
					rs = append(rs, provider.ToolResult{ID: c.ID, Name: c.Name, Output: answer(c)})
				}
				s.SendToolResults(rs)
			case provider.EventTurnComplete:
				if audio == 0 && said != "" {
					t.Error("no audio")
				}
				return heard, said
			}
		case <-timeout:
			t.Fatalf("turn did not complete; heard %q, said %q", heard, said)
		}
	}
}

func TestBackendsWithoutPlugin(t *testing.T) {
	b := &Backends{}
	p, err := b.Provider("nope/x", fakeSpeech{}, fakeSpeech{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Connect(context.Background(), provider.SessionOptions{InputFormat: models.DeviceInputFormat}); err == nil {
		t.Error("Connect without a manager succeeded")
	}
	sp, _ := b.Speech("nope/x", fakeSpeech{}, fakeSpeech{})
	if pcm, err := sp.Synthesize(context.Background(), "a", "ru"); err != nil || len(pcm) != 4 {
		t.Errorf("fallback Synthesize = %d %v", len(pcm), err)
	}
	if _, err := b.Provider("bad", fakeSpeech{}, fakeSpeech{}); err == nil {
		t.Error("an id without a slash was accepted")
	}
}

type fakeSpeech struct{}

func (fakeSpeech) Format() models.AudioFormat { return models.DefaultOutputFormat }
func (fakeSpeech) Synthesize(context.Context, string, string) ([]byte, error) {
	return make([]byte, 4), nil
}
func (fakeSpeech) Transcribe(context.Context, []byte, models.AudioFormat, string) (string, error) {
	return "", nil
}
