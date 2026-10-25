// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vad

import (
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

var (
	format  = models.DeviceInputFormat
	frameD  = 20 * time.Millisecond
	silence = make([]byte, format.FrameSize(frameD))
	speech  = audio.Tone(format, 300, 20, 0.3)
)

func feed(e *Endpointer, frame []byte, d time.Duration) Decision {
	var last Decision
	for t := time.Duration(0); t < d; t += frameD {
		last = e.Process(frame, frameD)
		if last == SpeechStarted || last == EndOfUtterance || last == NoSpeech {
			return last
		}
	}
	return last
}

func TestEndpointerUtterance(t *testing.T) {
	e := New(DefaultConfig())
	for i := 0; i < 50; i++ {
		e.Observe(silence)
	}
	e.Reset(0)
	if got := feed(e, silence, 500*time.Millisecond); got != Waiting {
		t.Fatalf("silence: got %v, want waiting", got)
	}
	if got := feed(e, speech, time.Second); got != SpeechStarted {
		t.Fatalf("speech onset: got %v, want speech_started", got)
	}
	if got := feed(e, speech, 500*time.Millisecond); got != Speaking {
		t.Fatalf("speech: got %v, want speaking", got)
	}
	if got := feed(e, silence, 2*time.Second); got != EndOfUtterance {
		t.Fatalf("trailing silence: got %v, want end_of_utterance", got)
	}
}

func TestEndpointerNoSpeech(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(time.Second)
	if got := feed(e, silence, 2*time.Second); got != NoSpeech {
		t.Fatalf("got %v, want no_speech", got)
	}
}

func TestEndpointerMaxUtterance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxUtterance = time.Second
	e := New(cfg)
	e.Reset(0)
	feed(e, speech, 200*time.Millisecond)
	if got := feed(e, speech, 3*time.Second); got != EndOfUtterance {
		t.Fatalf("got %v, want end_of_utterance", got)
	}
}

// noise returns a frame of uniform noise at roughly the given amplitude.
func noise(seed, amp int) []byte {
	b := make([]byte, format.FrameSize(frameD))
	x := uint32(seed*2654435761 + 1)
	for i := 0; i+1 < len(b); i += 2 {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		v := int16(int(x%uint32(2*amp+1)) - amp)
		b[i], b[i+1] = byte(v), byte(uint16(v)>>8)
	}
	return b
}

// Regression: with fluctuating room noise the floor used to converge on the
// quietest frames, so the noise itself counted as speech and utterances only
// ended at MaxUtterance.
func TestEndpointerFluctuatingNoise(t *testing.T) {
	e := New(DefaultConfig())
	amps := []int{60, 200, 30, 400, 90, 150, 20, 300}
	frame := func(i int) []byte { return noise(i, amps[i%len(amps)]) }
	for i := 0; i < 500; i++ {
		e.Observe(frame(i))
	}
	e.Reset(0)
	if got := feed(e, speech, time.Second); got != SpeechStarted {
		t.Fatalf("speech onset: got %v (floor %.1f dBFS)", got, e.NoiseFloor())
	}
	feed(e, speech, time.Second)
	for i := 0; i < 100; i++ { // 2 s of room noise
		if got := e.Process(frame(i), frameD); got == EndOfUtterance {
			return
		}
	}
	t.Fatalf("utterance did not end within 2s of noise (floor %.1f dBFS)", e.NoiseFloor())
}
