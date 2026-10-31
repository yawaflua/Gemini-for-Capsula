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

package volumecmd

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	out   string
	err   error
	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, f.err
}

func TestGet(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		level int
		muted bool
	}{
		{"state", "42 unmuted", 42, false},
		{"muted", "7 muted", 7, true},
		{"no state", "55\n", 55, false},
		{"percent", "80% unmuted", 80, false},
		{"upper case", "12 MUTED", 12, true},
		{"clamp high", "140 unmuted", 100, false},
		{"clamp low", "-5 unmuted", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &fakeRunner{out: c.out}
			v := &Volume{Command: "volume", Runner: r}
			st, err := v.Get(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Level != c.level || st.Muted != c.muted {
				t.Fatalf("Get = %+v, want level %d muted %v", st, c.level, c.muted)
			}
			if len(r.calls) != 1 || r.calls[0][1] != "get" {
				t.Fatalf("calls = %v, want [volume get]", r.calls)
			}
		})
	}
}

func TestGetBadOutput(t *testing.T) {
	for _, out := range []string{"", "loud", "40 louder"} {
		r := &fakeRunner{out: out}
		v := &Volume{Command: "volume", Runner: r}
		if _, err := v.Get(context.Background()); err == nil {
			t.Fatalf("Get(%q) must fail", out)
		}
	}
}

func TestSetClamps(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{{42, "42"}, {150, "100"}, {-3, "0"}, {0, "0"}, {100, "100"}} {
		r := &fakeRunner{}
		v := &Volume{Command: "volume", Runner: r}
		if err := v.Set(context.Background(), c.in); err != nil {
			t.Fatal(err)
		}
		got := r.calls[0]
		if len(got) != 3 || got[1] != "set" || got[2] != c.want {
			t.Fatalf("Set(%d) ran %v, want set %s", c.in, got, c.want)
		}
	}
}

func TestSetMuted(t *testing.T) {
	for _, c := range []struct {
		muted bool
		want  string
	}{{true, "on"}, {false, "off"}} {
		r := &fakeRunner{}
		v := &Volume{Command: "volume", Runner: r}
		if err := v.SetMuted(context.Background(), c.muted); err != nil {
			t.Fatal(err)
		}
		got := r.calls[0]
		if len(got) != 3 || got[1] != "mute" || got[2] != c.want {
			t.Fatalf("SetMuted(%v) ran %v, want mute %s", c.muted, got, c.want)
		}
	}
}

func TestErrors(t *testing.T) {
	boom := errors.New("boom")
	v := &Volume{Command: "volume", Runner: &fakeRunner{err: boom}}
	if _, err := v.Get(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Get error = %v, want boom", err)
	}
	if err := v.Set(context.Background(), 10); !errors.Is(err, boom) {
		t.Fatalf("Set error = %v, want boom", err)
	}
	if err := v.SetMuted(context.Background(), true); !errors.Is(err, boom) {
		t.Fatalf("SetMuted error = %v, want boom", err)
	}

	empty := &Volume{Runner: &fakeRunner{out: "10 unmuted"}}
	if _, err := empty.Get(context.Background()); err == nil {
		t.Fatal("Get without a command must fail")
	}
}
