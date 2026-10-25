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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// SPIAvailable reports whether [NewSPI] can drive the ring on this device.
func SPIAvailable(dev string) bool {
	if _, err := os.Stat(spiDriver); err != nil {
		return false
	}
	_, err := os.Stat(dev)
	return err == nil
}

// spiStrip writes frames to spidev directly, the way the firmware's ringled
// does: the meson-spicc driver only clocks the first transfer after a bind
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

// execStrip shows frames with the ringled utility, one run per frame.
type execStrip struct{ path string }

// NewExec returns a strip driven by the ringled binary at path.
func NewExec(path string) Strip { return execStrip{path: path} }

func (s execStrip) Write(f Frame) error {
	args := []string{"-b", "100", "set"}
	for i, px := range f {
		args = append(args, strconv.Itoa(i), px.String())
	}
	var (
		out []byte
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err = exec.CommandContext(ctx, s.path, args...).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		if strings.Contains(string(out), "no meson-spicc device bound") {
			bindSPI()
		}
	}
	return fmt.Errorf("ringled: %w: %s", err, strings.TrimSpace(string(out)))
}
