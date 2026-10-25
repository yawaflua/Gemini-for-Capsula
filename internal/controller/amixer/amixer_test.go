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

package amixer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

const sget = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 255
  Front Left: Playback 108 [42%] [-30.00dB] [off]
  Front Right: Playback 108 [42%] [-30.00dB] [off]`

type fakeRunner struct{ calls [][]string }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return sget, nil
}

func TestGet(t *testing.T) {
	v := &Volume{Card: "0", Control: "Master", Runner: &fakeRunner{}}
	got, err := v.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (models.VolumeState{Level: 42, Muted: true}); got != want {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestSet(t *testing.T) {
	r := &fakeRunner{}
	v := &Volume{Control: "PCM", Runner: r}
	if err := v.Set(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	want := []string{"amixer", "-q", "sset", "PCM", "30%", "unmute"}
	if !reflect.DeepEqual(r.calls[0], want) {
		t.Errorf("call = %v, want %v", r.calls[0], want)
	}
}

// noSwitchMixer simulates a control without a playback switch, like Master
// on the VK Capsule: "mute"/"unmute" fail with "Invalid command!".
type noSwitchMixer struct{ level int }

func (m *noSwitchMixer) Run(_ context.Context, _ string, args ...string) (string, error) {
	last := args[len(args)-1]
	switch {
	case args[0] == "sget":
		return fmt.Sprintf("Simple mixer control 'Master',0\n  Capabilities: volume\n  Mono: %d [%d%%]", m.level, m.level), nil
	case last == "mute" || last == "unmute":
		return "", errors.New("exit status 1: amixer: Invalid command!")
	case strings.HasSuffix(last, "%"):
		m.level, _ = strconv.Atoi(strings.TrimSuffix(last, "%"))
	}
	return "", nil
}

func TestSoftMute(t *testing.T) {
	ctx := context.Background()
	m := &noSwitchMixer{level: 43}
	v := &Volume{Runner: m}
	if err := v.SetMuted(ctx, true); err != nil {
		t.Fatal(err)
	}
	if m.level != 0 {
		t.Errorf("level while muted = %d, want 0", m.level)
	}
	if got, _ := v.Get(ctx); got != (models.VolumeState{Level: 43, Muted: true}) {
		t.Errorf("Get() while muted = %+v", got)
	}
	if err := v.SetMuted(ctx, true); err != nil || m.level != 0 {
		t.Errorf("second mute: %v, level %d", err, m.level)
	}
	if err := v.SetMuted(ctx, false); err != nil {
		t.Fatal(err)
	}
	if m.level != 43 {
		t.Errorf("level after unmute = %d, want 43", m.level)
	}
	if got, _ := v.Get(ctx); got != (models.VolumeState{Level: 43}) {
		t.Errorf("Get() after unmute = %+v", got)
	}

	// Setting a level while muted unmutes.
	v.SetMuted(ctx, true)
	v.Set(ctx, 20)
	if got, _ := v.Get(ctx); got != (models.VolumeState{Level: 20}) {
		t.Errorf("Get() after Set while muted = %+v", got)
	}
}
