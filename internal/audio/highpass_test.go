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
	"testing"
)

func tone(freq, rate float64, n int) []byte {
	pcm := make([]byte, n*2)
	for i := range n {
		v := int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return pcm
}

func TestHighPassKeepsSpeechAndDropsRumble(t *testing.T) {
	const rate = 16000
	rumble := NewHighPass(180, rate)
	speech := NewHighPass(180, rate)
	low, high := tone(50, rate, 1600), tone(1000, rate, 1600)
	rumble.Process(low)
	speech.Process(high)
	if got, want := RMS(low), 12000.0/32768.0/math.Sqrt2; got > want*0.5 {
		t.Errorf("50 Hz survived the filter: rms %.3f, want well below %.3f", got, want*0.5)
	}
	if got, want := RMS(high), 12000.0/32768.0/math.Sqrt2; got < want*0.85 {
		t.Errorf("1 kHz was attenuated: rms %.3f, want about %.3f", got, want)
	}
}
