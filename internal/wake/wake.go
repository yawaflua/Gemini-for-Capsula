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

// Package wake is the client of the wake word spotter service
// (deploy/wake), which listens for configurable keywords in the idle audio
// of speakers on behalf of the server.
//
// Protocol, over WebSocket:
//
//   - client → spotter, text: {"keywords": ["антон", "אנטון"], "sample_rate": 16000}
//     first, and again whenever the keywords change;
//   - client → spotter, binary: 16-bit little-endian mono PCM;
//   - spotter → client, text: {"keyword": "антон", "engine": "vosk"} on a
//     detection, or {"ready": true, "engines": {...}, "unsupported": [...]}
//     after each configuration.
package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// Detection is one heard keyword.
type Detection struct {
	Keyword string  `json:"keyword"`
	Engine  string  `json:"engine,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// Status is the spotter's answer to a configuration.
type Status struct {
	Engines map[string]string `json:"engines,omitempty"`
	// Unsupported lists keywords no engine can spot (e.g. a word missing
	// from the Vosk vocabulary).
	Unsupported []string `json:"unsupported,omitempty"`
}

type config struct {
	Keywords   []string `json:"keywords"`
	SampleRate int      `json:"sample_rate"`
}

type reply struct {
	Detection
	Ready bool `json:"ready,omitempty"`
	Status
	Error string `json:"error,omitempty"`
}

// Stream is one audio stream to the spotter.
type Stream struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	det    chan Detection
	status chan Status
	done   chan struct{}
	err    error
}

// Dial connects to the spotter at url and configures keywords for audio at
// sampleRate.
func Dial(ctx context.Context, url string, keywords []string, sampleRate int) (*Stream, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("wake: dial %s: %w", url, err)
	}
	sctx, scancel := context.WithCancel(ctx)
	s := &Stream{
		ws: ws, ctx: sctx, cancel: scancel,
		det:    make(chan Detection, 4),
		status: make(chan Status, 1),
		done:   make(chan struct{}),
	}
	if err := s.Configure(keywords, sampleRate); err != nil {
		s.Close()
		return nil, err
	}
	go s.read()
	return s, nil
}

// Configure replaces the keywords.
func (s *Stream) Configure(keywords []string, sampleRate int) error {
	b, _ := json.Marshal(config{Keywords: keywords, SampleRate: sampleRate})
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, b)
}

// Write sends a chunk of audio. It gives up quickly rather than delaying the
// caller: the spotter only needs most of the audio.
func (s *Stream) Write(pcm []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageBinary, pcm)
}

// Detections delivers heard keywords until the stream ends.
func (s *Stream) Detections() <-chan Detection { return s.det }

// Status delivers the spotter's answer to the latest configuration.
func (s *Stream) Status() <-chan Status { return s.status }

// Done is closed when the stream ends; Err then reports why.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err returns the error that ended the stream.
func (s *Stream) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close ends the stream.
func (s *Stream) Close() error {
	s.cancel()
	return s.ws.Close(websocket.StatusNormalClosure, "bye")
}

func (s *Stream) read() {
	defer close(s.done)
	defer s.cancel()
	for {
		typ, b, err := s.ws.Read(s.ctx)
		if err != nil {
			s.err = err
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var r reply
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		switch {
		case r.Error != "":
			s.err = errors.New("wake: spotter: " + r.Error)
			s.ws.Close(websocket.StatusNormalClosure, "error")
			return
		case r.Ready:
			select {
			case <-s.status:
			default:
			}
			s.status <- r.Status
		case r.Keyword != "":
			select {
			case s.det <- r.Detection:
			default: // a detection is already pending
			}
		}
	}
}

// MaxKeywords bounds the keyword list.
const MaxKeywords = 10

// ParseKeywords splits a comma separated list, as entered in the web panel,
// into normalized keywords: lower case, trimmed, without duplicates.
func ParseKeywords(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		kw := strings.ToLower(strings.Join(strings.Fields(part), " "))
		if kw == "" || seen[kw] {
			continue
		}
		if n := utf8.RuneCountInString(kw); n < 2 || n > 40 {
			return nil, fmt.Errorf("ключевое слово %q должно быть от 2 до 40 символов", kw)
		}
		for _, r := range kw {
			if !unicode.IsLetter(r) && !unicode.Is(unicode.Mn, r) && r != ' ' && r != '-' && r != '\'' {
				return nil, fmt.Errorf("в ключевом слове %q допустимы только буквы, пробел, дефис и апостроф", kw)
			}
		}
		seen[kw] = true
		out = append(out, kw)
	}
	if len(out) > MaxKeywords {
		return nil, fmt.Errorf("не больше %d ключевых слов", MaxKeywords)
	}
	return out, nil
}
