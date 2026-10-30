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

package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

type fakeSpeech struct{}

func (fakeSpeech) Transcribe(_ context.Context, pcm []byte, _ models.AudioFormat, _ string) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	return "сделай потише", nil
}
func (fakeSpeech) Format() models.AudioFormat { return models.DefaultOutputFormat }
func (fakeSpeech) Synthesize(_ context.Context, text, _ string) ([]byte, error) {
	return []byte(text), nil
}

func sse(w io.Writer, events ...string) {
	for _, e := range events {
		var typ struct{ Type string }
		json.Unmarshal([]byte(e), &typ)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, e)
	}
}

const msgStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`

// fakeAPI answers the first request with a volume_set call and the second
// with spoken text, recording request bodies.
func fakeAPI(t *testing.T) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			sse(w, msgStart,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"volume_set","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"level\": 30}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`)
			return
		}
		sse(w, msgStart,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Готово, поставила громкость тридцать. "}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Ещё что-нибудь?"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

func TestTurnWithToolCall(t *testing.T) {
	srv, bodies := fakeAPI(t)
	p, err := New(Config{APIKey: "test", BaseURL: srv.URL, Effort: "low", WebSearch: true,
		Recognizer: fakeSpeech{}, Synthesizer: fakeSpeech{}})
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := tools.Lookup("volume_set")
	s, err := p.Connect(context.Background(), provider.SessionOptions{
		SystemInstruction: "ты колонка", Language: "ru-RU",
		InputFormat: models.DeviceInputFormat, Tools: []tools.Tool{vol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.StartTurn()
	s.SendAudio(make([]byte, 640))
	s.EndTurn()

	var heard, said string
	var audio int
	timeout := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case e := <-s.Events():
			switch e.Kind {
			case provider.EventInputTranscript:
				heard = e.Text
			case provider.EventToolCall:
				c := e.ToolCalls[0]
				if c.Name != "volume_set" || c.Args["level"] != 30.0 {
					t.Errorf("tool call = %+v", c)
				}
				s.SendToolResults([]provider.ToolResult{{ID: c.ID, Name: c.Name, Output: map[string]any{"level": 30}}})
			case provider.EventOutputTranscript:
				said += e.Text
			case provider.EventAudio:
				audio += len(e.Audio)
			case provider.EventTurnComplete:
				done = true
			}
		case <-timeout:
			t.Fatal("turn did not complete")
		}
	}
	if heard != "сделай потише" {
		t.Errorf("heard %q", heard)
	}
	if want := "Готово, поставила громкость тридцать. Ещё что-нибудь? "; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	if audio == 0 {
		t.Error("no audio")
	}

	b := bodies()
	if len(b) != 2 {
		t.Fatalf("%d requests, want 2", len(b))
	}
	if b[0]["model"] != "claude-haiku-5-5" || b[0]["output_config"].(map[string]any)["effort"] != "low" {
		t.Errorf("request = %v", b[0])
	}
	toolsJSON, _ := json.Marshal(b[0]["tools"])
	if !strings.Contains(string(toolsJSON), "web_search_20250305") || !strings.Contains(string(toolsJSON), "volume_set") {
		t.Errorf("tools = %s", toolsJSON)
	}
	// The second request carries the tool result after the tool call.
	msgs := b[1]["messages"].([]any)
	last, _ := json.Marshal(msgs[len(msgs)-1])
	if len(msgs) != 3 || !strings.Contains(string(last), `"tool_use_id":"toolu_1"`) {
		t.Errorf("second request messages = %s", last)
	}
}

func TestNoSpeechCompletesTurn(t *testing.T) {
	p, _ := New(Config{APIKey: "test", BaseURL: "http://127.0.0.1:1", Recognizer: fakeSpeech{}, Synthesizer: fakeSpeech{}})
	s, _ := p.Connect(context.Background(), provider.SessionOptions{InputFormat: models.DeviceInputFormat})
	defer s.Close()
	s.StartTurn()
	s.EndTurn() // no audio -> empty transcript
	select {
	case e := <-s.Events():
		if e.Kind != provider.EventTurnComplete {
			t.Errorf("got %v, want turn_complete", e.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not complete")
	}
}
