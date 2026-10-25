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

// Package button implements a [wakeword.Detector] triggered by a hardware key
// exposed through the Linux evdev interface (/dev/input/eventN).
package button

import (
	"context"
	"fmt"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/evdev"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// Detector fires when a key is pressed.
type Detector struct {
	// Device is the evdev node, e.g. "/dev/input/event0".
	Device string
	// KeyCode is the Linux key code (see linux/input-event-codes.h). Zero
	// accepts any key.
	KeyCode uint16
}

var _ wakeword.Detector = (*Detector)(nil)

// WantsAudio implements [wakeword.Detector].
func (d *Detector) WantsAudio() bool { return false }

// Run implements [wakeword.Detector].
func (d *Detector) Run(ctx context.Context, _ <-chan []byte, out chan<- wakeword.Detection) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan evdev.KeyEvent, 16)
	errc := make(chan error, 1)
	go func() { errc <- evdev.ReadKeys(ctx, d.Device, events) }()
	for {
		select {
		case err := <-errc:
			return err
		case ev := <-events:
			if ev.Value != evdev.KeyPress || (d.KeyCode != 0 && ev.Code != d.KeyCode) {
				continue
			}
			select {
			case out <- wakeword.Detection{Source: "button", Keyword: fmt.Sprintf("key:%d", ev.Code), At: time.Now()}:
			default:
			}
		}
	}
}
