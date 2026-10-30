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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Strip shows frames on the ring.
type Strip interface {
	Write(Frame) error
}

// DefaultDevice is the spidev node of the ring.
const DefaultDevice = "/dev/spidev0.0"

// compatiblePath lists the device tree compatible strings (a variable for
// tests).
var compatiblePath = "/proc/device-tree/compatible"

// Detect reports whether this is a VK Capsula Mini whose ring [NewSPI] can
// drive: the device tree names the board and the meson-spicc driver is
// loaded. The spidev node itself may be missing for a moment, while the
// controller is rebound, or for good after an interrupted rebind, which the
// first frame repairs.
func Detect() bool {
	b, err := os.ReadFile(compatiblePath)
	if err != nil || !slices.Contains(strings.Split(string(b), "\x00"), "vk,capsula-mini") {
		return false
	}
	_, err = os.Stat(spiDriver)
	return err == nil
}

// spiStrip writes frames to spidev directly: the meson-spicc driver only clocks the first transfer after a bind
// at the requested rate, so the controller is rebound before every frame.
// That costs a few milliseconds, still fine for 30 frames a second.
type spiStrip struct {
	dev string
	rx  []byte
}

// NewSPI returns a strip on the spidev node dev.
func NewSPI(dev string) Strip { return &spiStrip{dev: dev} }

// spiTransfer is struct spi_ioc_transfer.
type spiTransfer struct {
	txBuf, rxBuf   uint64
	len, speedHz   uint32
	delayUsecs     uint16
	bitsPerWord    uint8
	csChange       uint8
	txNbits        uint8
	rxNbits        uint8
	wordDelayUsecs uint8
	_              uint8
}

// spiIOCMessage2 is SPI_IOC_MESSAGE(2).
const spiIOCMessage2 = 0x40000000 | (2*32)<<16 | 'k'<<8

func (s *spiStrip) Write(f Frame) error {
	tx := encode(f)
	if len(s.rx) < len(tx) {
		s.rx = make([]byte, len(tx))
	}
	if err := rebindSPI(s.dev); err != nil {
		return err
	}
	fd, err := syscall.Open(s.dev, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.dev, err)
	}
	defer syscall.Close(fd)

	prime := make([]byte, primeBytes)
	xfer := [2]spiTransfer{
		{
			txBuf: uint64(uintptr(unsafe.Pointer(&prime[0]))), rxBuf: uint64(uintptr(unsafe.Pointer(&prime[0]))),
			len: primeBytes, speedHz: primeSpeed, bitsPerWord: 64,
		},
		{
			txBuf: uint64(uintptr(unsafe.Pointer(&tx[0]))), rxBuf: uint64(uintptr(unsafe.Pointer(&s.rx[0]))),
			len: uint32(len(tx)), speedHz: spiSpeed, bitsPerWord: 64,
		},
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), spiIOCMessage2, uintptr(unsafe.Pointer(&xfer[0])))
	runtime.KeepAlive(prime)
	runtime.KeepAlive(tx)
	runtime.KeepAlive(s.rx)
	if errno != 0 {
		return fmt.Errorf("SPI_IOC_MESSAGE on %s: %w", s.dev, errno)
	}
	return nil
}

// rebindSPI unbinds and binds the meson-spicc controllers and waits for dev
// to come back. A controller left unbound by an interrupted run is bound.
func rebindSPI(dev string) error {
	entries, err := os.ReadDir(spiDriver)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".spi") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		if _, err := bindSPI(); err != nil {
			return err
		}
	}
	for _, n := range names {
		// No cancellation in between: an unbound controller breaks the ring
		// until it is bound again.
		if err := os.WriteFile(filepath.Join(spiDriver, "unbind"), []byte(n), 0); err != nil {
			return fmt.Errorf("unbind %s: %w", n, err)
		}
		if err := os.WriteFile(filepath.Join(spiDriver, "bind"), []byte(n), 0); err != nil {
			return fmt.Errorf("bind %s: %w", n, err)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); ; {
		if err := syscall.Access(dev, 2 /* W_OK */); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("spidev node did not come back after rebind")
		}
		time.Sleep(time.Millisecond)
	}
}

// ledPrefix names the ring LEDs of builds that drive the ring with an
// is31fl32xx LED controller instead of a WS2812 chain on SPI (the stock 4.9
// kernel): top-led1..top-led36, three per pixel in R, G, B order.
const ledPrefix = "top-led"

// ledsRoot is the LED class directory (a variable for tests).
var ledsRoot = "/sys/class/leds"

// LEDsAvailable reports whether [NewLEDs] can drive the ring.
func LEDsAvailable() bool {
	for i := 1; i <= Pixels*3; i++ {
		if _, err := os.Stat(filepath.Join(ledsRoot, ledPrefix+strconv.Itoa(i), "brightness")); err != nil {
			return false
		}
	}
	return true
}

// ledStrip writes frames to the brightness of LED class devices, only the
// channels that changed: every write is an I²C transfer.
type ledStrip struct {
	files [Pixels][3]*os.File
	max   int
	last  [Pixels][3]int
}

// NewLEDs returns a strip on the LED class devices top-led1..top-led36.
func NewLEDs() (Strip, error) {
	s := &ledStrip{max: 255}
	for i := range Pixels {
		for c := range 3 {
			dir := filepath.Join(ledsRoot, ledPrefix+strconv.Itoa(i*3+c+1))
			os.WriteFile(filepath.Join(dir, "trigger"), []byte("none"), 0)
			f, err := os.OpenFile(filepath.Join(dir, "brightness"), os.O_WRONLY, 0)
			if err != nil {
				s.close()
				return nil, err
			}
			s.files[i][c] = f
			s.last[i][c] = -1
		}
	}
	if b, err := os.ReadFile(filepath.Join(ledsRoot, ledPrefix+"1", "max_brightness")); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			s.max = v
		}
	}
	return s, nil
}

func (s *ledStrip) close() {
	for i := range s.files {
		for _, f := range s.files[i] {
			if f != nil {
				f.Close()
			}
		}
	}
}

func (s *ledStrip) Write(f Frame) error {
	var first error
	for i, px := range f {
		for c, v := range [3]uint8{px.R, px.G, px.B} {
			level := int(v) * s.max / 255
			if level == s.last[i][c] {
				continue
			}
			if _, err := s.files[i][c].WriteAt([]byte(strconv.Itoa(level)), 0); err != nil {
				s.last[i][c] = -1
				if first == nil {
					first = fmt.Errorf("ring LED %d: %w", i*3+c+1, err)
				}
				continue
			}
			s.last[i][c] = level
		}
	}
	return first
}
