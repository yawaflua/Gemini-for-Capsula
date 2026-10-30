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

// Package controller defines the device control interfaces (volume, media,
// Bluetooth) and a [Dispatcher] that executes [models.Command] values
// against them. Concrete implementations live in subpackages.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Volume controls the output level.
type Volume interface {
	Get(ctx context.Context) (models.VolumeState, error)
	// Set sets the level in percent, 0..100.
	Set(ctx context.Context, level int) error
	SetMuted(ctx context.Context, muted bool) error
}

// Media controls a music player.
type Media interface {
	Control(ctx context.Context, op models.MediaOperation) error
	PlayURL(ctx context.Context, url string) error
	Status(ctx context.Context) (models.MediaStatus, error)
}

// MediaSearcher is a [Media] that can also find what to play by name.
type MediaSearcher interface {
	Media
	PlaySearch(ctx context.Context, query string) error
}

// Bluetooth manages Bluetooth connections.
type Bluetooth interface {
	// Pair makes the device discoverable and accepts incoming pairing
	// requests for the given duration. It returns once pairing mode is on.
	Pair(ctx context.Context, timeout time.Duration) error
	Connect(ctx context.Context, device string) error
	// Disconnect disconnects device, or every device if empty.
	Disconnect(ctx context.Context, device string) error
	Devices(ctx context.Context) ([]models.BluetoothDevice, error)
}

// Light controls decorative lighting, e.g. the LED ring as a night light.
type Light interface {
	// SetNightLight turns the night light on or off. Empty color and zero
	// brightness select defaults.
	SetNightLight(ctx context.Context, on bool, color string, brightness int) error
	NightLight(ctx context.Context) (bool, error)
}

// ErrUnsupported is returned for actions without a configured backend.
var ErrUnsupported = errors.New("not supported on this device")

// Ring is an LED ring that can be taken over by hand. Overrides expire on
// their own (see [models.RingRequest]).
type Ring interface {
	SetRing(ctx context.Context, req models.RingRequest) (models.Override, error)
	// ResetRing returns the ring to the automatic state indicator.
	ResetRing(ctx context.Context) error
}

// Display is a display that can be taken over by hand. Overrides expire
// on their own (see [models.DisplayRequest]).
type Display interface {
	ShowDisplay(ctx context.Context, req models.DisplayRequest) (models.Override, error)
	// ResetDisplay returns the display to the automatic face.
	ResetDisplay(ctx context.Context) error
}

// Sensors reports what the device senses and shows.
type Sensors interface {
	Surface(ctx context.Context) (models.SurfaceState, error)
}

// Dispatcher routes commands to controllers. Nil controllers disable the
// corresponding actions.
type Dispatcher struct {
	Volume    Volume
	Media     Media
	Bluetooth Bluetooth
	Light     Light
	Ring      Ring
	Display   Display
	Sensors   Sensors
}

// Capabilities lists the actions the dispatcher can execute.
func (d *Dispatcher) Capabilities() []models.Action {
	var out []models.Action
	if d.Volume != nil {
		out = append(out, models.ActionVolumeGet, models.ActionVolumeSet, models.ActionVolumeAdjust, models.ActionVolumeMute)
	}
	if d.Media != nil {
		out = append(out, models.ActionMediaControl, models.ActionMediaPlayURL, models.ActionMediaStatus)
		if _, ok := d.Media.(MediaSearcher); ok {
			out = append(out, models.ActionMediaPlaySearch)
		}
	}
	if d.Bluetooth != nil {
		out = append(out, models.ActionBluetoothPair, models.ActionBluetoothConnect, models.ActionBluetoothDisconnect, models.ActionBluetoothDevices)
	}
	if d.Light != nil {
		out = append(out, models.ActionLightNight)
	}
	if d.Ring != nil {
		out = append(out, models.ActionRingSet, models.ActionRingReset)
	}
	if d.Display != nil {
		out = append(out, models.ActionDisplayShow, models.ActionDisplayReset)
	}
	if d.Sensors != nil {
		out = append(out, models.ActionSensorsGet)
	}
	return out
}

// Execute runs cmd and reports its result. It never returns nil.
func (d *Dispatcher) Execute(ctx context.Context, cmd *models.Command) *models.CommandResult {
	data, err := d.execute(ctx, cmd)
	if err != nil {
		return cmd.Failure(err)
	}
	return cmd.Success(data)
}

