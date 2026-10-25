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

package logging

import (
	"io"
	"log/slog"
	"testing"
)

func TestForwarder(t *testing.T) {
	f := NewForwarder(slog.NewTextHandler(io.Discard, nil), slog.LevelWarn, 2)
	log := slog.New(f).With("component", "ringled")
	log.Info("not forwarded")
	log.Warn("ringled failed", "err", "exit status 1")
	log.Error("x")
	log.Error("dropped, queue full")

	r := <-f.Records()
	if r.Level != "WARN" || r.Message != "ringled failed" || r.Attrs["component"] != "ringled" || r.Attrs["err"] != "exit status 1" {
		t.Errorf("record = %+v", r)
	}
	if r := <-f.Records(); r.Message != "x" {
		t.Errorf("second record = %+v", r)
	}
	if len(f.Records()) != 0 {
		t.Error("queue should be empty")
	}
}
