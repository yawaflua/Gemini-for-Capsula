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

// Package openairt implements [provider.Provider] with the OpenAI Realtime
// API: speech to speech over a WebSocket, like the Gemini Live API.
//
// The API takes and produces 24 kHz audio; speech from the device is
// resampled on the way in.
package openairt

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Defaults.
const (
	DefaultBaseURL = "wss://api.openai.com/v1"
	DefaultModel   = "gpt-realtime"
	DefaultVoice   = "marin"
	// DefaultTranscriptionModel transcribes the user's speech for the
	// history in the panel.
	DefaultTranscriptionModel = "gpt-4o-mini-transcribe"
)

// voices are the voices of the realtime models.
var voices = []string{"alloy", "ash", "ballad", "cedar", "coral", "echo", "marin", "sage", "shimmer", "verse"}

// Format is the audio format of the API in both directions.
var Format = models.AudioFormat{Encoding: models.EncodingLinear16, SampleRateHertz: 24000, Channels: 1}

// Config configures the provider.
type Config struct {
	// BaseURL of the API, e.g. wss://api.openai.com/v1; https:// is
	// turned into wss://. Defaults to OpenAI.
	BaseURL string
	APIKey  string
	// Model defaults to [DefaultModel]; Voice to [DefaultVoice].
	Model string
	Voice string
	// TranscriptionModel defaults to [DefaultTranscriptionModel]; "none"
	// disables transcription of the user's speech.
	TranscriptionModel string
	// HTTPClient dials the WebSocket, e.g. through a proxy; nil uses the
	// default.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Provider is an OpenAI Realtime backed [provider.Provider].
type Provider struct {
	cfg Config
	log *slog.Logger

	mu    sync.RWMutex
	model string
	voice string
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.Tuner        = (*Provider)(nil)
	_ provider.VADDetector  = (*Provider)(nil)
	_ provider.TextProvider = (*Provider)(nil)
)

// New creates a provider.
func New(cfg Config) (*Provider, error) {
	base := strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultBaseURL), "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("openairt: base URL: %w", err)
	}
	cfg.BaseURL = base
	cfg.TranscriptionModel = cmp.Or(cfg.TranscriptionModel, DefaultTranscriptionModel)
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Provider{cfg: cfg, log: log, model: cmp.Or(cfg.Model, DefaultModel), voice: cmp.Or(cfg.Voice, DefaultVoice)}, nil
}

// Tuning implements [provider.Tuner].
func (p *Provider) Tuning() provider.Tuning {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return provider.Tuning{Model: p.model, Voice: p.voice}
}

// Tune implements [provider.Tuner].
func (p *Provider) Tune(t provider.Tuning) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.Model != "" {
		p.model = t.Model
	}
	if t.Voice != "" {
		p.voice = t.Voice
	}
}

// Voices implements [provider.VoiceLister].
func (p *Provider) Voices() []string { return voices }

// SupportsServerVAD implements [provider.VADDetector].
func (p *Provider) SupportsServerVAD() bool { return true }

// AcceptsText implements [provider.TextProvider].
func (p *Provider) AcceptsText() bool { return true }

// Name implements [provider.Provider].
func (p *Provider) Name() string { return "openai-realtime/" + p.Tuning().Model }

// OutputFormat implements [provider.Provider].
func (p *Provider) OutputFormat() models.AudioFormat { return Format }

