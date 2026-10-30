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

// Package openaispeech implements [speech.Recognizer] and
// [speech.Synthesizer] with the audio endpoints of the OpenAI API
// (/audio/transcriptions and /audio/speech), or of any server compatible
// with them (LocalAI, Speaches, ...).
package openaispeech

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/speech"
)

// Defaults.
const (
	DefaultBaseURL  = "https://api.openai.com/v1"
	DefaultSTTModel = "gpt-4o-mini-transcribe"
	DefaultTTSModel = "gpt-4o-mini-tts"
	DefaultVoice    = "coral"
)

// voices are the voices of the OpenAI TTS models.
var voices = []string{"alloy", "ash", "ballad", "cedar", "coral", "echo", "fable", "marin", "nova", "onyx", "sage", "shimmer", "verse"}

// Format is the format of "pcm" responses of /audio/speech.
var Format = models.AudioFormat{Encoding: models.EncodingLinear16, SampleRateHertz: 24000, Channels: 1}

// Config configures [Speech].
type Config struct {
	// BaseURL defaults to [DefaultBaseURL].
	BaseURL string
	APIKey  string
	// STTModel and TTSModel default to [DefaultSTTModel] and [DefaultTTSModel].
	STTModel string
	TTSModel string
	// Voice defaults to [DefaultVoice].
	Voice string
	// Instructions steer the speaking style (gpt-4o-mini-tts only).
	Instructions string
	// HTTPClient sends the requests; nil uses a client with a 60 s timeout.
	HTTPClient *http.Client
}

// Speech recognizes and synthesizes speech.
type Speech struct {
	cfg  Config
	http *http.Client

	mu    sync.RWMutex
	voice string
}

var (
	_ speech.Recognizer  = (*Speech)(nil)
	_ speech.Synthesizer = (*Speech)(nil)
)

// New returns a [Speech].
func New(cfg Config) *Speech {
	cfg.BaseURL = strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultBaseURL), "/")
	cfg.STTModel = cmp.Or(cfg.STTModel, DefaultSTTModel)
	cfg.TTSModel = cmp.Or(cfg.TTSModel, DefaultTTSModel)
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Speech{cfg: cfg, http: hc, voice: cmp.Or(cfg.Voice, DefaultVoice)}
}

// Voice returns the voice used for synthesis.
func (s *Speech) Voice() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.voice
}

// SetVoice changes the voice used for synthesis.
func (s *Speech) SetVoice(v string) {
	s.mu.Lock()
	s.voice = v
	s.mu.Unlock()
}

// Voices lists the voices of the OpenAI TTS models.
func (s *Speech) Voices() []string { return voices }

// Transcribe implements [speech.Recognizer].
func (s *Speech) Transcribe(ctx context.Context, pcm []byte, f models.AudioFormat, language string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		return "", err
	}
	fw.Write(audio.WAV(pcm, f))
	mw.WriteField("model", s.cfg.STTModel)
	mw.WriteField("response_format", "json")
	if lang := isoLanguage(language); lang != "" {
		mw.WriteField("language", lang)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := s.do(ctx, "/audio/transcriptions", mw.FormDataContentType(), &body, func(r io.Reader) error {
		return json.NewDecoder(r).Decode(&out)
	}); err != nil {
		return "", fmt.Errorf("openaispeech: transcribe: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// Format implements [speech.Synthesizer].
func (s *Speech) Format() models.AudioFormat { return Format }

// Synthesize implements [speech.Synthesizer].
func (s *Speech) Synthesize(ctx context.Context, text, _ string) ([]byte, error) {
	req := map[string]any{
		"model":           s.cfg.TTSModel,
		"input":           text,
		"voice":           s.Voice(),
		"response_format": "pcm",
	}
	if s.cfg.Instructions != "" {
		req["instructions"] = s.cfg.Instructions
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var pcm []byte
	if err := s.do(ctx, "/audio/speech", "application/json", bytes.NewReader(b), func(r io.Reader) error {
		pcm, err = io.ReadAll(r)
		return err
	}); err != nil {
		return nil, fmt.Errorf("openaispeech: synthesize: %w", err)
	}
	if len(pcm) == 0 {
		return nil, errors.New("openaispeech: no audio in response")
	}
	return pcm[:len(pcm)&^1], nil
}

func (s *Speech) do(ctx context.Context, path, contentType string, body io.Reader, read func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return APIError(resp)
	}
	return read(resp.Body)
}

// APIError describes a failed response of an OpenAI compatible API.
func APIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error.Message)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// isoLanguage turns a BCP-47 tag into the ISO-639-1 code the API expects.
func isoLanguage(tag string) string {
	lang, _, _ := strings.Cut(tag, "-")
	if len(lang) != 2 {
		return ""
	}
	return strings.ToLower(lang)
}
