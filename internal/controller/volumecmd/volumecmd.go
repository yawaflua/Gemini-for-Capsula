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

// Package volumecmd implements [controller.Volume] with an external program,
// so a device can use whichever tool owns its mixer without the agent knowing
// about it.
//
// The program is called with one of three subcommands:
//
//	<cmd> get          print "<level> <state>", e.g. "40 unmuted"
//	<cmd> set <0..100> set the level in percent
//	<cmd> mute on|off  mute or unmute; setting the current state is a no-op
//
// The state word is optional and defaults to "unmuted"; a trailing "%" on the
// level is accepted. The program is not run through a shell, so arguments are
// exactly the ones listed above.
package volumecmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// Volume controls the output level by running Command.
type Volume struct {
	// Command is the program to run, e.g. "/usr/local/bin/volume".
	Command string
	// Runner executes the command. Defaults to [syscmd.Exec].
	Runner syscmd.Runner

	// mu serialises mute changes: a program may read the current state
	// before toggling it.
	mu sync.Mutex
}

var _ controller.Volume = (*Volume)(nil)

func (v *Volume) run(ctx context.Context, args ...string) (string, error) {
	if v.Command == "" {
		return "", errors.New("volumecmd: command is not configured")
	}
	r := v.Runner
	if r == nil {
		r = syscmd.Exec{}
	}
	return r.Run(ctx, v.Command, args...)
}

// Get implements [controller.Volume].
func (v *Volume) Get(ctx context.Context) (models.VolumeState, error) {
	out, err := v.run(ctx, "get")
	if err != nil {
		return models.VolumeState{}, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return models.VolumeState{}, fmt.Errorf("volumecmd: %s get: empty output", v.Command)
	}
	level, err := strconv.Atoi(strings.TrimSuffix(fields[0], "%"))
	if err != nil {
		return models.VolumeState{}, fmt.Errorf("volumecmd: %s get: level %q is not a number", v.Command, fields[0])
	}
	if level < 0 {
		level = 0
	}
	if level > 100 {
		level = 100
	}
	state := models.VolumeState{Level: level}
	if len(fields) > 1 {
		switch strings.ToLower(fields[1]) {
		case "muted":
			state.Muted = true
		case "unmuted":
		default:
			return models.VolumeState{}, fmt.Errorf("volumecmd: %s get: state %q is neither muted nor unmuted", v.Command, fields[1])
		}
	}
	return state, nil
}

// Set implements [controller.Volume]. Levels outside 0..100 are clamped.
func (v *Volume) Set(ctx context.Context, level int) error {
	level = clamp(level)
	_, err := v.run(ctx, "set", strconv.Itoa(level))
	return err
}

// SetMuted implements [controller.Volume].
func (v *Volume) SetMuted(ctx context.Context, muted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	arg := "off"
	if muted {
		arg = "on"
	}
	_, err := v.run(ctx, "mute", arg)
	return err
}

func clamp(level int) int {
	if level < 0 {
		return 0
	}
	if level > 100 {
		return 100
	}
	return level
}
