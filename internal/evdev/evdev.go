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

// Package evdev reads key events from Linux input devices
// (/dev/input/eventN) without cgo.
package evdev

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Key event values.
const (
	KeyRelease = 0
	KeyPress   = 1
	KeyRepeat  = 2
)

const (
	evKey = 0x01
	// eventSize is sizeof(struct input_event) on 64-bit kernels:
	// timeval(16) + type(2) + code(2) + value(4).
	eventSize = 24
)

// KeyEvent is a single EV_KEY event.
type KeyEvent struct {
	Code  uint16
	Value int32
}

// FindByName returns the event node of the input device called name, as
// listed in /sys/class/input/eventN/device/name.
func FindByName(name string) (string, error) {
	names, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err == nil && strings.TrimSpace(string(b)) == name {
			return "/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(n))), nil
		}
	}
	return "", fmt.Errorf("evdev: input device %q not found", name)
}

// ReadKeys sends key events from the device at path to out until ctx is done
// or reading fails.
func ReadKeys(ctx context.Context, path string, out chan<- KeyEvent) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("evdev: %w", err)
	}
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	buf := make([]byte, eventSize)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("evdev: read %s: %w", path, err)
		}
		if binary.LittleEndian.Uint16(buf[16:]) != evKey {
			continue
		}
		ev := KeyEvent{
			Code:  binary.LittleEndian.Uint16(buf[18:]),
			Value: int32(binary.LittleEndian.Uint32(buf[20:])),
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return nil
		}
	}
}
