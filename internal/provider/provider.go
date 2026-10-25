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

// Package provider defines the interface between the server and a realtime
// speech-to-speech model backend.
//
// Implementations live in subpackages (see provider/gemini). The server only
// depends on this package, so a backend can be swapped without touching the
// session logic.
package provider

import (
	"context"
	"errors"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// ErrClosed is returned by [Session] methods after the session ended.
var ErrClosed = errors.New("provider: session closed")

// Provider opens realtime conversation sessions.
type Provider interface {
	// Name identifies the provider in logs.
	Name() string
	// OutputFormat is the format of [EventAudio] payloads.
	OutputFormat() models.AudioFormat
	// Connect opens a new session.
	Connect(ctx context.Context, opts SessionOptions) (Session, error)
}

// SessionOptions configures a [Session].
// VADDetector is implemented by providers that can detect the end of speech
// themselves (see SessionOptions.ServerVAD).
type VADDetector interface {
	SupportsServerVAD() bool
}

// SupportsServerVAD reports whether p can detect the end of speech.
func SupportsServerVAD(p Provider) bool {
	v, ok := p.(VADDetector)
	return ok && v.SupportsServerVAD()
}

// Tuning holds the provider settings that may change while the server runs.
// Empty fields mean "not supported" or "keep the current value".
type Tuning struct {
	Model string `json:"model,omitempty"`
	Voice string `json:"voice,omitempty"`
}

// Tuner is implemented by providers whose model and voice can be changed at
// runtime. New values apply to sessions opened afterwards.
type Tuner interface {
	Tuning() Tuning
	Tune(Tuning)
}

// TextSender is implemented by sessions that accept a typed request instead
// of speech. The reply is delivered like the answer to a spoken turn, ending
// with [EventTurnComplete].
type TextSender interface {
	SendText(text string) error
}

// TextProvider is implemented by providers whose sessions are [TextSender]s.
type TextProvider interface {
	AcceptsText() bool
}

// AcceptsText reports whether sessions of p accept typed requests.
func AcceptsText(p Provider) bool {
	t, ok := p.(TextProvider)
	return ok && t.AcceptsText()
}

type SessionOptions struct {
	// SystemInstruction is the persona/system prompt.
	SystemInstruction string
	// Language is a BCP-47 language code for speech recognition and synthesis.
	Language string
	// ExtraLanguages are other languages the user may speak, hints for
	// speech recognition.
	ExtraLanguages []string
	// InputFormat is the format of audio passed to [Session.SendAudio].
	InputFormat models.AudioFormat
	// Tools are the functions the model may call.
	Tools []tools.Tool
	// ServerVAD lets the backend detect the start and end of speech. Then
	// [Session.StartTurn] is a no-op, [Session.EndTurn] only flushes the
	// audio stream and [EventSpeechEnd] reports the end of the utterance.
	ServerVAD bool
}

// Session is a single stateful conversation with the model.
//
// Conversation history is preserved across turns of the same session.
// Send methods are safe for concurrent use.
type Session interface {
	// StartTurn signals that the user started speaking.
	StartTurn() error
	// SendAudio streams user audio in [SessionOptions.InputFormat].
	SendAudio(pcm []byte) error
	// EndTurn signals that the user finished speaking.
	EndTurn() error
	// SendToolResults answers [EventToolCall] events.
	SendToolResults(results []ToolResult) error
	// Events delivers model output. The channel is closed when the session
	// ends; [Session.Err] then reports why.
	Events() <-chan Event
	// Err returns the error that terminated the session, if any.
	Err() error
	// Close terminates the session.
	Close() error
}

// EventKind discriminates an [Event].
type EventKind int

const (
	// EventAudio carries a chunk of synthesized speech in Audio.
	EventAudio EventKind = iota + 1
	// EventInputTranscript carries recognized user speech in Text.
	EventInputTranscript
	// EventOutputTranscript carries the text of the model's reply in Text.
	EventOutputTranscript
	// EventToolCall carries function calls in ToolCalls.
	EventToolCall
	// EventToolCallCancel lists cancelled call IDs in CancelledIDs.
	EventToolCallCancel
	// EventInterrupted means the model stopped its reply because the user
	// started speaking.
	EventInterrupted
	// EventTurnComplete means the model finished its reply.
	EventTurnComplete
	// EventGoAway means the backend will close the session soon.
	EventGoAway
	// EventSpeechEnd means the backend detected the end of user speech.
	EventSpeechEnd
)

func (k EventKind) String() string {
	switch k {
	case EventAudio:
		return "audio"
	case EventInputTranscript:
		return "input_transcript"
	case EventOutputTranscript:
		return "output_transcript"
	case EventToolCall:
		return "tool_call"
	case EventToolCallCancel:
		return "tool_call_cancel"
	case EventInterrupted:
		return "interrupted"
	case EventTurnComplete:
		return "turn_complete"
	case EventGoAway:
		return "go_away"
	case EventSpeechEnd:
		return "speech_end"
	}
	return "unknown"
}

// Event is a single piece of model output.
type Event struct {
	Kind         EventKind
	Audio        []byte
	Text         string
	ToolCalls    []ToolCall
	CancelledIDs []string
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolResult is the outcome of a [ToolCall].
type ToolResult struct {
	ID     string
	Name   string
	Output map[string]any
	Err    string
}
