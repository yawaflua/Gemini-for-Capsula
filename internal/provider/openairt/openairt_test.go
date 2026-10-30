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

package openairt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// fakeAPI plays the server side of a Realtime session: it answers the
// first response.create with a volume_set call and the second with audio.
func fakeAPI(t *testing.T, got chan<- map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "gpt-realtime" ||
			r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(1 << 20)
		ctx := r.Context()
		send := func(ev string) { ws.Write(ctx, websocket.MessageText, []byte(ev)) }
		responses := 0
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var ev map[string]any
			json.Unmarshal(b, &ev)
			got <- ev
			if ev["type"] != "response.create" {
				continue
			}
			responses++
			send(`{"type":"response.created"}`)
			if responses == 1 {
				send(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"сделай потише"}`)
				send(`{"type":"response.function_call_arguments.done","call_id":"c1","name":"volume_set","arguments":"{\"level\":30}"}`)
				send(`{"type":"response.done","response":{"status":"completed"}}`)
				continue
			}
			pcm := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
			send(`{"type":"response.output_audio.delta","delta":"` + pcm + `"}`)
			send(`{"type":"response.output_audio_transcript.delta","delta":"Готово."}`)
			send(`{"type":"response.done","response":{"status":"completed"}}`)
		}
	}))
}

func TestTurnWithToolCall(t *testing.T) {
	got := make(chan map[string]any, 100)
	srv := fakeAPI(t, got)
	defer srv.Close()

	p, err := New(Config{BaseURL: "http" + strings.TrimPrefix(srv.URL, "http") + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := tools.Lookup("volume_set")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := p.Connect(ctx, provider.SessionOptions{
		SystemInstruction: "Ты Маруся.",
		Language:          "ru-RU",
		InputFormat:       models.DeviceInputFormat,
		Tools:             []tools.Tool{vol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	upd := <-got
	if upd["type"] != "session.update" {
		t.Fatalf("first event = %v", upd["type"])
	}
	sess := upd["session"].(map[string]any)
	in := sess["audio"].(map[string]any)["input"].(map[string]any)
	if in["turn_detection"] != nil {
		t.Fatalf("turn_detection = %v, want null without server VAD", in["turn_detection"])
	}
	if in["transcription"].(map[string]any)["language"] != "ru" || sess["instructions"] != "Ты Маруся." {
		t.Fatalf("session = %v", sess)
	}

	s.StartTurn()
	s.SendAudio(make([]byte, models.DeviceInputFormat.FrameSize(200*time.Millisecond)))
	s.EndTurn()

	var kinds []provider.EventKind
	var audio []byte
	for done := false; !done; {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("session ended: %v", s.Err())
			}
			kinds = append(kinds, e.Kind)
			switch e.Kind {
			case provider.EventToolCall:
				c := e.ToolCalls[0]
				if c.ID != "c1" || c.Args["level"] != float64(30) {
					t.Fatalf("call = %+v", c)
				}
				s.SendToolResults([]provider.ToolResult{{ID: c.ID, Name: c.Name, Output: map[string]any{"level": 30}}})
			case provider.EventAudio:
				audio = append(audio, e.Audio...)
			case provider.EventTurnComplete:
				done = true
			}
		case <-ctx.Done():
			t.Fatalf("turn did not complete; events %v", kinds)
		}
	}
	if len(audio) != 4 {
		t.Fatalf("audio = %v", audio)
	}

	// Client events after the session update.
	var types []string
	var appended int
	for len(got) > 0 {
		ev := <-got
		types = append(types, ev["type"].(string))
		if ev["type"] == "input_audio_buffer.append" {
			b, _ := base64.StdEncoding.DecodeString(ev["audio"].(string))
			appended += len(b)
		}
		if ev["type"] == "conversation.item.create" {
			item := ev["item"].(map[string]any)
			if item["call_id"] != "c1" || item["output"] != `{"level":30}` {
				t.Fatalf("function output = %v", item)
			}
		}
	}
	want := "input_audio_buffer.clear input_audio_buffer.append input_audio_buffer.commit response.create conversation.item.create response.create"
	if strings.Join(types, " ") != want {
		t.Fatalf("client events:\n got %v\nwant %v", types, want)
	}
	// 200 ms resampled from 16 to 24 kHz.
	if n := Format.FrameSize(200 * time.Millisecond); appended < n-4 || appended > n+4 {
		t.Fatalf("appended %d bytes, want ~%d", appended, n)
	}
}

func TestShortTurnCompletesWithoutRequest(t *testing.T) {
	got := make(chan map[string]any, 100)
	srv := fakeAPI(t, got)
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k"})
	s, err := p.Connect(context.Background(), provider.SessionOptions{InputFormat: models.DeviceInputFormat})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.StartTurn()
	s.SendAudio(make([]byte, 320)) // 10 ms
	s.EndTurn()
	select {
	case e := <-s.Events():
		if e.Kind != provider.EventTurnComplete {
			t.Fatalf("got %v", e.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no turn_complete")
	}
}

func TestServerVADEvents(t *testing.T) {
	s := &session{serverVAD: true}
	s.translate(&serverEvent{Type: "response.created"})
	if ev := s.translate(&serverEvent{Type: "input_audio_buffer.speech_started"}); len(ev) != 1 || ev[0].Kind != provider.EventInterrupted {
		t.Fatalf("speech_started during a reply = %v", ev)
	}
	if ev := s.translate(&serverEvent{Type: "response.done", Response: struct {
		Status string `json:"status"`
	}{"cancelled"}}); len(ev) != 0 {
		t.Fatalf("a cancelled reply must not complete the new turn: %v", ev)
	}
	if ev := s.translate(&serverEvent{Type: "input_audio_buffer.speech_stopped"}); len(ev) != 1 || ev[0].Kind != provider.EventSpeechEnd {
		t.Fatalf("speech_stopped = %v", ev)
	}
}
