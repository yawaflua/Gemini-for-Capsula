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

package openaichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// fakeAPI answers the first request with a streamed volume_set call split
// over two chunks and the second with text.
func fakeAPI(t *testing.T) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"error":{"message":"bad"}}`, http.StatusUnauthorized)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		n := len(reqs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
		if n == 1 {
			send(`{"choices":[{"delta":{"content":"Сейчас."}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"volume_set","arguments":"{\"lev"}}]}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"el\":30}"}}]}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			send(`{"choices":[{"delta":{"content":"Готово, громкость тридцать."}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		}
		send("[DONE]")
	}))
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return reqs
	}
}

func TestTurnWithToolCall(t *testing.T) {
	srv, requests := fakeAPI(t)
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Recognizer: fakeSpeech{}, Synthesizer: fakeSpeech{}})
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := tools.Lookup("volume_set")
	s, err := p.Connect(context.Background(), provider.SessionOptions{
		SystemInstruction: "Ты Маруся.",
		InputFormat:       models.DeviceInputFormat,
		Tools:             []tools.Tool{vol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.StartTurn()
	s.SendAudio(make([]byte, 320))
	s.EndTurn()

	var spoken []string
	timeout := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case e := <-s.Events():
			switch e.Kind {
			case provider.EventToolCall:
				c := e.ToolCalls[0]
				if c.ID != "call_1" || c.Name != "volume_set" || c.Args["level"] != float64(30) {
					t.Fatalf("tool call = %+v", c)
				}
				s.SendToolResults([]provider.ToolResult{{ID: c.ID, Name: c.Name, Output: map[string]any{"level": 30}}})
			case provider.EventOutputTranscript:
				spoken = append(spoken, e.Text)
			case provider.EventTurnComplete:
				done = true
			}
		case <-timeout:
			t.Fatal("turn did not complete")
		}
	}
	if len(spoken) != 2 || spoken[0] != "Сейчас. " {
		t.Fatalf("spoken = %q", spoken)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	msgs := reqs[1]["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("second request has %d messages: %v", len(msgs), msgs)
	}
	asst := msgs[2].(map[string]any)
	call := asst["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["arguments"] != `{"level":30}` {
		t.Fatalf("assistant message = %v", asst)
	}
	if _, ok := call["index"]; ok {
		t.Fatal("index is sent back to the API")
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != `{"level":30}` {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestErrorIsSpoken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"quota"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Recognizer: fakeSpeech{}, Synthesizer: fakeSpeech{}})
	s, _ := p.Connect(context.Background(), provider.SessionOptions{InputFormat: models.DeviceInputFormat})
	defer s.Close()
	s.(provider.TextSender).SendText("привет")

	var spoke bool
	for e := range s.Events() {
		if e.Kind == provider.EventAudio {
			spoke = true
		}
		if e.Kind == provider.EventTurnComplete {
			break
		}
	}
	if !spoke {
		t.Fatal("the failure was not spoken")
	}
}
