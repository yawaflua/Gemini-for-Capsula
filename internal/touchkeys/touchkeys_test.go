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
package touchkeys

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

type fakeVolume struct {
	mu    sync.Mutex
	level int
}

func (v *fakeVolume) Get(context.Context) (models.VolumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return models.VolumeState{Level: v.level}, nil
}

func (v *fakeVolume) Set(_ context.Context, l int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.level = l
	return nil
}

func (v *fakeVolume) SetMuted(context.Context, bool) error { return nil }

type fakeBT struct{ paired chan struct{} }

func (b *fakeBT) Pair(context.Context, time.Duration) error { close(b.paired); return nil }
func (b *fakeBT) Connect(context.Context, string) error     { return nil }
func (b *fakeBT) Disconnect(context.Context, string) error  { return nil }
func (b *fakeBT) Devices(context.Context) ([]models.BluetoothDevice, error) {
	return nil, nil
}

type overlays struct {
	mu   sync.Mutex
	text []string
}

func (o *overlays) Overlay(s string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text = append(o.text, s)
}

func event(code uint16, value int32) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b[16:], 1) // EV_KEY
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	return b
}

func TestPanel(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "event0")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	vol := &fakeVolume{level: 50}
	bt := &fakeBT{paired: make(chan struct{})}
	ov := &overlays{}
	var (
		muted  atomic.Bool
		keysMu sync.Mutex
		keys   []string
	)
	cfg := DefaultConfig()
	cfg.Device = fifo
	cfg.LongPress = 100 * time.Millisecond
	p := &Panel{
		Config:     cfg,
		Dispatcher: &controller.Dispatcher{Volume: vol, Bluetooth: bt},
		Display:    ov,
		OnMicMute:  func(m bool) { muted.Store(m) },
		OnKey: func(name string, _ uint16, pressed bool) {
			if pressed {
				keysMu.Lock()
				keys = append(keys, name)
				keysMu.Unlock()
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan wakeword.Detection, 4)
	go p.Run(ctx, nil, out)

	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	send := func(code uint16, v int32) { w.Write(event(code, v)) }

	// Short centre press activates the assistant.
	send(KeyVoice, 1)
	send(KeyVoice, 0)
	select {
	case d := <-out:
		if d.Source != "touch" {
			t.Errorf("detection source = %q", d.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("short press did not activate the assistant")
	}

	// Long centre press pairs and does not activate.
	send(KeyVoice, 1)
	select {
	case <-bt.paired:
	case <-time.After(time.Second):
		t.Fatal("long press did not start pairing")
	}
	send(KeyVoice, 0)

	// Volume up by one step.
	send(KeyVolumeUp, 1)
	send(KeyVolumeUp, 0)

	// Mic mute.
	send(KeyMicMute, 1)
	send(KeyMicMute, 0)

	time.Sleep(100 * time.Millisecond)
	if lvl, _ := vol.Get(ctx); lvl.Level != 55 {
		t.Errorf("volume = %d, want 55", lvl.Level)
	}
	if !muted.Load() {
		t.Error("microphone should be muted")
	}
	if len(out) != 0 {
		t.Error("long press must not activate the assistant")
	}
	keysMu.Lock()
	if want := []string{"voice", "voice", "volume_up", "mic"}; !slices.Equal(keys, want) {
		t.Errorf("OnKey presses = %v, want %v", keys, want)
	}
	keysMu.Unlock()
	ov.mu.Lock()
	defer ov.mu.Unlock()
	// Muting shows no text: the ring and the face show it.
	if want := []string{"bt", "55"}; len(ov.text) != 2 || ov.text[0] != want[0] || ov.text[1] != want[1] {
		t.Errorf("overlays = %v, want %v", ov.text, want)
	}
}
