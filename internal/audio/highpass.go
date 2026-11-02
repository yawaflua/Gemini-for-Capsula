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

package audio

import (
	"encoding/binary"
	"math"
)

// HighPass is a one-pole high-pass filter for 16-bit little-endian mono PCM.
// Rooms bring fan and mains rumble below ~200 Hz, and the endpointer compares
// speech with the noise floor: without the filter quiet speech can sit only a
// few dB above the rumble and never count as speech.
type HighPass struct {
	a      float64
	x1, y1 float64
}

// NewHighPass returns a filter with the given cutoff in Hz.
func NewHighPass(cutoffHz, sampleRate float64) *HighPass {
	if cutoffHz <= 0 || sampleRate <= 0 || cutoffHz >= sampleRate/2 {
		return &HighPass{a: 1}
	}
	return &HighPass{a: 1 / (1 + 2*math.Pi*cutoffHz/sampleRate)}
}

// Process filters pcm in place.
func (h *HighPass) Process(pcm []byte) {
	if h == nil {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		x := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		y := h.a * (h.y1 + x - h.x1)
		h.x1, h.y1 = x, y
		switch {
		case y > math.MaxInt16:
			y = math.MaxInt16
		case y < math.MinInt16:
			y = math.MinInt16
		}
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(math.Round(y))))
	}
}
