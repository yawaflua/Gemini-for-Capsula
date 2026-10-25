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

package gemini

import (
	"errors"
	"sync"

	"google.golang.org/genai"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
)

// session adapts a [genai.Session] to [provider.Session].
type session struct {
	gs          *genai.Session
	inputFormat models.AudioFormat
	serverVAD   bool
	events      chan provider.Event
	done        chan struct{}

	// writeMu serializes writes: the underlying WebSocket connection does
	// not support concurrent writers.
	writeMu sync.Mutex

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

var (
	_ provider.Session    = (*session)(nil)
	_ provider.TextSender = (*session)(nil)
)

func newSession(gs *genai.Session, in models.AudioFormat, serverVAD bool) *session {
	return &session{
		gs:          gs,
		inputFormat: in,
		serverVAD:   serverVAD,
		events:      make(chan provider.Event, 256),
		done:        make(chan struct{}),
	}
}

func (s *session) send(in genai.LiveRealtimeInput) error {
	select {
	case <-s.done:
		return provider.ErrClosed
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.gs.SendRealtimeInput(in)
}

func (s *session) StartTurn() error {
	if s.serverVAD {
		return nil
	}
	return s.send(genai.LiveRealtimeInput{ActivityStart: &genai.ActivityStart{}})
}

func (s *session) SendAudio(pcm []byte) error {
	return s.send(genai.LiveRealtimeInput{
		Audio: &genai.Blob{Data: pcm, MIMEType: s.inputFormat.MIMEType()},
	})
}

func (s *session) EndTurn() error {
	if s.serverVAD {
		// Flushes buffered audio so the VAD can close the utterance.
		return s.send(genai.LiveRealtimeInput{AudioStreamEnd: true})
	}
	return s.send(genai.LiveRealtimeInput{ActivityEnd: &genai.ActivityEnd{}})
}

// SendText implements [provider.TextSender]. The text goes through the
// realtime input like speech: native audio models may leave the first
// client-content turn of a session unanswered. Without server VAD the text
// is wrapped in explicit activity markers, as audio would be.
func (s *session) SendText(text string) error {
	if !s.serverVAD {
		if err := s.send(genai.LiveRealtimeInput{ActivityStart: &genai.ActivityStart{}}); err != nil {
			return err
		}
	}
	if err := s.send(genai.LiveRealtimeInput{Text: text}); err != nil {
		return err
	}
	if !s.serverVAD {
		return s.send(genai.LiveRealtimeInput{ActivityEnd: &genai.ActivityEnd{}})
	}
	return nil
}

func (s *session) SendToolResults(results []provider.ToolResult) error {
	select {
	case <-s.done:
		return provider.ErrClosed
	default:
	}
	resp := make([]*genai.FunctionResponse, 0, len(results))
	for _, r := range results {
		out := r.Output
		if r.Err != "" {
			out = map[string]any{"error": r.Err}
		} else if out == nil {
			out = map[string]any{"output": "ok"}
		}
		resp = append(resp, &genai.FunctionResponse{ID: r.ID, Name: r.Name, Response: out})
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.gs.SendToolResponse(genai.LiveToolResponseInput{FunctionResponses: resp})
}

func (s *session) Events() <-chan provider.Event { return s.events }

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if errors.Is(s.err, errFromClose) {
		return nil
	}
	return s.err
}

func (s *session) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.setErr(errFromClose)
		close(s.done)
		err = s.gs.Close()
	})
	return err
}

// emit delivers e unless the session is closed.
func (s *session) emit(e provider.Event) bool {
	select {
	case s.events <- e:
		return true
	case <-s.done:
		return false
	}
}

// receive pumps backend messages into the events channel until the
// connection ends.
func (s *session) receive() {
	defer close(s.events)
	for {
		msg, err := s.gs.Receive()
		if err != nil {
			s.setErr(err)
			s.Close()
			return
		}
		for _, e := range translate(msg) {
			if !s.emit(e) {
				return
			}
		}
	}
}

// translate converts one server message into zero or more events.
func translate(msg *genai.LiveServerMessage) []provider.Event {
	var out []provider.Event
	if va := msg.VoiceActivity; va != nil && va.VoiceActivityType == genai.VoiceActivityTypeActivityEnd {
		out = append(out, provider.Event{Kind: provider.EventSpeechEnd})
	}
	if sc := msg.ServerContent; sc != nil {
		if t := sc.InputTranscription; t != nil && t.Text != "" {
			out = append(out, provider.Event{Kind: provider.EventInputTranscript, Text: t.Text})
		}
		if sc.ModelTurn != nil {
			for _, p := range sc.ModelTurn.Parts {
				if p.InlineData != nil && len(p.InlineData.Data) > 0 {
					out = append(out, provider.Event{Kind: provider.EventAudio, Audio: p.InlineData.Data})
				}
			}
		}
		if t := sc.OutputTranscription; t != nil && t.Text != "" {
			out = append(out, provider.Event{Kind: provider.EventOutputTranscript, Text: t.Text})
		}
		if sc.Interrupted {
			out = append(out, provider.Event{Kind: provider.EventInterrupted})
		}
		if sc.TurnComplete {
			out = append(out, provider.Event{Kind: provider.EventTurnComplete})
		}
	}
	if tc := msg.ToolCall; tc != nil && len(tc.FunctionCalls) > 0 {
		calls := make([]provider.ToolCall, 0, len(tc.FunctionCalls))
		for _, fc := range tc.FunctionCalls {
			calls = append(calls, provider.ToolCall{ID: fc.ID, Name: fc.Name, Args: fc.Args})
		}
		out = append(out, provider.Event{Kind: provider.EventToolCall, ToolCalls: calls})
	}
	if c := msg.ToolCallCancellation; c != nil && len(c.IDs) > 0 {
		out = append(out, provider.Event{Kind: provider.EventToolCallCancel, CancelledIDs: c.IDs})
	}
	if msg.GoAway != nil {
		out = append(out, provider.Event{Kind: provider.EventGoAway})
	}
	return out
}
