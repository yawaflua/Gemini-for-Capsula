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

package statecmd

import (
	"context"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/indicator"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls chan call
}

func newFake() *fakeRunner { return &fakeRunner{calls: make(chan call, 8)} }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls <- call{name: name, args: args}
	return "", nil
}

func waitCall(t *testing.T, f *fakeRunner) call {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no command run")
		return call{}
	}
}

func noCall(t *testing.T, f *fakeRunner) {
	t.Helper()
	select {
	case c := <-f.calls:
		t.Fatalf("unexpected run: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShowRunsCommandWithState(t *testing.T) {
	r := newFake()
	i := &Indicator{Command: []string{"ring", "--quiet"}, Runner: r}

	i.Show(indicator.Listening)
	if c := waitCall(t, r); c.name != "ring" || len(c.args) != 2 || c.args[0] != "--quiet" || c.args[1] != "listening" {
		t.Fatalf("ran %+v, want ring --quiet listening", c)
	}

	i.Show(indicator.Thinking)
	if c := waitCall(t, r); c.args[1] != "thinking" {
		t.Fatalf("ran %+v, want thinking", c)
	}

	i.Show(indicator.Idle)
	if c := waitCall(t, r); c.args[1] != "idle" {
		t.Fatalf("ran %+v, want idle", c)
	}
}

func TestShowSkipsRepeatedState(t *testing.T) {
	r := newFake()
	i := &Indicator{Command: []string{"ring"}, Runner: r}

	i.Show(indicator.Listening)
	waitCall(t, r)
	i.Show(indicator.Listening)
	noCall(t, r)

	i.Show(indicator.Thinking)
	if c := waitCall(t, r); c.args[0] != "thinking" {
		t.Fatalf("ran %+v, want thinking", c)
	}
}

func TestShowWithoutCommand(t *testing.T) {
	r := newFake()
	i := &Indicator{Runner: r}
	i.Show(indicator.Listening)
	noCall(t, r)
}

func TestShowDoesNotBlock(t *testing.T) {
	r := newFake()
	i := &Indicator{Command: []string{"ring"}, Runner: r}
	block := make(chan struct{})
	defer close(block)
	go func() {
		<-block
	}()
	done := make(chan struct{})
	go func() {
		i.Show(indicator.Speaking)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Show blocked")
	}
}
