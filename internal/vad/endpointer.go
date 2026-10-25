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

// Package vad implements a lightweight energy-based voice activity detector
// and utterance endpointer suitable for a low-power speaker SoC.
package vad

import (
	"math"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
)

// Config tunes the [Endpointer].
type Config struct {
	// ThresholdDB is how far above the noise floor a frame must be to count
	// as speech.
	ThresholdDB float64
	// MinSpeechDBFS is an absolute floor below which nothing is speech.
	MinSpeechDBFS float64
	// SpeechStart is how much continuous speech starts an utterance.
	SpeechStart time.Duration
	// SilenceEnd is how much trailing silence ends an utterance.
	SilenceEnd time.Duration
	// NoSpeechTimeout gives up if the user does not start speaking.
	NoSpeechTimeout time.Duration
	// MaxUtterance bounds the length of an utterance.
	MaxUtterance time.Duration
}

// DefaultConfig returns settings that work for a typical room.
func DefaultConfig() Config {
	return Config{
		ThresholdDB:     12,
		MinSpeechDBFS:   -50,
		SpeechStart:     100 * time.Millisecond,
		SilenceEnd:      900 * time.Millisecond,
		NoSpeechTimeout: 6 * time.Second,
		MaxUtterance:    20 * time.Second,
	}
}

// Decision is the outcome of [Endpointer.Process].
type Decision int

const (
	// Waiting means no speech has been detected yet.
	Waiting Decision = iota
	// SpeechStarted is returned once, on the frame where speech begins.
	SpeechStarted
	// Speaking means the utterance is in progress.
	Speaking
	// EndOfUtterance means the user stopped speaking.
	EndOfUtterance
	// NoSpeech means the user never started speaking.
	NoSpeech
)

func (d Decision) String() string {
	return [...]string{"waiting", "speech_started", "speaking", "end_of_utterance", "no_speech"}[d]
}

// Endpointer detects the start and end of a single utterance.
//
// The noise floor is tracked continuously with [Endpointer.Observe] while the
// device is idle, so the detector adapts to the room before a turn begins.
type Endpointer struct {
	cfg      Config
	floor    float64               // dBFS
	samples  int                   // frames observed, for the initial estimate
	window   [smoothFrames]float64 // recent frame levels as linear power
	wpos     int
	wlen     int
	noSpeech time.Duration

	elapsed   time.Duration
	speech    time.Duration // continuous speech
	silence   time.Duration // continuous silence after speech
	started   bool
	completed bool
}

// smoothFrames is how many recent frames are averaged to classify speech, so
// short noise bursts and gaps between syllables do not flip the decision.
const smoothFrames = 5

// New creates an endpointer.
func New(cfg Config) *Endpointer {
	return &Endpointer{cfg: cfg, floor: -60, noSpeech: cfg.NoSpeechTimeout}
}

// Observe updates the noise floor estimate from background audio.
//
// The floor is a slow moving average of the frame level in dB. Frames that
// look like speech are skipped so talking does not drag it up. (A fast
// downward attack would converge on the quietest frames instead of the
// typical noise level, and then ordinary room noise counts as speech and
// utterances never end.)
func (e *Endpointer) Observe(frame []byte) {
	db := audio.DBFS(audio.RMS(frame))
	if math.IsInf(db, -1) || db < -100 {
		db = -100
	}
	if e.samples < 25 {
		// Converge quickly after start.
		e.samples++
		e.floor += (db - e.floor) / float64(e.samples)
		return
	}
	if db > e.floor+e.cfg.ThresholdDB {
		return
	}
	e.floor += (db - e.floor) * 0.02
}

// Reset prepares for a new utterance, keeping the noise floor.
// noSpeechTimeout overrides Config.NoSpeechTimeout for this utterance if
// non-zero.
func (e *Endpointer) Reset(noSpeechTimeout time.Duration) {
	e.elapsed, e.speech, e.silence = 0, 0, 0
	e.started, e.completed = false, false
	e.wpos, e.wlen = 0, 0
	e.noSpeech = e.cfg.NoSpeechTimeout
	if noSpeechTimeout > 0 {
		e.noSpeech = noSpeechTimeout
	}
}

// NoiseFloor returns the current noise floor estimate in dBFS.
func (e *Endpointer) NoiseFloor() float64 { return e.floor }

// IsSpeech classifies a single frame.
func (e *Endpointer) IsSpeech(frame []byte) bool {
	return e.isSpeech(audio.DBFS(audio.RMS(frame)))
}

func (e *Endpointer) isSpeech(db float64) bool {
	return db > e.cfg.MinSpeechDBFS && db > e.floor+e.cfg.ThresholdDB
}

// smoothed adds frame to the window and returns the average level in dBFS.
func (e *Endpointer) smoothed(frame []byte) float64 {
	r := audio.RMS(frame)
	e.window[e.wpos] = r * r
	e.wpos = (e.wpos + 1) % smoothFrames
	e.wlen = min(e.wlen+1, smoothFrames)
	var sum float64
	for _, p := range e.window[:e.wlen] {
		sum += p
	}
	return audio.DBFS(math.Sqrt(sum / float64(e.wlen)))
}

// Process consumes a frame of duration d and returns the utterance state.
func (e *Endpointer) Process(frame []byte, d time.Duration) Decision {
	if e.completed {
		return EndOfUtterance
	}
	e.elapsed += d
	speech := e.isSpeech(e.smoothed(frame))
	if !speech && !e.started {
		e.Observe(frame)
	}

	if !e.started {
		if speech {
			e.speech += d
		} else {
			e.speech = 0
		}
		if e.speech >= e.cfg.SpeechStart {
			e.started = true
			e.elapsed = e.speech
			return SpeechStarted
		}
		if e.elapsed >= e.noSpeech {
			e.completed = true
			return NoSpeech
		}
		return Waiting
	}

	if speech {
		e.silence = 0
	} else {
		e.silence += d
		e.Observe(frame)
	}
	if e.silence >= e.cfg.SilenceEnd || e.elapsed >= e.cfg.MaxUtterance {
		e.completed = true
		return EndOfUtterance
	}
	return Speaking
}
