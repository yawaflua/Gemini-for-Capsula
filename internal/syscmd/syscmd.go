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

// Package syscmd runs short-lived system utilities.
package syscmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds a single command.
const DefaultTimeout = 10 * time.Second

// Runner executes commands. It is an interface so controllers can be tested
// with a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Exec is the [Runner] backed by os/exec.
type Exec struct {
	// Env is added to the inherited environment ("KEY=value").
	Env []string
}

// Run executes name with args and returns its trimmed stdout.
func (e Exec) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if len(e.Env) > 0 {
		cmd.Env = append(os.Environ(), e.Env...)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
