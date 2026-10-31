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

// Package openaichat implements [provider.Provider] with a text model behind
// an OpenAI compatible Chat Completions API: OpenAI itself, OpenRouter,
// DeepSeek, Groq, Ollama, LM Studio, vLLM, ...
//
// Like provider/claude it is a speech pipeline (see provider/pipeline).
package openaichat

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/pipeline"
	"github.com/yawaflua/marusya-gemini/internal/speech"
	"github.com/yawaflua/marusya-gemini/internal/speech/openaispeech"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Defaults.
const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-5-mini"
)

// maxIterations bounds the tool-call loop of one turn.
const maxIterations = 8

// Config configures the provider.
type Config struct {
	// BaseURL of the API, up to and including /v1. Defaults to OpenAI.
	BaseURL string
	APIKey  string
	// Model defaults to [DefaultModel].
	Model string
	// Label names the backend in logs and the panel, e.g. "openrouter".
	// Defaults to "openai".
	Label string
	// FastPath routes short speaker commands (volume, AirPlay, weather,
	// time, simple smart home) straight to the tools, without the text
	// model. Off by default.
	FastPath bool

	Recognizer  speech.Recognizer
	Synthesizer speech.Synthesizer
	// HTTPClient sends the API requests; nil uses http.DefaultClient.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Provider is an OpenAI compatible [provider.Provider].
type Provider struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	mu    sync.RWMutex
	model string
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Tuner    = (*Provider)(nil)
)

// voicer is implemented by synthesizers whose voice can be changed at runtime.
type voicer interface {
	Voice() string
	SetVoice(string)
}

// New creates a provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Recognizer == nil || cfg.Synthesizer == nil {
		return nil, errors.New("openaichat: a speech recognizer and synthesizer are required")
	}
	cfg.BaseURL = strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultBaseURL), "/")
	cfg.Model = cmp.Or(cfg.Model, DefaultModel)
	cfg.Label = cmp.Or(cfg.Label, "openai")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Provider{cfg: cfg, http: hc, log: log, model: cfg.Model}, nil
}

func (p *Provider) currentModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model
}

// Tuning implements [provider.Tuner]. The voice is the synthesizer's.
func (p *Provider) Tuning() provider.Tuning {
	t := provider.Tuning{Model: p.currentModel()}
	if v, ok := p.cfg.Synthesizer.(voicer); ok {
		t.Voice = v.Voice()
	}
	return t
}

// Tune implements [provider.Tuner].
func (p *Provider) Tune(t provider.Tuning) {
	if t.Model != "" {
		p.mu.Lock()
		p.model = t.Model
		p.mu.Unlock()
	}
	if v, ok := p.cfg.Synthesizer.(voicer); ok && t.Voice != "" {
		v.SetVoice(t.Voice)
	}
}

// Voices implements [provider.VoiceLister] with the synthesizer's voices.
func (p *Provider) Voices() []string { return provider.Voices(p.cfg.Synthesizer) }

// AcceptsText implements [provider.TextProvider].
func (p *Provider) AcceptsText() bool { return true }

// Name implements [provider.Provider].
func (p *Provider) Name() string { return p.cfg.Label + "/" + p.currentModel() }

// OutputFormat implements [provider.Provider].
func (p *Provider) OutputFormat() models.AudioFormat { return p.cfg.Synthesizer.Format() }

// Connect implements [provider.Provider].
func (p *Provider) Connect(_ context.Context, opts provider.SessionOptions) (provider.Session, error) {
	if err := opts.InputFormat.Validate(); err != nil {
		return nil, err
	}
	c := &conversation{p: p, model: p.currentModel(), tools: functions(opts.Tools)}
	if opts.SystemInstruction != "" {
		c.history = []message{{Role: "system", Content: opts.SystemInstruction}}
	}
	return pipeline.New(pipeline.Config{
		Recognizer:  p.cfg.Recognizer,
		Synthesizer: p.cfg.Synthesizer,
		Options:     opts,
		Name:        p.cfg.Label,
		Logger:      p.log,
	}, c), nil
}

