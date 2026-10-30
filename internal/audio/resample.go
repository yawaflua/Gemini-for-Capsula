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

import "encoding/binary"

// Resampler converts a mono LINEAR16 stream between sample rates by linear
// interpolation. It keeps state between calls, so a stream split into
// chunks of any size is converted without clicks at chunk boundaries.
// Speech needs nothing better: both rates are well above its bandwidth.
//
// A Resampler is not safe for concurrent use.
type Resampler struct {
	from, to int
	pos      int   // position of the next output sample relative to prev, in 1/to input samples
	prev     int16 // last input sample of the previous chunk
	started  bool
}

// NewResampler returns a resampler from rate from to rate to (Hz).
func NewResampler(from, to int) *Resampler {
	return &Resampler{from: from, to: to}
}

// Write converts a chunk of samples. A trailing odd byte is ignored.
func (r *Resampler) Write(pcm []byte) []byte {
	if r.from == r.to {
		return pcm
	}
	n := len(pcm) / 2
	if n == 0 {
		return nil
	}
	in := func(i int) int16 { // i = -1 is the last sample of the previous chunk
		if i < 0 {
			return r.prev
		}
		return int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	if !r.started {
		r.prev, r.pos, r.started = in(0), r.to, true
	}
	out := make([]byte, 0, (n*r.to/r.from+2)*2)
	// Input samples are indexed from prev (index 0) to in(n-1) (index n).
	for r.pos <= n*r.to {
		i, frac := r.pos/r.to, r.pos%r.to
		v := int(in(i - 1))
		if frac > 0 && i < n {
			v += (int(in(i)) - v) * frac / r.to
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(v)))
		r.pos += r.from
	}
	r.pos -= n * r.to
	r.prev = in(n - 1)
	return out
}
