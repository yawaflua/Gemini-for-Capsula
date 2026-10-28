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

// Package claude implements [provider.Provider] with Claude via the
// Anthropic Messages API.
//
// Claude is a text model, so a turn is a pipeline: the user's audio is
// transcribed by a [speech.Recognizer], Claude answers (calling device tools
// and searching the web as needed) and the reply is spoken sentence by
// sentence by a [speech.Synthesizer] while it is still being generated.
package claude

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/speech"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// DefaultModel is a fast model suited to voice.
const DefaultModel = "claude-haiku-5-5"

// Config configures the provider.
type Config struct {
	// APIKey authenticates against the Claude API. If empty, the SDK
	// resolves credentials itself ($ANTHROPIC_API_KEY, an `ant auth login`
	// profile, ...).
	APIKey string
	// Model defaults to [DefaultModel].
	Model string
	// Effort is "low", "medium", "high", ... Empty keeps the model default.
	// Low effort keeps spoken answers quick.
	Effort string
	// WebSearch enables the server-side web search tool.
	WebSearch bool
	// MaxTokens bounds one response, thinking included.
	MaxTokens int64

	Recognizer  speech.Recognizer
	Synthesizer speech.Synthesizer
	// BaseURL overrides the API endpoint (for tests and proxies).
	BaseURL string
	// HTTPClient sends the API requests, e.g. through a proxy; nil uses
	// the SDK default.
	HTTPClient *http.Client
	// Logger receives pipeline errors; optional.
	Logger *slog.Logger
}

// Provider is a Claude backed [provider.Provider].
type Provider struct {
	cfg    Config
	client anthropic.Client
	log    *slog.Logger

	mu    sync.RWMutex // guards model, see Tune
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
		return nil, errors.New("claude: a speech recognizer and synthesizer are required")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 16000
	}
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Provider{cfg: cfg, client: anthropic.NewClient(opts...), log: log, model: cfg.Model}, nil
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

// AcceptsText implements [provider.TextProvider].
func (p *Provider) AcceptsText() bool { return true }

// Name implements [provider.Provider].
func (p *Provider) Name() string { return "claude/" + p.currentModel() }

// OutputFormat implements [provider.Provider].
func (p *Provider) OutputFormat() models.AudioFormat { return p.cfg.Synthesizer.Format() }

// Connect implements [provider.Provider]. The conversation lives in the
// session; nothing is sent until the first turn ends.
func (p *Provider) Connect(_ context.Context, opts provider.SessionOptions) (provider.Session, error) {
	if err := opts.InputFormat.Validate(); err != nil {
		return nil, err
	}
	return newSession(p, opts), nil
}

// params builds the request parameters shared by every call of a session.
func (p *Provider) params(opts provider.SessionOptions) anthropic.MessageNewParams {
	model := p.currentModel()
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: p.cfg.MaxTokens,
		Tools:     toolParams(opts.Tools),
		// Caches the growing conversation between turns.
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	if opts.SystemInstruction != "" {
		params.System = []anthropic.TextBlockParam{{Text: opts.SystemInstruction}}
	}
	if p.cfg.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(p.cfg.Effort)}
	}
	if p.cfg.WebSearch {
		params.Tools = append(params.Tools, webSearch(model))
	}
	return params
}

// webSearch returns the newest web search tool the model supports: the
// dynamic-filtering variant needs a large model, Haiku uses the basic one.
func webSearch(model string) anthropic.ToolUnionParam {
	if isHaiku(model) {
		return anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{MaxUses: anthropic.Int(3)}}
	}
	return anthropic.ToolUnionParam{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{MaxUses: anthropic.Int(3)}}
}

func isHaiku(model string) bool {
	return len(model) >= 12 && model[:12] == "claude-haiku"
}

// toolParams converts provider-agnostic tools into Claude tool definitions.
func toolParams(ts []tools.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(ts)+1)
	for _, t := range ts {
		props := map[string]any{}
		var required []string
		for _, p := range t.Parameters {
			s := map[string]any{"type": string(p.Type), "description": p.Description}
			if len(p.Enum) > 0 {
				s["enum"] = p.Enum
			}
			if p.Minimum != nil {
				s["minimum"] = *p.Minimum
			}
			if p.Maximum != nil {
				s["maximum"] = *p.Maximum
			}
			props[p.Name] = s
			if p.Required {
				required = append(required, p.Name)
			}
		}
		tp := anthropic.ToolParam{
			Name:        t.Name(),
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: required},
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out
}
