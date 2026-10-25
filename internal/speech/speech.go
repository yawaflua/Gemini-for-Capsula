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

// Package speech defines speech recognition and synthesis interfaces used
// by text-only model providers (see provider/claude). Implementations live
// in subpackages.
package speech

import (
	"context"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Recognizer converts speech to text.
type Recognizer interface {
	// Transcribe returns the text spoken in pcm, or "" if there is no speech.
	// language is a BCP-47 hint, e.g. "ru-RU".
	Transcribe(ctx context.Context, pcm []byte, f models.AudioFormat, language string) (string, error)
}

// Synthesizer converts text to speech.
type Synthesizer interface {
	// Format is the format of synthesized audio.
	Format() models.AudioFormat
	// Synthesize speaks text. language is a BCP-47 hint.
	Synthesize(ctx context.Context, text, language string) ([]byte, error)
}
