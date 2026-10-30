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

package airplay

import (
	"context"
	"slices"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

type fakeRemote struct {
	running bool
	props   map[string]any
	calls   []string
}

func (f *fakeRemote) Call(_ context.Context, method string, _ ...any) error {
	if !f.running {
		return ErrUnavailable
	}
	f.calls = append(f.calls, method)
	return nil
}

func (f *fakeRemote) Property(_ context.Context, name string) (dbus.Variant, error) {
	if !f.running {
		return dbus.Variant{}, ErrUnavailable
	}
	v, ok := f.props[name]
	if !ok {
		return dbus.Variant{}, dbus.ErrMsgNoObject
	}
	return dbus.MakeVariant(v), nil
}

type fakeMedia struct{ ops []string }

func (m *fakeMedia) Control(_ context.Context, op models.MediaOperation) error {
	m.ops = append(m.ops, string(op))
	return nil
}
func (m *fakeMedia) PlayURL(_ context.Context, url string) error {
	m.ops = append(m.ops, "url "+url)
	return nil
}
func (m *fakeMedia) PlaySearch(_ context.Context, q string) error {
	m.ops = append(m.ops, "search "+q)
	return nil
}
func (m *fakeMedia) Status(context.Context) (models.MediaStatus, error) {
	return models.MediaStatus{Title: "next"}, nil
}

func playing(state string) *fakeRemote {
	return &fakeRemote{running: true, props: map[string]any{
		"Available":   true,
		"PlayerState": state,
		"Client":      "192.168.1.5",
		"Metadata": map[string]dbus.Variant{
			"xesam:title":  dbus.MakeVariant("Группа крови"),
			"xesam:artist": dbus.MakeVariant([]string{"Кино"}),
		},
	}}
}

func TestControlGoesToPhoneWhilePlaying(t *testing.T) {
	ctx := context.Background()
	r, next := playing("Playing"), &fakeMedia{}
	p := &Player{Next: next, Remote: r}

	if err := p.Control(ctx, models.MediaPause); err != nil {
		t.Fatal(err)
	}
	if err := p.Control(ctx, models.MediaNext); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls, []string{"Pause", "Next"}) || len(next.ops) != 0 {
		t.Fatalf("airplay calls %v, next player %v", r.calls, next.ops)
	}
	st, _ := p.Status(ctx)
	if !st.Playing || st.Title != "Кино — Группа крови" || st.Source != Source {
		t.Fatalf("status = %+v", st)
	}
}

func TestPausedPhoneIsResumed(t *testing.T) {
	r, next := playing("Paused"), &fakeMedia{}
	p := &Player{Next: next, Remote: r}
	p.Control(context.Background(), models.MediaPlay)
	if !slices.Equal(r.calls, []string{"Play"}) {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestIdleAirPlayDelegates(t *testing.T) {
	ctx := context.Background()
	for name, r := range map[string]*fakeRemote{
		"not running":  {},
		"no phone":     {running: true, props: map[string]any{"Available": false}},
		"disconnected": {running: true, props: map[string]any{"Available": true, "PlayerState": "Paused", "Client": ""}},
	} {
		next := &fakeMedia{}
		p := &Player{Next: next, Remote: r}
		if err := p.Control(ctx, models.MediaPause); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st, _ := p.Status(ctx); st.Title != "next" {
			t.Errorf("%s: status from %+v", name, st)
		}
		if len(r.calls) != 0 || !slices.Equal(next.ops, []string{"pause"}) {
			t.Errorf("%s: airplay %v, next %v", name, r.calls, next.ops)
		}
	}
}

func TestOtherMusicPausesAirPlay(t *testing.T) {
	r, next := playing("Playing"), &fakeMedia{}
	p := searcher{&Player{Next: next, Remote: r}}
	if err := p.PlaySearch(context.Background(), "кино"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls, []string{"Pause"}) || !slices.Equal(next.ops, []string{"search кино"}) {
		t.Fatalf("airplay %v, next %v", r.calls, next.ops)
	}
}

func TestWrapKeepsSearchCapability(t *testing.T) {
	if _, ok := Wrap(&fakeMedia{}, nil).(controller.MediaSearcher); !ok {
		t.Error("a searching player lost search")
	}
	if _, ok := Wrap(plainMedia{}, nil).(controller.MediaSearcher); ok {
		t.Error("a plain player gained search")
	}
}

type plainMedia struct{}

func (plainMedia) Control(context.Context, models.MediaOperation) error { return nil }
func (plainMedia) PlayURL(context.Context, string) error                { return nil }
func (plainMedia) Status(context.Context) (models.MediaStatus, error) {
	return models.MediaStatus{}, nil
}
