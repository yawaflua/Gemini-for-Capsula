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

// Package audio defines the audio I/O abstractions used by the device agent
// and pure-Go PCM helpers. Concrete backends live in subpackages
// (see audio/alsa).
package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Capturer continuously records microphone audio.
type Capturer interface {
	// Format is the format of captured frames.
	Format() models.AudioFormat
	// Run records until ctx is done, delivering fixed-size frames to out.
	// Frames are dropped if out is full. Run does not close out.
	Run(ctx context.Context, out chan<- []byte) error
}

// Player opens playback streams.
type Player interface {
	// Open starts a new playback stream in the given format.
	Open(ctx context.Context, f models.AudioFormat) (Stream, error)
}

// Stream is a single playback stream.
type Stream interface {
	// Write queues PCM audio. It never blocks on the audio device.
	Write(pcm []byte) error
	// Drain waits until all queued audio is played and releases the stream.
	Drain() error
	// Abort stops playback immediately and releases the stream.
	Abort() error
}

// RMS returns the root-mean-square level of LINEAR16 PCM normalized to
// [0, 1].
func RMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// DBFS converts a normalized RMS level to decibels relative to full scale.
func DBFS(rms float64) float64 {
	if rms <= 1e-9 {
		return -180
	}
	return 20 * math.Log10(rms)
}

// Tone synthesizes a sine tone with short fades to avoid clicks.
func Tone(f models.AudioFormat, freqHz float64, durMs int, gain float64) []byte {
	frames := f.SampleRateHertz * durMs / 1000
	fade := min(frames/4, f.SampleRateHertz/200) // up to 5 ms
	out := make([]byte, frames*f.Channels*2)
	for i := 0; i < frames; i++ {
		env := 1.0
		if i < fade {
			env = float64(i) / float64(fade)
		} else if i > frames-fade {
			env = float64(frames-i) / float64(fade)
		}
		v := int16(gain * env * 32767 * math.Sin(2*math.Pi*freqHz*float64(i)/float64(f.SampleRateHertz)))
		for c := 0; c < f.Channels; c++ {
			binary.LittleEndian.PutUint16(out[(i*f.Channels+c)*2:], uint16(v))
		}
	}
	return out
}

// Silence returns durMs of silence.
func Silence(f models.AudioFormat, durMs int) []byte {
	return make([]byte, f.FrameSize(time.Duration(durMs)*time.Millisecond))
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
