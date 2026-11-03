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

// Package statecmd shows the assistant state by running an external command.
// It is for devices whose ring or display belongs to another process: the
// agent reports the state, that process draws it.
//
// The state is appended as the last argument: idle, listening, thinking,
// speaking, error, disconnected or announcing. Repeated states are skipped.
package statecmd

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// timeout bounds one run: a slow or hung command must not delay the next
// state for long.
const timeout = 3 * time.Second

// Indicator runs a command on every state change.
type Indicator struct {
	// Command is the program and its fixed arguments; the state is appended.
	Command []string
	// Runner executes the command. Defaults to [syscmd.Exec].
	Runner syscmd.Runner
	// Logger reports failed runs. Nil discards them.
	Logger *slog.Logger

	// mu guards the last shown state.
	mu   sync.Mutex
	last indicator.State
	seen bool
	// runMu keeps runs in order, so a quick listening -> thinking sequence
	// cannot arrive reversed.
	runMu sync.Mutex
}

var _ indicator.Indicator = (*Indicator)(nil)

// Show implements [indicator.Indicator]. It returns at once and runs the
// command in the background.
func (i *Indicator) Show(s indicator.State) {
	if len(i.Command) == 0 {
		return
	}
	i.mu.Lock()
	if i.seen && i.last == s {
		i.mu.Unlock()
		return
	}
	i.last, i.seen = s, true
	i.mu.Unlock()
	go i.run(s)
}

func (i *Indicator) run(s indicator.State) {
	i.runMu.Lock()
	defer i.runMu.Unlock()

	r := i.Runner
	if r == nil {
		r = syscmd.Exec{}
	}
	args := append(append([]string{}, i.Command[1:]...), s.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := r.Run(ctx, i.Command[0], args...); err != nil && i.Logger != nil {
		i.Logger.Warn("state command failed", "state", s.String(), "err", err)
	}
}