// Wire types of the Chat Completions API.
type (
	message struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []toolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	toolCall struct {
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	// deltaCall is a piece of a tool call in a stream chunk.
	deltaCall struct {
		toolCall
		Index int `json:"index"`
	}
	function struct {
		Type     string         `json:"type"`
		Function map[string]any `json:"function"`
	}
	chunk struct {
		Choices []struct {
			Delta struct {
				Content   string      `json:"content"`
				ToolCalls []deltaCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
)

func functions(ts []tools.Tool) []function {
	out := make([]function, 0, len(ts))
	for _, t := range ts {
		out = append(out, function{Type: "function", Function: map[string]any{
			"name": t.Name(), "description": t.Description, "parameters": t.Schema(),
		}})
	}
	return out
}

// conversation is the model side of a [pipeline.Session].
type conversation struct {
	p       *Provider
	model   string
	tools   []function
	history []message
}

// Turn implements [pipeline.Conversation].
func (c *conversation) Turn(ctx context.Context, text string, out *pipeline.Output) error {
	// Short speaker commands bypass the text model: volume and AirPlay stay
	// responsive when the backend is slow, offline or rate-limited.
	if c.p.cfg.FastPath {
		if msg, ok := c.fastPath(ctx, text, out); ok {
			if msg.Content != "" {
				c.history = append(c.history, message{Role: "user", Content: text}, msg)
			}
			return nil
		}
	}
	// Messages of this turn join the history only if it completes.
	msgs := append(c.history[:len(c.history):len(c.history)], message{Role: "user", Content: text})
	for range maxIterations {
		msg, err := c.generate(ctx, msgs, out)
		if err != nil {
			return err
		}
		msgs = append(msgs, msg)
		if len(msg.ToolCalls) == 0 {
			break
		}
		calls := make([]provider.ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			args := map[string]any{}
			if tc.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					args = map[string]any{}
				}
			}
			calls[i] = provider.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args}
		}
		results, ok := out.CallTools(calls)
		if !ok {
			return nil
		}
		for _, r := range results {
			content := r.Err
			if content == "" {
				b, _ := json.Marshal(r.Output)
				if r.Output == nil {
					b = []byte(`{"output":"ok"}`)
				}
				content = string(b)
			} else {
				content = `{"error":` + jsonString(content) + `}`
			}
			msgs = append(msgs, message{Role: "tool", ToolCallID: r.ID, Content: content})
		}
	}
	if ctx.Err() == nil {
		c.history = msgs
	}
	return nil
}

// generate streams one response, speaking text as it arrives.
func (c *conversation) generate(ctx context.Context, msgs []message, out *pipeline.Output) (message, error) {
	req := map[string]any{"model": c.model, "messages": msgs, "stream": true}
	if len(c.tools) > 0 {
		req["tools"] = c.tools
	}
	body, err := json.Marshal(req)
	if err != nil {
		return message{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	if c.p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+c.p.cfg.APIKey)
	}
	resp, err := c.p.http.Do(hr)
	if err != nil {
		return message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return message{}, openaispeech.APIError(resp)
	}

	msg := message{Role: "assistant"}
	var content strings.Builder
	calls := map[int]*toolCall{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return message{}, fmt.Errorf("bad stream chunk: %w", err)
		}
		if ch.Error != nil {
			return message{}, errors.New(ch.Error.Message)
		}
		for _, choice := range ch.Choices {
			if t := choice.Delta.Content; t != "" {
				content.WriteString(t)
				out.Write(t)
			}
			for _, d := range choice.Delta.ToolCalls {
				tc := calls[d.Index]
				if tc == nil {
					tc = &toolCall{Type: "function"}
					calls[d.Index] = tc
				}
				if d.ID != "" {
					tc.ID = d.ID
				}
				if d.Function.Name != "" {
					tc.Function.Name = d.Function.Name
				}
				tc.Function.Arguments += d.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return message{}, err
	}
	out.Flush()
	msg.Content = content.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for n, i := range idx {
		tc := *calls[i]
		if tc.ID == "" { // some local servers omit IDs
			tc.ID = fmt.Sprintf("call_%d", n)
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	return msg, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
