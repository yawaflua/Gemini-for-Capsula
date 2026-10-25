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

// Package logging configures structured logging for the binaries.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// New returns a logger writing to stderr at the given level
// ("debug", "info", "warn", "error"). json selects JSON output.
func New(level string, json bool) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: l}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if json {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger, nil
}
