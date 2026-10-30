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

package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/claude"
	"github.com/yawaflua/marusya-gemini/internal/provider/gemini"
	"github.com/yawaflua/marusya-gemini/internal/provider/openaichat"
	"github.com/yawaflua/marusya-gemini/internal/provider/openairt"
	"github.com/yawaflua/marusya-gemini/internal/speech"
	"github.com/yawaflua/marusya-gemini/internal/speech/geminispeech"
	"github.com/yawaflua/marusya-gemini/internal/speech/openaispeech"
)

var kinds = []KindInfo{
	{Kind: KindGemini, Label: "Gemini Live (голос в голос)", BaseURL: "https://generativelanguage.googleapis.com", Model: gemini.DefaultModel},
	{Kind: KindOpenAIRealtime, Label: "OpenAI Realtime (голос в голос)", BaseURL: openairt.DefaultBaseURL, Model: openairt.DefaultModel},
	{Kind: KindOpenAI, Label: "OpenAI-совместимый чат (OpenRouter, Ollama, DeepSeek…)", BaseURL: openaichat.DefaultBaseURL, Model: openaichat.DefaultModel, Pipeline: true},
	{Kind: KindAnthropic, Label: "Anthropic Claude", BaseURL: "https://api.anthropic.com", Model: claude.DefaultModel, Pipeline: true},
}

// BuildOptions configure [NewBuilder].
type BuildOptions struct {
	// HTTPClient sends requests to OpenAI and Anthropic compatible APIs,
	// e.g. through a proxy; nil uses the default.
	HTTPClient *http.Client
	// GeminiTransport carries Gemini REST requests; nil uses the default.
	GeminiTransport http.RoundTripper
	// GeminiAPIKey is used for the default speech of pipeline providers
	// (Spec.Speech empty); empty uses $GEMINI_API_KEY.
	GeminiAPIKey string
	// GoogleSearch grounds Gemini answers; it also enables web search for
	// Claude.
	GoogleSearch bool
	// ClaudeEffort is the effort of Claude models, e.g. "low".
	ClaudeEffort string
	Logger       *slog.Logger
}

// NewBuilder returns a [Builder] for every [Kind].
func NewBuilder(o BuildOptions) Builder {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return func(ctx context.Context, s Spec, all []Spec) (provider.Provider, error) {
		switch s.Kind {
		case KindGemini:
			return gemini.New(ctx, gemini.Config{
				APIKey: s.APIKey, BaseURL: s.BaseURL, Model: s.Model, Voice: s.Voice,
				GoogleSearch: o.GoogleSearch, Transport: o.GeminiTransport,
			})
		case KindOpenAIRealtime:
			return openairt.New(openairt.Config{
				BaseURL: s.BaseURL, APIKey: s.APIKey, Model: s.Model, Voice: s.Voice,
				HTTPClient: o.HTTPClient, Logger: o.Logger,
			})
		case KindOpenAI:
			rec, syn, err := o.speech(ctx, s, all)
			if err != nil {
				return nil, err
			}
			return openaichat.New(openaichat.Config{
				BaseURL: s.BaseURL, APIKey: s.APIKey, Model: s.Model, Label: s.ID,
				Recognizer: rec, Synthesizer: syn, HTTPClient: o.HTTPClient, Logger: o.Logger,
			})
		case KindAnthropic:
			rec, syn, err := o.speech(ctx, s, all)
			if err != nil {
				return nil, err
			}
			return claude.New(claude.Config{
				APIKey: s.APIKey, BaseURL: s.BaseURL, Model: s.Model, Effort: o.ClaudeEffort,
				WebSearch: o.GoogleSearch, Recognizer: rec, Synthesizer: syn,
				HTTPClient: o.HTTPClient, Logger: o.Logger,
			})
		}
		return nil, fmt.Errorf("неизвестный тип провайдера %q", s.Kind)
	}
}

// voiced is the speech used by a pipeline provider.
type voiced interface {
	speech.Recognizer
	speech.Synthesizer
	SetVoice(string)
}

// speech returns speech recognition and synthesis for the pipeline spec s.
// Every provider gets its own instance, so their voices are independent.
func (o BuildOptions) speech(ctx context.Context, s Spec, all []Spec) (speech.Recognizer, speech.Synthesizer, error) {
	var sp voiced
	switch src := s.Speech; src {
	case "":
		g, err := o.gemini(ctx, o.GeminiAPIKey, "")
		if err != nil {
			return nil, nil, fmt.Errorf("речь по умолчанию (Gemini): %w", err)
		}
		sp = g
	case SpeechSelf:
		sp = o.openai(s.BaseURL, s.APIKey)
	default:
		var from Spec
		for _, a := range all {
			if a.ID == src {
				from = a
			}
		}
		switch from.Kind {
		case KindGemini:
			g, err := o.gemini(ctx, from.APIKey, from.BaseURL)
			if err != nil {
				return nil, nil, err
			}
			sp = g
		case KindOpenAI, KindOpenAIRealtime:
			sp = o.openai(httpURL(from.BaseURL), from.APIKey)
		default:
			return nil, nil, fmt.Errorf("провайдер %q не умеет распознавать речь", src)
		}
	}
	if sp == nil {
		return nil, nil, errors.New("нет распознавания речи")
	}
	if s.Voice != "" {
		sp.SetVoice(s.Voice)
	}
	return sp, sp, nil
}

func (o BuildOptions) gemini(ctx context.Context, key, baseURL string) (*geminispeech.Speech, error) {
	return geminispeech.New(ctx, geminispeech.Config{APIKey: key, BaseURL: baseURL, Transport: o.GeminiTransport})
}

func (o BuildOptions) openai(baseURL, key string) *openaispeech.Speech {
	return openaispeech.New(openaispeech.Config{BaseURL: baseURL, APIKey: key, HTTPClient: o.HTTPClient})
}

// httpURL turns the WebSocket URL of a realtime provider into the REST one.
func httpURL(u string) string {
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}
