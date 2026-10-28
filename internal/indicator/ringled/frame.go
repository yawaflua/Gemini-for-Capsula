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

package ringled

import (
	"fmt"
	"strconv"
)

// Pixels is the number of LEDs on the ring. Pixel 0 is next to the MICMUTE
// key; indices follow the chain.
const Pixels = 12

// RGB is one pixel.
type RGB struct{ R, G, B uint8 }

// Frame is the colour of every pixel.
type Frame [Pixels]RGB

// ParseRGB parses RRGGBB.
func ParseRGB(s string) (RGB, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || len(s) != 6 {
		return RGB{}, fmt.Errorf("invalid colour %q, want RRGGBB", s)
	}
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}, nil
}

func (c RGB) String() string { return fmt.Sprintf("%02x%02x%02x", c.R, c.G, c.B) }

// The wire format (WS2812-style chain
// on the meson-spicc SPI controller at 3.34 MHz):
//
//   - every data bit is four SPI bits, 1000 for 0 and 1100 for 1, so a byte
//     carries two data bits: 00→88, 01→8c, 10→c8, 11→cc;
//   - pixels in chain order, channels G, R, B, most significant bit first;
//   - resetBytes zero bytes before and after the data latch the frame;
//   - the buffer is sent with 64-bit SPI words (DMA rather than PIO, which
//     would break the frame every 16 bytes), so every 8 bytes are reversed.
const (
	resetBytes = 128
	spiSpeed   = 3340000
	// primeSpeed is the rate of a short dummy transfer before every frame;
	// changing the rate makes the driver reprogram its clock divider.
	primeSpeed = 1000000
	primeBytes = 16
)

var symbols = [4]byte{0x88, 0x8c, 0xc8, 0xcc}

// encode returns the SPI buffer for f.
func encode(f Frame) []byte {
	n := 2*resetBytes + Pixels*12
	n = (n + 7) &^ 7
	buf := make([]byte, n)
	p := buf[resetBytes:]
	for _, px := range f {
		for _, v := range [3]uint8{px.G, px.R, px.B} {
			p[0] = symbols[v>>6&3]
			p[1] = symbols[v>>4&3]
			p[2] = symbols[v>>2&3]
			p[3] = symbols[v&3]
			p = p[4:]
		}
	}
	for i := 0; i < len(buf); i += 8 {
		w := buf[i : i+8]
		for a, b := 0, 7; a < b; a, b = a+1, b-1 {
			w[a], w[b] = w[b], w[a]
		}
	}
	return buf
}