func (d *Dispatcher) execute(ctx context.Context, cmd *models.Command) (map[string]any, error) {
	if !slices.Contains(d.Capabilities(), cmd.Action) {
		return nil, fmt.Errorf("%s: %w", cmd.Action, ErrUnsupported)
	}
	switch cmd.Action {
	case models.ActionVolumeGet:
		v, err := d.Volume.Get(ctx)
		if err != nil {
			return nil, err
		}
		return v.Map(), nil

	case models.ActionVolumeSet:
		level, ok := cmd.Int("level")
		if !ok {
			return nil, errors.New("missing argument: level")
		}
		return d.setVolume(ctx, level)

	case models.ActionVolumeAdjust:
		delta, ok := cmd.Int("delta")
		if !ok {
			return nil, errors.New("missing argument: delta")
		}
		v, err := d.Volume.Get(ctx)
		if err != nil {
			return nil, err
		}
		return d.setVolume(ctx, v.Level+delta)

	case models.ActionVolumeMute:
		muted, ok := cmd.Bool("muted")
		if !ok {
			return nil, errors.New("missing argument: muted")
		}
		if err := d.Volume.SetMuted(ctx, muted); err != nil {
			return nil, err
		}
		return map[string]any{"muted": muted}, nil

	case models.ActionMediaControl:
		op, _ := cmd.String("operation")
		if !slices.Contains(models.MediaOperations, models.MediaOperation(op)) {
			return nil, fmt.Errorf("invalid operation %q", op)
		}
		return nil, d.Media.Control(ctx, models.MediaOperation(op))

	case models.ActionMediaPlayURL:
		url, ok := cmd.String("url")
		if !ok || url == "" {
			return nil, errors.New("missing argument: url")
		}
		if err := d.Media.PlayURL(ctx, url); err != nil {
			return nil, err
		}
		return d.nowPlaying(ctx), nil

	case models.ActionMediaPlaySearch:
		q, _ := cmd.String("query")
		if q = strings.TrimSpace(q); q == "" {
			return nil, errors.New("missing argument: query")
		}
		if err := d.Media.(MediaSearcher).PlaySearch(ctx, q); err != nil {
			return nil, err
		}
		return d.nowPlaying(ctx), nil

	case models.ActionMediaStatus:
		s, err := d.Media.Status(ctx)
		if err != nil {
			return nil, err
		}
		return s.Map(), nil

	case models.ActionBluetoothPair:
		secs, ok := cmd.Int("timeout_seconds")
		if !ok || secs <= 0 {
			secs = 120
		}
		if err := d.Bluetooth.Pair(ctx, time.Duration(secs)*time.Second); err != nil {
			return nil, err
		}
		return map[string]any{"discoverable_seconds": secs}, nil

	case models.ActionBluetoothConnect:
		dev, _ := cmd.String("device")
		if dev == "" {
			return nil, errors.New("missing argument: device")
		}
		return nil, d.Bluetooth.Connect(ctx, dev)

	case models.ActionBluetoothDisconnect:
		dev, _ := cmd.String("device")
		return nil, d.Bluetooth.Disconnect(ctx, dev)

	case models.ActionLightNight:
		on, ok := cmd.Bool("on")
		if !ok {
			return nil, errors.New("missing argument: on")
		}
		color, _ := cmd.String("color")
		brightness, _ := cmd.Int("brightness")
		if err := d.Light.SetNightLight(ctx, on, color, brightness); err != nil {
			return nil, err
		}
		return map[string]any{"on": on}, nil

	case models.ActionRingSet:
		scene, _ := cmd.String("scene")
		color, _ := cmd.String("color")
		brightness, _ := cmd.Int("brightness")
		secs, _ := cmd.Int("seconds")
		o, err := d.Ring.SetRing(ctx, models.RingRequest{Scene: scene, Color: color, Brightness: brightness, Hold: time.Duration(secs) * time.Second})
		if err != nil {
			return nil, err
		}
		return o.Map(), nil

	case models.ActionRingReset:
		return nil, d.Ring.ResetRing(ctx)

	case models.ActionDisplayShow:
		mode, _ := cmd.String("mode")
		text, _ := cmd.String("text")
		brightness, _ := cmd.Int("brightness")
		secs, _ := cmd.Int("seconds")
		o, err := d.Display.ShowDisplay(ctx, models.DisplayRequest{Mode: mode, Text: text, Brightness: brightness, Hold: time.Duration(secs) * time.Second})
		if err != nil {
			return nil, err
		}
		return o.Map(), nil

	case models.ActionDisplayReset:
		return nil, d.Display.ResetDisplay(ctx)

	case models.ActionSensorsGet:
		s, err := d.Sensors.Surface(ctx)
		if err != nil {
			return nil, err
		}
		return s.Map(), nil

	case models.ActionBluetoothDevices:
		devs, err := d.Bluetooth.Devices(ctx)
		if err != nil {
			return nil, err
		}
		list := make([]map[string]any, 0, len(devs))
		for _, dv := range devs {
			list = append(list, map[string]any{"name": dv.Name, "address": dv.Address, "connected": dv.Connected})
		}
		return map[string]any{"devices": list}, nil
	}
	return nil, fmt.Errorf("%s: %w", cmd.Action, ErrUnsupported)
}

// nowPlaying reports what started playing, so the assistant can name it.
func (d *Dispatcher) nowPlaying(ctx context.Context) map[string]any {
	st, err := d.Media.Status(ctx)
	if err != nil || st.Title == "" {
		return nil
	}
	return map[string]any{"title": st.Title}
}

func (d *Dispatcher) setVolume(ctx context.Context, level int) (map[string]any, error) {
	level = max(0, min(100, level))
	if err := d.Volume.Set(ctx, level); err != nil {
		return nil, err
	}
	return map[string]any{"level": level}, nil
}

// ObservedVolume reports every change made through it, so the speaker can
// show feedback no matter who changed the volume (voice, web panel, keys).
type ObservedVolume struct {
	Volume
	// OnChange is called after a successful Set or SetMuted.
	OnChange func(models.VolumeState)
}

// Set implements [Volume].
func (o *ObservedVolume) Set(ctx context.Context, level int) error {
	if err := o.Volume.Set(ctx, level); err != nil {
		return err
	}
	o.OnChange(models.VolumeState{Level: level})
	return nil
}

// SetMuted implements [Volume].
func (o *ObservedVolume) SetMuted(ctx context.Context, muted bool) error {
	if err := o.Volume.SetMuted(ctx, muted); err != nil {
		return err
	}
	st, err := o.Volume.Get(ctx)
	if err != nil {
		st = models.VolumeState{}
	}
	st.Muted = muted
	o.OnChange(st)
	return nil
}
