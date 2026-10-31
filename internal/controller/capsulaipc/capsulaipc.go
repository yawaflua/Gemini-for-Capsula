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

// Package capsulaipc controls the VK Capsule Mini volume through capsule-ipc,
// the vendor IPC the device panel uses. Unlike amixer alone it keeps the
// panel, the hardware mixer and AirPlay volume in sync.
package capsulaipc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// Volume controls the speaker through capsule-ipc.
type Volume struct {
	// Runner executes capsule-ipc. Defaults to [syscmd.Exec].
	Runner syscmd.Runner

	mu sync.Mutex
}

var _ controller.Volume = (*Volume)(nil)

func (v *Volume) run(ctx context.Context, command string) (string, error) {
	r := v.Runner
	if r == nil {
		r = syscmd.Exec{}
	}
	return r.Run(ctx, "capsule-ipc", command)
}

// Get implements [controller.Volume]. capsule-ipc GET replies with
// "OK <volume> <muted> ..."; the rest is the panel state.
func (v *Volume) Get(ctx context.Context) (models.VolumeState, error) {
	out, err := v.run(ctx, "GET")
	if err != nil {
		return models.VolumeState{}, err
	}
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "OK" {
		return models.VolumeState{}, fmt.Errorf("capsule-ipc: unexpected reply %q", out)
	}
	level, err := strconv.Atoi(f[1])
	if err != nil {
		return models.VolumeState{}, fmt.Errorf("capsule-ipc: volume %q: %w", f[1], err)
	}
	return models.VolumeState{Level: level, Muted: f[2] == "1"}, nil
}

// Set implements [controller.Volume]. VOL also unmutes, like the panel.
func (v *Volume) Set(ctx context.Context, level int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if level < 0 {
		level = 0
	}
	if level > 100 {
		level = 100
	}
	_, err := v.run(ctx, "VOL "+strconv.Itoa(level))
	return err
}

// SetMuted implements [controller.Volume]. capsule-ipc has only a toggle.
func (v *Volume) SetMuted(ctx context.Context, muted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, err := v.Get(ctx)
	if err != nil {
		return err
	}
	if st.Muted == muted {
		return nil
	}
	_, err = v.run(ctx, "MUTE")
	return err
}
