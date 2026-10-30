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
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func ramp(n int) []byte {
	var b []byte
	for i := range n {
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(i*10)))
	}
	return b
}

func samples(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

func TestResamplerUpsamplesRamp(t *testing.T) {
	r := NewResampler(16000, 24000)
	got := samples(r.Write(ramp(160)))
	if len(got) < 238 || len(got) > 240 {
		t.Fatalf("got %d samples, want ~240", len(got))
	}
	// A ramp of 10 per input sample is 10*16/24 per output sample.
	for i, v := range got {
		want := float64(i) * 10 * 16000 / 24000
		if math.Abs(float64(v)-want) > 1 {
			t.Fatalf("sample %d = %d, want %.1f", i, v, want)
		}
	}
}

func TestResamplerChunkingIsSeamless(t *testing.T) {
	in := ramp(1000)
	whole := NewResampler(16000, 24000).Write(in)
	r := NewResampler(16000, 24000)
	var parts []byte
	for _, n := range []int{2, 34, 318, 6, 1640} {
		parts = append(parts, r.Write(in[:n])...)
		in = in[n:]
	}
	if !bytes.Equal(whole, parts) {
		t.Fatalf("chunked output differs: %d vs %d bytes", len(parts), len(whole))
	}
}

func TestResamplerDownsamples(t *testing.T) {
	got := NewResampler(24000, 16000).Write(ramp(240))
	if n := len(got) / 2; n < 159 || n > 161 {
		t.Fatalf("got %d samples, want ~160", n)
	}
}

func TestResamplerSameRate(t *testing.T) {
	in := ramp(10)
	if got := NewResampler(16000, 16000).Write(in); !bytes.Equal(got, in) {
		t.Fatal("same-rate resampling changed the audio")
	}
}
