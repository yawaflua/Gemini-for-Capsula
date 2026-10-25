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

// Package geminispeech implements speech recognition and synthesis with the
// Gemini API: audio understanding for transcription and the native TTS
// models for speech.
package geminispeech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/speech"
)

// Default models.
const (
	DefaultSTTModel = "gemini-2.5-flash"
	DefaultTTSModel = "gemini-2.5-flash-preview-tts"
)

// Config configures [Speech].
type Config struct {
	// APIKey for the Gemini Developer API; empty uses $GEMINI_API_KEY.
	APIKey string
	// STTModel and TTSModel default to [DefaultSTTModel] and [DefaultTTSModel].
	STTModel string
	TTSModel string
	// Voice is a prebuilt voice name, e.g. "Kore".
	Voice string
}

// Speech is a Gemini backed [speech.Recognizer] and [speech.Synthesizer].
type Speech struct {
	cfg    Config
	client *genai.Client

	mu    sync.RWMutex
	voice string
}

var (
	_ speech.Recognizer  = (*Speech)(nil)
	_ speech.Synthesizer = (*Speech)(nil)
)

// New creates a client.
func New(ctx context.Context, cfg Config) (*Speech, error) {
	if cfg.STTModel == "" {
		cfg.STTModel = DefaultSTTModel
	}
	if cfg.TTSModel == "" {
		cfg.TTSModel = DefaultTTSModel
	}
	c, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: cfg.APIKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return nil, fmt.Errorf("geminispeech: %w", err)
	}
	return &Speech{cfg: cfg, client: c, voice: cfg.Voice}, nil
}

// Voice returns the prebuilt voice used for synthesis.
func (s *Speech) Voice() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.voice
}

// SetVoice changes the voice of later synthesis requests.
func (s *Speech) SetVoice(v string) {
	s.mu.Lock()
	s.voice = v
	s.mu.Unlock()
}

// noSpeech is what the recognizer is told to answer for silence or noise.
const noSpeech = "<none>"

// Transcribe implements [speech.Recognizer].
func (s *Speech) Transcribe(ctx context.Context, pcm []byte, f models.AudioFormat, language string) (string, error) {
	prompt := "Transcribe the speech in this recording verbatim, in its original language" +
		" (most likely " + language + "). Output only the transcript, without quotes or comments." +
		" If there is no intelligible speech, output exactly " + noSpeech + "."
	resp, err := s.client.Models.GenerateContent(ctx, s.cfg.STTModel, []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{Text: prompt},
			{InlineData: &genai.Blob{MIMEType: "audio/wav", Data: WAV(pcm, f)}},
		},
	}}, &genai.GenerateContentConfig{
		Temperature:    genai.Ptr[float32](0),
		ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)},
	})
	if err != nil {
		return "", fmt.Errorf("geminispeech: transcribe: %w", err)
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" || strings.Contains(text, noSpeech) {
		return "", nil
	}
	return text, nil
}

// Format implements [speech.Synthesizer]: the TTS models produce 24 kHz mono
// LINEAR16.
func (s *Speech) Format() models.AudioFormat { return models.DefaultOutputFormat }

// Synthesize implements [speech.Synthesizer].
func (s *Speech) Synthesize(ctx context.Context, text, language string) ([]byte, error) {
	cfg := &genai.GenerateContentConfig{
		ResponseModalities: []string{"AUDIO"},
		SpeechConfig:       &genai.SpeechConfig{LanguageCode: language},
	}
	if voice := s.Voice(); voice != "" {
		cfg.SpeechConfig.VoiceConfig = &genai.VoiceConfig{
			PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: voice},
		}
	}
	resp, err := s.client.Models.GenerateContent(ctx, s.cfg.TTSModel, genai.Text(text), cfg)
	if err != nil {
		return nil, fmt.Errorf("geminispeech: synthesize: %w", err)
	}
	var out []byte
	for _, c := range resp.Candidates {
		if c.Content == nil {
			continue
		}
		for _, p := range c.Content.Parts {
			if p.InlineData != nil {
				out = append(out, p.InlineData.Data...)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("geminispeech: no audio in response")
	}
	return out, nil
}

// WAV wraps LINEAR16 PCM in a RIFF/WAVE header.
func WAV(pcm []byte, f models.AudioFormat) []byte {
	var b bytes.Buffer
	ch, rate := uint16(f.Channels), uint32(f.SampleRateHertz)
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(ch)
	w(rate)
	w(rate * uint32(ch) * 2)
	w(ch * 2)
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
