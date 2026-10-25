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

// Package gemini implements [provider.Provider] on top of the Gemini Live API
// (bidirectional audio streaming with native speech output and function
// calling).
package gemini

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/genai"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// DefaultModel is a Live API model with native audio output. The
// 09-2025 preview often ends its turn silently after a function result
// (with a longer system prompt), so time, weather or alarms got no answer.
const DefaultModel = "gemini-2.5-flash-native-audio-latest"

// Config configures the Gemini provider.
type Config struct {
	// APIKey authenticates against the Gemini Developer API. If empty, the
	// GEMINI_API_KEY / GOOGLE_API_KEY environment variables are used.
	APIKey string
	// VertexAI selects the Vertex AI backend instead of the Developer API.
	VertexAI bool
	// Project and Location are used with VertexAI.
	Project  string
	Location string

	// Model is the Live API model name. Defaults to [DefaultModel].
	Model string
	// Voice is a prebuilt voice name, e.g. "Kore", "Puck", "Charon".
	Voice string
	// GoogleSearch enables grounding with Google Search.
	GoogleSearch bool
}

// Provider is a Gemini Live API backed [provider.Provider].
type Provider struct {
	cfg    Config
	client *genai.Client

	mu    sync.RWMutex // guards model and voice, see Tune
	model string
	voice string
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Tuner    = (*Provider)(nil)
)

// New creates a provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	cc := &genai.ClientConfig{APIKey: cfg.APIKey, Backend: genai.BackendGeminiAPI}
	cc.HTTPOptions.APIVersion = "v1beta"
	if cfg.VertexAI {
		cc = &genai.ClientConfig{Backend: genai.BackendVertexAI, Project: cfg.Project, Location: cfg.Location}
		cc.HTTPOptions.APIVersion = "v1beta1"
	}
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}
	return &Provider{cfg: cfg, client: client, model: cfg.Model, voice: cfg.Voice}, nil
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

// SupportsServerVAD implements [provider.VADDetector].
func (p *Provider) SupportsServerVAD() bool { return true }

// AcceptsText implements [provider.TextProvider].
func (p *Provider) AcceptsText() bool { return true }

// Name implements [provider.Provider].
func (p *Provider) Name() string { return "gemini/" + p.Tuning().Model }

// OutputFormat implements [provider.Provider]. The Live API always produces
// 24 kHz mono LINEAR16.
func (p *Provider) OutputFormat() models.AudioFormat { return models.DefaultOutputFormat }

// Connect implements [provider.Provider].
func (p *Provider) Connect(ctx context.Context, opts provider.SessionOptions) (provider.Session, error) {
	if err := opts.InputFormat.Validate(); err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	t := p.Tuning()
	gs, err := p.client.Live.Connect(ctx, t.Model, p.liveConfig(opts, t.Voice))
	if err != nil {
		return nil, fmt.Errorf("gemini: connect: %w", err)
	}
	s := newSession(gs, opts.InputFormat, opts.ServerVAD)
	go s.receive()
	return s, nil
}

func (p *Provider) liveConfig(opts provider.SessionOptions, voice string) *genai.LiveConnectConfig {
	// By default the device runs its own endpointer and marks turn
	// boundaries explicitly. With ServerVAD Gemini detects them itself,
	// which is far more robust in a noisy room.
	aad := &genai.AutomaticActivityDetection{Disabled: true}
	if opts.ServerVAD {
		aad = &genai.AutomaticActivityDetection{
			EndOfSpeechSensitivity: genai.EndSensitivityHigh,
			PrefixPaddingMs:        genai.Ptr[int32](100),
			SilenceDurationMs:      genai.Ptr[int32](600),
		}
	}
	cfg := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		RealtimeInputConfig: &genai.RealtimeInputConfig{
			AutomaticActivityDetection: aad,
			ActivityHandling:           genai.ActivityHandlingStartOfActivityInterrupts,
		},
		InputAudioTranscription:  transcription(opts.Language, opts.ExtraLanguages),
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
		ContextWindowCompression: &genai.ContextWindowCompressionConfig{
			SlidingWindow: &genai.SlidingWindow{},
		},
	}
	if opts.SystemInstruction != "" {
		cfg.SystemInstruction = genai.NewContentFromText(opts.SystemInstruction, genai.RoleUser)
	}
	if voice != "" || opts.Language != "" {
		cfg.SpeechConfig = &genai.SpeechConfig{LanguageCode: opts.Language}
		if voice != "" {
			cfg.SpeechConfig.VoiceConfig = &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: voice},
			}
		}
	}
	if decls := declarations(opts.Tools); len(decls) > 0 {
		cfg.Tools = append(cfg.Tools, &genai.Tool{FunctionDeclarations: decls})
	}
	if p.cfg.GoogleSearch {
		cfg.Tools = append(cfg.Tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	return cfg
}

// transcription configures input transcription with language hints, so a
// short Russian question is not transcribed as Hebrew, and a Hebrew one is
// not forced into Russian (or Latin gibberish) when Hebrew is expected too.
func transcription(lang string, extra []string) *genai.AudioTranscriptionConfig {
	if lang == "" {
		return &genai.AudioTranscriptionConfig{}
	}
	return &genai.AudioTranscriptionConfig{LanguageCodes: append([]string{lang}, extra...)}
}

// declarations converts provider-agnostic tools into Gemini declarations.
func declarations(ts []tools.Tool) []*genai.FunctionDeclaration {
	out := make([]*genai.FunctionDeclaration, 0, len(ts))
	for _, t := range ts {
		d := &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description}
		if len(t.Parameters) > 0 {
			schema := &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{}}
			for _, p := range t.Parameters {
				schema.Properties[p.Name] = &genai.Schema{
					Type:        schemaType(p.Type),
					Description: p.Description,
					Enum:        p.Enum,
					Minimum:     p.Minimum,
					Maximum:     p.Maximum,
				}
				if p.Required {
					schema.Required = append(schema.Required, p.Name)
				}
			}
			d.Parameters = schema
		}
		out = append(out, d)
	}
	return out
}

func schemaType(t tools.Type) genai.Type {
	switch t {
	case tools.TypeInteger:
		return genai.TypeInteger
	case tools.TypeBoolean:
		return genai.TypeBoolean
	default:
		return genai.TypeString
	}
}

// errFromClose is used to tell an intentional Close from a backend failure.
var errFromClose = errors.New("closed by client")
