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

// Package touchkeys handles the speaker's touch panel (Hynitron CSK05T):
//
//	VOL+ / VOL-     volume, auto-repeat while held, level shown on the display
//	centre, short   activate the assistant
//	centre, long    Bluetooth pairing (bt-pair)
//	PROG1           next track
//	MICMUTE         mute the microphone
//
// [Panel] is a [wakeword.Detector], so the centre key activates the agent
// exactly like a wake word.
package touchkeys

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/evdev"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

// DefaultDeviceName is the input device name of the touch controller.
const DefaultDeviceName = "Hynitron CSK05T"

// Linux key codes used by the panel.
const (
	KeyVolumeDown = 114
	KeyVolumeUp   = 115
	KeyProg1      = 148
	KeyMicMute    = 248
	KeyVoice      = 582
)

// Config configures a [Panel].
type Config struct {
	// Device is the evdev node; empty looks it up by Name.
	Device string
	Name   string

	CenterKey, NextKey, MicKey, VolumeUpKey, VolumeDownKey uint16

	// VolumeStep is the change per press, per cent.
	VolumeStep int
	// LongPress is how long the centre key must be held for pairing.
	LongPress time.Duration
	// RepeatDelay and RepeatInterval control volume auto-repeat.
	RepeatDelay, RepeatInterval time.Duration
	// PairTimeout is passed to [controller.Bluetooth.Pair].
	PairTimeout time.Duration
}

// DefaultConfig returns the VK Capsula Mini layout.
func DefaultConfig() Config {
	return Config{
		Name:           DefaultDeviceName,
		CenterKey:      KeyVoice,
		NextKey:        KeyProg1,
		MicKey:         KeyMicMute,
		VolumeUpKey:    KeyVolumeUp,
		VolumeDownKey:  KeyVolumeDown,
		VolumeStep:     5,
		LongPress:      1500 * time.Millisecond,
		RepeatDelay:    400 * time.Millisecond,
		RepeatInterval: 150 * time.Millisecond,
		PairTimeout:    2 * time.Minute,
	}
}

// KeyName returns the name of a key of the panel: "voice", "next", "mic",
// "volume_up", "volume_down", or "key_<code>" for any other key.
func (c Config) KeyName(code uint16) string {
	switch code {
	case c.CenterKey:
		return "voice"
	case c.NextKey:
		return "next"
	case c.MicKey:
		return "mic"
	case c.VolumeUpKey:
		return "volume_up"
	case c.VolumeDownKey:
		return "volume_down"
	}
	return "key_" + strconv.Itoa(int(code))
}

// Overlayer shows short feedback, e.g. the volume level.
type Overlayer interface {
	Overlay(text string, hold time.Duration)
}

type nopOverlay struct{}

func (nopOverlay) Overlay(string, time.Duration) {}

// Panel reads the touch keys and performs their actions.
type Panel struct {
	Config     Config
	Dispatcher *controller.Dispatcher
	// Display shows feedback; optional.
	Display Overlayer
	// OnMicMute is called when the microphone is muted or unmuted.
	OnMicMute func(muted bool)
	// OnKey, if set, is called for every press and release with the key
	// name (see [Config.KeyName]), before the key's own action.
	OnKey  func(name string, code uint16, pressed bool)
	Logger *slog.Logger

	muted bool
}

var _ wakeword.Detector = (*Panel)(nil)

// WantsAudio implements [wakeword.Detector].
func (p *Panel) WantsAudio() bool { return false }

// Run implements [wakeword.Detector].
func (p *Panel) Run(ctx context.Context, _ <-chan []byte, out chan<- wakeword.Detection) error {
	if p.Display == nil {
		p.Display = nopOverlay{}
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	dev := p.Config.Device
	if dev == "" {
		var err error
		if dev, err = evdev.FindByName(p.Config.Name); err != nil {
			return err
		}
	}
	p.Logger.Info("touch keys enabled", "device", dev)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan evdev.KeyEvent, 16)
	errc := make(chan error, 1)
	go func() { errc <- evdev.ReadKeys(ctx, dev, events) }()

	var (
		longT   *time.Timer
		longC   <-chan time.Time
		longHit bool
		repT    *time.Timer
		repC    <-chan time.Time
		repD    int
	)
	stopRepeat := func() {
		if repT != nil {
			repT.Stop()
		}
		repC, repD = nil, 0
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err

		case <-repC:
			p.adjustVolume(ctx, repD)
			repT = time.NewTimer(p.Config.RepeatInterval)
			repC = repT.C

		case <-longC:
			longC, longHit = nil, true
			p.pair()

		case ev := <-events:
			if ev.Value == evdev.KeyRepeat {
				continue // the kernel's repeat is replaced by ours
			}
			pressed := ev.Value == evdev.KeyPress
			if p.OnKey != nil {
				p.OnKey(p.Config.KeyName(ev.Code), ev.Code, pressed)
			}
			switch ev.Code {
			case p.Config.VolumeUpKey, p.Config.VolumeDownKey:
				stopRepeat()
				if pressed {
					repD = p.Config.VolumeStep
					if ev.Code == p.Config.VolumeDownKey {
						repD = -repD
					}
					p.adjustVolume(ctx, repD)
					repT = time.NewTimer(p.Config.RepeatDelay)
					repC = repT.C
				}

			case p.Config.CenterKey:
				if pressed {
					longHit = false
					longT = time.NewTimer(p.Config.LongPress)
					longC = longT.C
				} else if longT != nil {
					longT.Stop()
					longC = nil
					if !longHit {
						p.activate(out)
					}
				}

			case p.Config.NextKey:
				if pressed {
					p.next(ctx)
				}

			case p.Config.MicKey:
				if pressed {
					p.toggleMic()
				}
			}
		}
	}
}

func (p *Panel) activate(out chan<- wakeword.Detection) {
	if p.muted {
		p.Display.Overlay("OFF", 800*time.Millisecond)
		return
	}
	select {
	case out <- wakeword.Detection{Source: "touch", At: time.Now()}:
	default:
	}
}

func (p *Panel) adjustVolume(ctx context.Context, delta int) {
	v := p.Dispatcher.Volume
	if v == nil {
		return
	}
	st, err := v.Get(ctx)
	if err == nil {
		level := max(0, min(100, st.Level+delta))
		if err = v.Set(ctx, level); err == nil {
			p.Display.Overlay(strconv.Itoa(level), 1500*time.Millisecond)
			return
		}
	}
	p.Logger.Warn("touch keys: volume", "err", err)
	p.Display.Overlay("Err", time.Second)
}

func (p *Panel) pair() {
	bt := p.Dispatcher.Bluetooth
	if bt == nil {
		return
	}
	p.Display.Overlay("bt", 2*time.Second)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bt.Pair(ctx, p.Config.PairTimeout); err != nil {
			p.Logger.Warn("touch keys: bluetooth pairing", "err", err)
			p.Display.Overlay("Err", 1500*time.Millisecond)
		}
	}()
}

func (p *Panel) next(ctx context.Context) {
	m := p.Dispatcher.Media
	if m == nil {
		return
	}
	if err := m.Control(ctx, models.MediaNext); err != nil {
		p.Logger.Warn("touch keys: next track", "err", err)
	}
}

func (p *Panel) toggleMic() {
	p.muted = !p.muted
	// No text on the display: the LED ring and the face show the state.
	if p.OnMicMute != nil {
		p.OnMicMute(p.muted)
	}
}
