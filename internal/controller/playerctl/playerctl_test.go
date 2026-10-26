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

package playerctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeSpotifyd is spotifyd 0.4: it ignores links while not the active
// device, activates on TransferPlayback, and opens a track one position too
// far in its album.
type fakeSpotifyd struct {
	mpris  bool // its MPRIS player is on the bus
	active bool
	dead   bool // TransferPlayback does nothing
	calls  []string
	album  []string
	pos    int // -1: nothing playing
}

func (f *fakeSpotifyd) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "playerctl -l":
		if f.mpris {
			return "spotifyd.instance42", nil
		}
		return "", nil
	case call == "playerctl -p spotifyd status":
		if f.active {
			return "Playing", nil
		}
		return "Stopped", nil
	case strings.HasSuffix(call, "ListNames"):
		return "method return\n   array [\n      string \"org.freedesktop.DBus\"\n      string \"rs.spotifyd.instance42\"\n   ]", nil
	case strings.HasSuffix(call, "TransferPlayback"):
		if !strings.Contains(call, "--dest=rs.spotifyd.instance42 /rs/spotifyd/Controls") {
			panic(call)
		}
		if !f.dead {
			f.mpris, f.active = true, true
		}
	case strings.HasPrefix(call, "playerctl -p spotifyd open spotify:track:"):
		if !f.active {
			return "", nil // "will be ignored while Not Active"
		}
		id := strings.TrimPrefix(call, "playerctl -p spotifyd open spotify:track:")
		for i, t := range f.album {
			if t == id {
				f.pos = min(i+1, len(f.album)-1)
			}
		}
	case call == "playerctl -p spotifyd previous":
		f.pos = max(f.pos-1, 0)
	case call == "playerctl -p spotifyd metadata mpris:trackid":
		if f.pos < 0 {
			return "/org/mpris/MediaPlayer2/TrackList/NoTrack", nil
		}
		return "/org/spotify/track/" + f.album[f.pos], nil
	}
	return "", nil
}

func (f *fakeSpotifyd) called(s string) bool { return strings.Contains(strings.Join(f.calls, ","), s) }

func TestOpenSpotify(t *testing.T) {
	defer func(d time.Duration) { spotifydWait = d }(spotifydWait)
	spotifydWait = 300 * time.Millisecond
	ctx := context.Background()

	// Never picked in an app: no MPRIS player until activated.
	f := &fakeSpotifyd{album: []string{"t1", "t2", "t3"}, pos: -1}
	p := &Player{Name: "spotifyd", Runner: f}
	if err := p.PlayURL(ctx, "spotify:track:t1"); err != nil {
		t.Fatal(err)
	}
	if f.pos != 0 || !f.called("TransferPlayback") || !f.called("previous") {
		t.Errorf("cold start: pos %d, calls %q", f.pos, f.calls)
	}

	// Active: no activation, the last track opens right.
	f.calls = nil
	if err := p.PlayURL(ctx, "spotify:track:t3"); err != nil || f.pos != 2 || f.called("TransferPlayback") || f.called("previous") {
		t.Errorf("active: %v, pos %d, calls %q", err, f.pos, f.calls)
	}

	// Stopped, its MPRIS player left behind: it must be activated again.
	f.active, f.calls = false, nil
	if err := p.PlayURL(ctx, "spotify:track:t2"); err != nil || f.pos != 1 || !f.called("TransferPlayback") {
		t.Errorf("inactive: %v, pos %d, calls %q", err, f.pos, f.calls)
	}

	// Cannot be activated: an error, so the server plays it elsewhere.
	f.active, f.dead = false, true
	if err := p.PlayURL(ctx, "spotify:track:t1"); err == nil {
		t.Error("a link spotifyd ignored must fail")
	}

	// Other links never touch spotifyd.
	f.calls = nil
	p.PlayURL(ctx, "https://example.com/a.mp3")
	if len(f.calls) != 1 {
		t.Errorf("http link: calls %q", f.calls)
	}
}
