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

package models

import (
	"fmt"
	"time"
)

// Encoding identifies a raw audio sample encoding.
type Encoding string

const (
	// EncodingLinear16 is signed 16-bit little-endian PCM.
	EncodingLinear16 Encoding = "LINEAR16"
)

// AudioFormat describes a raw PCM stream.
type AudioFormat struct {
	Encoding        Encoding `json:"encoding"`
	SampleRateHertz int      `json:"sample_rate_hertz"`
	Channels        int      `json:"channels"`
}

var (
	// DeviceInputFormat is the format of microphone audio sent by the device.
	DeviceInputFormat = AudioFormat{Encoding: EncodingLinear16, SampleRateHertz: 16000, Channels: 1}

	// DefaultOutputFormat is the format of synthesized speech produced by the
	// Gemini Live API.
	DefaultOutputFormat = AudioFormat{Encoding: EncodingLinear16, SampleRateHertz: 24000, Channels: 1}
)

// BytesPerSample returns the size of a single sample of a single channel.
func (f AudioFormat) BytesPerSample() int { return 2 }

// BytesPerSecond returns the byte rate of the stream.
func (f AudioFormat) BytesPerSecond() int {
	return f.SampleRateHertz * f.Channels * f.BytesPerSample()
}

// FrameSize returns the number of bytes covering d of audio, aligned to a
// whole sample frame.
func (f AudioFormat) FrameSize(d time.Duration) int {
	frames := int(int64(f.SampleRateHertz) * int64(d) / int64(time.Second))
	return frames * f.Channels * f.BytesPerSample()
}

// Duration returns the playback duration of n bytes of audio.
func (f AudioFormat) Duration(n int) time.Duration {
	bps := f.BytesPerSecond()
	if bps == 0 {
		return 0
	}
	return time.Duration(int64(n) * int64(time.Second) / int64(bps))
}

// MIMEType returns the MIME type understood by the Gemini API.
func (f AudioFormat) MIMEType() string {
	return fmt.Sprintf("audio/pcm;rate=%d", f.SampleRateHertz)
}

// Validate reports whether the format is supported.
func (f AudioFormat) Validate() error {
	if f.Encoding != EncodingLinear16 {
		return fmt.Errorf("unsupported encoding %q", f.Encoding)
	}
	if f.SampleRateHertz <= 0 || f.Channels <= 0 {
		return fmt.Errorf("invalid audio format %+v", f)
	}
	return nil
}
