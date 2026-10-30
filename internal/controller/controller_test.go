// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

type fakeVolume struct{ state models.VolumeState }

func (f *fakeVolume) Get(context.Context) (models.VolumeState, error) { return f.state, nil }
func (f *fakeVolume) Set(_ context.Context, l int) error              { f.state.Level = l; return nil }
func (f *fakeVolume) SetMuted(_ context.Context, m bool) error        { f.state.Muted = m; return nil }

type fakeBT struct{ paired time.Duration }

func (f *fakeBT) Pair(_ context.Context, d time.Duration) error { f.paired = d; return nil }
func (f *fakeBT) Connect(context.Context, string) error         { return nil }
func (f *fakeBT) Disconnect(context.Context, string) error      { return nil }
func (f *fakeBT) Devices(context.Context) ([]models.BluetoothDevice, error) {
	return []models.BluetoothDevice{{Address: "AA:BB:CC:DD:EE:FF", Name: "Phone", Connected: true}}, nil
}

type fakeLight struct {
	on    bool
	color string
}

func (f *fakeLight) SetNightLight(_ context.Context, on bool, color string, _ int) error {
	f.on, f.color = on, color
	return nil
}
func (f *fakeLight) NightLight(context.Context) (bool, error) { return f.on, nil }

func TestDispatcher(t *testing.T) {
	ctx := context.Background()
	vol := &fakeVolume{state: models.VolumeState{Level: 50}}
	bt := &fakeBT{}
	light := &fakeLight{}
	d := &controller.Dispatcher{Volume: vol, Bluetooth: bt, Light: light}

	tests := []struct {
		name   string
		cmd    models.Command
		wantOK bool
		check  func() bool
	}{
		{"adjust", models.Command{Action: models.ActionVolumeAdjust, Args: map[string]any{"delta": float64(20)}}, true, func() bool { return vol.state.Level == 70 }},
		{"clamp", models.Command{Action: models.ActionVolumeAdjust, Args: map[string]any{"delta": float64(90)}}, true, func() bool { return vol.state.Level == 100 }},
		{"set", models.Command{Action: models.ActionVolumeSet, Args: map[string]any{"level": float64(15)}}, true, func() bool { return vol.state.Level == 15 }},
		{"mute", models.Command{Action: models.ActionVolumeMute, Args: map[string]any{"muted": true}}, true, func() bool { return vol.state.Muted }},
		{"missing arg", models.Command{Action: models.ActionVolumeSet}, false, nil},
		{"pair default timeout", models.Command{Action: models.ActionBluetoothPair}, true, func() bool { return bt.paired == 2*time.Minute }},
		{"night light", models.Command{Action: models.ActionLightNight, Args: map[string]any{"on": true, "color": "ffb000"}}, true, func() bool { return light.on && light.color == "ffb000" }},
		{"night light missing on", models.Command{Action: models.ActionLightNight}, false, nil},
		{"devices", models.Command{Action: models.ActionBluetoothDevices}, true, nil},
		{"unsupported", models.Command{Action: models.ActionMediaControl, Args: map[string]any{"operation": "play"}}, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := d.Execute(ctx, &tc.cmd)
			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v (%s), want %v", res.OK, res.Error, tc.wantOK)
			}
			if tc.check != nil && !tc.check() {
				t.Errorf("unexpected controller state after %s", tc.name)
			}
		})
	}
}

type fakeSurface struct {
	ring    models.RingRequest
	display models.DisplayRequest
	resets  int
}

func (f *fakeSurface) SetRing(_ context.Context, r models.RingRequest) (models.Override, error) {
	if err := r.Normalize(); err != nil {
		return models.Override{}, err
	}
	f.ring = r
	return models.Override{Scene: r.Scene, Color: r.Color, Until: time.Now().Add(r.Hold)}, nil
}
func (f *fakeSurface) ResetRing(context.Context) error { f.resets++; return nil }
func (f *fakeSurface) ShowDisplay(_ context.Context, r models.DisplayRequest) (models.Override, error) {
	if err := r.Normalize(); err != nil {
		return models.Override{}, err
	}
	f.display = r
	return models.Override{Mode: r.Mode, Text: r.Text, Until: time.Now().Add(r.Hold)}, nil
}
func (f *fakeSurface) ResetDisplay(context.Context) error { f.resets++; return nil }
func (f *fakeSurface) Surface(context.Context) (models.SurfaceState, error) {
	lux := 42.0
	return models.SurfaceState{State: "idle", Lux: &lux, HasRing: true}, nil
}

func TestDispatcherSurface(t *testing.T) {
	ctx := context.Background()
	s := &fakeSurface{}
	d := &controller.Dispatcher{Ring: s, Display: s, Sensors: s}
	exec := func(a models.Action, args map[string]any) *models.CommandResult {
		return d.Execute(ctx, &models.Command{Action: a, Args: args})
	}

	res := exec(models.ActionRingSet, map[string]any{"scene": "spin", "color": "00ff00", "seconds": float64(30)})
	if !res.OK || s.ring.Scene != "spin" || s.ring.Hold != 30*time.Second || res.Data["seconds"] != 30 {
		t.Errorf("ring.set: %+v, request %+v", res, s.ring)
	}
	if res := exec(models.ActionRingSet, map[string]any{"color": "green"}); res.OK {
		t.Error("ring.set must reject colour names")
	}
	res = exec(models.ActionDisplayShow, map[string]any{"text": "HI"})
	if !res.OK || s.display.Mode != "text" || s.display.Hold != models.DefaultOverride || res.Data["text"] != "HI" {
		t.Errorf("display.show: %+v, request %+v", res, s.display)
	}
	if !exec(models.ActionRingReset, nil).OK || !exec(models.ActionDisplayReset, nil).OK || s.resets != 2 {
		t.Error("resets failed")
	}
	res = exec(models.ActionSensorsGet, nil)
	if !res.OK || res.Data["lux"] != 42.0 || res.Data["state"] != "idle" {
		t.Errorf("sensors.get: %+v", res)
	}
	if got := len(d.Capabilities()); got != 5 {
		t.Errorf("capabilities = %d, want 5", got)
	}
}
