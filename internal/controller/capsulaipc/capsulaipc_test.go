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

package capsulaipc

import (
	"context"
	"reflect"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

const reply = "OK 42 1 1 0 0 0 -1 -1 500 50 1500 6 3000 80557300 31.10.2026"

type fakeRunner struct {
	out   string
	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, nil
}

func TestGet(t *testing.T) {
	v := &Volume{Runner: &fakeRunner{out: reply}}
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
	v := &Volume{Runner: r}
	if err := v.Set(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	want := []string{"capsule-ipc", "VOL 30"}
	if !reflect.DeepEqual(r.calls[0], want) {
		t.Errorf("call = %v, want %v", r.calls[0], want)
	}
}

func TestSetMuted(t *testing.T) {
	r := &fakeRunner{out: "OK 42 0 1 0 0 0 -1 -1 500 50 1500 6 3000 80557300 31.10.2026"}
	v := &Volume{Runner: r}
	if err := v.SetMuted(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"capsule-ipc", "GET"}, {"capsule-ipc", "MUTE"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
}

func TestSetMutedAlready(t *testing.T) {
	r := &fakeRunner{out: "OK 42 1 1 0 0 0 -1 -1 500 50 1500 6 3000 80557300 31.10.2026"}
	v := &Volume{Runner: r}
	if err := v.SetMuted(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Errorf("calls = %v, want only GET", r.calls)
	}
}