// Connect implements [provider.Provider].
func (p *Provider) Connect(ctx context.Context, opts provider.SessionOptions) (provider.Session, error) {
	if err := opts.InputFormat.Validate(); err != nil {
		return nil, fmt.Errorf("openairt: %w", err)
	}
	if opts.InputFormat.Channels != 1 {
		return nil, errors.New("openairt: only mono input is supported")
	}
	t := p.Tuning()
	h := http.Header{}
	if p.cfg.APIKey != "" {
		h.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	ws, resp, err := websocket.Dial(ctx, p.cfg.BaseURL+"/realtime?model="+url.QueryEscape(t.Model), &websocket.DialOptions{
		HTTPClient: p.cfg.HTTPClient,
		HTTPHeader: h,
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("openairt: connect: %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("openairt: connect: %w", err)
	}
	ws.SetReadLimit(16 << 20)
	s := &session{
		ws:        ws,
		log:       p.log,
		serverVAD: opts.ServerVAD,
		resampler: audio.NewResampler(opts.InputFormat.SampleRateHertz, Format.SampleRateHertz),
		events:    make(chan provider.Event, 256),
		done:      make(chan struct{}),
	}
	if err := s.write(ctx, map[string]any{"type": "session.update", "session": p.sessionConfig(opts, t)}); err != nil {
		ws.CloseNow()
		return nil, fmt.Errorf("openairt: configure session: %w", err)
	}
	go s.receive()
	return s, nil
}

func (p *Provider) sessionConfig(opts provider.SessionOptions, t provider.Tuning) map[string]any {
	format := map[string]any{"type": "audio/pcm", "rate": Format.SampleRateHertz}
	input := map[string]any{"format": format}
	if opts.ServerVAD {
		input["turn_detection"] = map[string]any{
			"type":               "semantic_vad",
			"eagerness":          "high",
			"create_response":    true,
			"interrupt_response": true,
		}
	} else {
		input["turn_detection"] = nil // the device marks turn boundaries
	}
	if p.cfg.TranscriptionModel != "none" {
		tr := map[string]any{"model": p.cfg.TranscriptionModel}
		if lang, _, _ := strings.Cut(opts.Language, "-"); len(lang) == 2 {
			tr["language"] = strings.ToLower(lang)
		}
		input["transcription"] = tr
	}
	fns := make([]map[string]any, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		fns = append(fns, function(tool))
	}
	cfg := map[string]any{
		"type":              "realtime",
		"model":             t.Model,
		"output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input":  input,
			"output": map[string]any{"format": format, "voice": t.Voice},
		},
		"tools":       fns,
		"tool_choice": "auto",
	}
	if opts.SystemInstruction != "" {
		cfg["instructions"] = opts.SystemInstruction
	}
	return cfg
}

func function(t tools.Tool) map[string]any {
	return map[string]any{"type": "function", "name": t.Name(), "description": t.Description, "parameters": t.Schema()}
}

// session is one Realtime API conversation.
type session struct {
	ws        *websocket.Conn
	log       *slog.Logger
	serverVAD bool
	events    chan provider.Event
	done      chan struct{}

	mu        sync.Mutex
	resampler *audio.Resampler
	buffered  int  // bytes appended since the last commit or clear
	active    bool // a response is being generated
	awaiting  bool // a response was requested and the turn is not complete
	calls     []provider.ToolCall

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

var (
	_ provider.Session    = (*session)(nil)
	_ provider.TextSender = (*session)(nil)
)

func (s *session) write(ctx context.Context, ev map[string]any) error {
	select {
	case <-s.done:
		return provider.ErrClosed
	default:
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// coder/websocket serializes concurrent writers itself.
	return s.ws.Write(ctx, websocket.MessageText, b)
}

func (s *session) send(ev map[string]any) error { return s.write(context.Background(), ev) }

// StartTurn implements [provider.Session]. Without server VAD it stops a
// reply in progress (barge-in) and drops stale audio.
func (s *session) StartTurn() error {
	if s.serverVAD {
		return nil
	}
	s.mu.Lock()
	active := s.active
	s.buffered = 0
	s.awaiting = false
	s.calls = nil
	s.mu.Unlock()
	if active {
		if err := s.send(map[string]any{"type": "response.cancel"}); err != nil {
			return err
		}
	}
	return s.send(map[string]any{"type": "input_audio_buffer.clear"})
}

// SendAudio implements [provider.Session].
func (s *session) SendAudio(pcm []byte) error {
	s.mu.Lock()
	pcm = s.resampler.Write(pcm)
	s.buffered += len(pcm)
	s.mu.Unlock()
	if len(pcm) == 0 {
		return nil
	}
	return s.send(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm)})
}

// minCommit is the shortest audio the API accepts in a commit (100 ms).
var minCommit = Format.FrameSize(100 * time.Millisecond)

// EndTurn implements [provider.Session]. With server VAD the API closes the
// utterance itself; otherwise the audio is committed and answered.
func (s *session) EndTurn() error {
	if s.serverVAD {
		return nil
	}
	s.mu.Lock()
	enough := s.buffered >= minCommit
	s.buffered = 0
	if enough {
		s.awaiting = true
	}
	s.mu.Unlock()
	if !enough {
		// Nothing to answer; complete the turn so the device goes idle.
		s.emit(provider.Event{Kind: provider.EventTurnComplete})
		return nil
	}
	if err := s.send(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return err
	}
	return s.send(map[string]any{"type": "response.create"})
}

// SendText implements [provider.TextSender].
func (s *session) SendText(text string) error {
	s.mu.Lock()
	active := s.active
	s.awaiting, s.calls = true, nil
	s.mu.Unlock()
	if active {
		if err := s.send(map[string]any{"type": "response.cancel"}); err != nil {
			return err
		}
	}
	if err := s.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}},
	}}); err != nil {
		return err
	}
	return s.send(map[string]any{"type": "response.create"})
}

// SendToolResults implements [provider.Session].
func (s *session) SendToolResults(results []provider.ToolResult) error {
	for _, r := range results {
		out := r.Output
		if r.Err != "" {
			out = map[string]any{"error": r.Err}
		} else if out == nil {
			out = map[string]any{"output": "ok"}
		}
		b, _ := json.Marshal(out)
		if err := s.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
			"type": "function_call_output", "call_id": r.ID, "output": string(b),
		}}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.awaiting = true
	s.mu.Unlock()
	return s.send(map[string]any{"type": "response.create"})
}

// Events implements [provider.Session].
func (s *session) Events() <-chan provider.Event { return s.events }

// Err implements [provider.Session].
func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *session) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// Close implements [provider.Session].
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.ws.Close(websocket.StatusNormalClosure, "")
	})
	return nil
}

func (s *session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) emit(e provider.Event) bool {
	select {
	case s.events <- e:
		return true
	case <-s.done:
		return false
	}
}

// serverEvent is the union of the server events used here.
type serverEvent struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	CallID     string `json:"call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
	Response   struct {
		Status string `json:"status"`
	} `json:"response"`
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// receive pumps server events into the events channel until the connection
// ends.
func (s *session) receive() {
	defer close(s.events)
	ctx := context.Background()
	for {
		_, b, err := s.ws.Read(ctx)
		if err != nil {
			if !s.closed() {
				s.setErr(err)
				s.Close()
			}
			return
		}
		var ev serverEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			s.log.Warn("openairt: bad server event", "err", err)
			continue
		}
		for _, e := range s.translate(&ev) {
			if !s.emit(e) {
				return
			}
		}
	}
}

// translate converts one server event into zero or more events.
func (s *session) translate(ev *serverEvent) []provider.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case "response.created":
		s.active = true
	case "response.output_audio.delta", "response.audio.delta":
		pcm, err := base64.StdEncoding.DecodeString(ev.Delta)
		if err != nil || len(pcm) == 0 {
			return nil
		}
		return []provider.Event{{Kind: provider.EventAudio, Audio: pcm}}
	case "response.output_audio_transcript.delta", "response.audio_transcript.delta":
		if ev.Delta != "" {
			return []provider.Event{{Kind: provider.EventOutputTranscript, Text: ev.Delta}}
		}
	case "conversation.item.input_audio_transcription.completed":
		if t := strings.TrimSpace(ev.Transcript); t != "" {
			return []provider.Event{{Kind: provider.EventInputTranscript, Text: t}}
		}
	case "response.function_call_arguments.done":
		args := map[string]any{}
		if ev.Arguments != "" {
			if err := json.Unmarshal([]byte(ev.Arguments), &args); err != nil {
				args = map[string]any{}
			}
		}
		s.calls = append(s.calls, provider.ToolCall{ID: ev.CallID, Name: ev.Name, Args: args})
	case "input_audio_buffer.speech_started":
		s.awaiting = true
		if s.active {
			// The API cancels the reply itself (interrupt_response).
			return []provider.Event{{Kind: provider.EventInterrupted}}
		}
	case "input_audio_buffer.speech_stopped":
		return []provider.Event{{Kind: provider.EventSpeechEnd}}
	case "response.done":
		s.active = false
		calls := s.calls
		s.calls = nil
		switch {
		case ev.Response.Status == "cancelled":
			// Barge-in: the next turn has started, it completes on its own.
			return nil
		case len(calls) > 0:
			return []provider.Event{{Kind: provider.EventToolCall, ToolCalls: calls}}
		}
		s.awaiting = false
		return []provider.Event{{Kind: provider.EventTurnComplete}}
	case "error":
		s.log.Warn("openairt: API error", "type", ev.Error.Type, "code", ev.Error.Code, "message", ev.Error.Message)
		if ev.Error.Code == "response_cancel_not_active" {
			return nil
		}
		if s.awaiting && !s.active {
			// The request was rejected: no response will come.
			s.awaiting = false
			return []provider.Event{{Kind: provider.EventTurnComplete}}
		}
	}
	return nil
}
