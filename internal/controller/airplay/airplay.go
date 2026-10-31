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

// Package airplay makes AirPlay 2 music a [controller.Media] source.
//
// AirPlay is received by shairport-sync (with nqptp and avahi, e.g. the
// build of github.com/ryysel/capsula-mini-airplay2). The agent controls it
// through its D-Bus interface on the system bus: while a phone is playing
// to the speaker, pause, next track and status go to the phone (over DACP);
// otherwise commands go to the next media player.
package airplay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// shairport-sync D-Bus names.
const (
	BusName       = "org.gnome.ShairportSync"
	ObjectPath    = "/org/gnome/ShairportSync"
	RemoteControl = BusName + ".RemoteControl"
)

// Source names AirPlay in [models.MediaStatus.Source].
const Source = "airplay"

// Remote is the part of shairport-sync's D-Bus interface the player uses.
// [SystemBus] implements it; tests use a fake.
type Remote interface {
	// Call invokes a RemoteControl method.
	Call(ctx context.Context, method string, args ...any) error
	// Property reads a RemoteControl property.
	Property(ctx context.Context, name string) (dbus.Variant, error)
}

// ErrUnavailable means shairport-sync is not running.
var ErrUnavailable = errors.New("airplay: shairport-sync is not running")

// Player routes media commands to AirPlay while a phone plays to the
// speaker and to Next otherwise.
type Player struct {
	// Next handles everything that is not AirPlay; may be nil.
	Next controller.Media
	// Remote reaches shairport-sync; nil uses [SystemBus].
	Remote Remote
	Logger *slog.Logger

	once sync.Once
}

var _ controller.Media = (*Player)(nil)

// searcher is a [Player] whose next player can search for music.
type searcher struct{ *Player }

var _ controller.MediaSearcher = searcher{}

// Wrap returns next with AirPlay in front of it. The result can search for
// music only if next can.
func Wrap(next controller.Media, log *slog.Logger) controller.Media {
	p := &Player{Next: next, Logger: log}
	if _, ok := next.(controller.MediaSearcher); ok {
		return searcher{p}
	}
	return p
}

func (p *Player) remote() Remote {
	p.once.Do(func() {
		if p.Remote == nil {
			p.Remote = &SystemBus{}
		}
	})
	return p.Remote
}

// State is what shairport-sync is doing.
type State struct {
	// Available means shairport-sync runs.
	Available bool
	// PlayerState is "Playing", "Paused" or "Stopped".
	PlayerState string
	// Client is the address of the connected phone.
	Client string
	Title  string
	Artist string
	Album  string
}

// Playing reports whether music is being played to the speaker.
func (s State) Playing() bool { return s.Available && s.PlayerState == "Playing" }

// Engaged reports whether AirPlay owns the playback: playing, or paused
// with a phone still connected (so "продолжи" resumes it).
func (s State) Engaged() bool {
	return s.Available && (s.PlayerState == "Playing" || (s.PlayerState == "Paused" && s.Client != ""))
}

// Track returns "Artist — Title", or whatever of it is known.
func (s State) Track() string {
	switch {
	case s.Artist != "" && s.Title != "":
		return s.Artist + " — " + s.Title
	case s.Title != "":
		return s.Title
	}
	return s.Artist
}

// State reads the state of shairport-sync. A shairport-sync that is not
// running is reported as unavailable, not as an error.
func (p *Player) State(ctx context.Context) (State, error) {
	r := p.remote()
	var st State
	// This shairport-sync build reports Available=false even while a phone
	// plays; PlayerState and Client are the reliable signals. A missing bus
	// name still means shairport-sync is not running.
	v, err := r.Property(ctx, "PlayerState")
	if errors.Is(err, ErrUnavailable) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Available = true
	st.PlayerState, _ = v.Value().(string)
	if v, err := r.Property(ctx, "Client"); err == nil {
		st.Client, _ = v.Value().(string)
	}
	if v, err := r.Property(ctx, "Metadata"); err == nil {
		md, _ := v.Value().(map[string]dbus.Variant)
		st.Title = str(md["xesam:title"])
		st.Album = str(md["xesam:album"])
		st.Artist = str(md["xesam:artist"])
	}
	return st, nil
}

// str returns a string or the first string of an array variant.
func str(v dbus.Variant) string {
	switch x := v.Value().(type) {
	case string:
		return x
	case []string:
		return strings.Join(x, ", ")
	}
	return ""
}

var methods = map[models.MediaOperation]string{
	models.MediaPlay:     "Play",
	models.MediaPause:    "Pause",
	models.MediaToggle:   "PlayPause",
	models.MediaStop:     "Stop",
	models.MediaNext:     "Next",
	models.MediaPrevious: "Previous",
}

// Control implements [controller.Media].
func (p *Player) Control(ctx context.Context, op models.MediaOperation) error {
	st, err := p.State(ctx)
	if err != nil {
		p.log().Debug("airplay state", "err", err)
	}
	if !st.Engaged() {
		if p.Next == nil {
			return errors.New("нечего воспроизводить")
		}
		return p.Next.Control(ctx, op)
	}
	m, ok := methods[op]
	if !ok {
		return fmt.Errorf("airplay: unsupported operation %q", op)
	}
	if err := p.remote().Call(ctx, m); err != nil {
		return fmt.Errorf("airplay: %s: %w", m, err)
	}
	return nil
}

// PlayURL implements [controller.Media]: AirPlay is paused so the two do
// not play over each other.
func (p *Player) PlayURL(ctx context.Context, url string) error {
	if p.Next == nil {
		return errors.New("плеер не настроен")
	}
	p.pause(ctx)
	return p.Next.PlayURL(ctx, url)
}

// PlaySearch implements [controller.MediaSearcher].
func (s searcher) PlaySearch(ctx context.Context, query string) error {
	s.pause(ctx)
	return s.Next.(controller.MediaSearcher).PlaySearch(ctx, query)
}

func (p *Player) pause(ctx context.Context) {
	if st, err := p.State(ctx); err == nil && st.Playing() {
		if err := p.remote().Call(ctx, "Pause"); err != nil {
			p.log().Warn("pause airplay", "err", err)
		}
	}
}

// Status implements [controller.Media].
func (p *Player) Status(ctx context.Context) (models.MediaStatus, error) {
	if st, err := p.State(ctx); err == nil && st.Engaged() {
		return models.MediaStatus{Playing: st.Playing(), Title: st.Track(), Source: Source}, nil
	}
	if p.Next == nil {
		return models.MediaStatus{}, nil
	}
	return p.Next.Status(ctx)
}

func (p *Player) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// SystemBus reaches shairport-sync on the system D-Bus. It connects lazily
// and reconnects after the bus or shairport-sync restarts.
type SystemBus struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

func (b *SystemBus) object(ctx context.Context) (dbus.BusObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil || !b.conn.Connected() {
		c, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("airplay: system bus: %w", err)
		}
		b.conn = c
	}
	var has bool
	if err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, BusName).Store(&has); err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrUnavailable
	}
	return b.conn.Object(BusName, ObjectPath), nil
}

// Call implements [Remote].
func (b *SystemBus) Call(ctx context.Context, method string, args ...any) error {
	o, err := b.object(ctx)
	if err != nil {
		return err
	}
	return o.CallWithContext(ctx, RemoteControl+"."+method, 0, args...).Err
}

// Property implements [Remote].
func (b *SystemBus) Property(ctx context.Context, name string) (dbus.Variant, error) {
	o, err := b.object(ctx)
	if err != nil {
		return dbus.Variant{}, err
	}
	var v dbus.Variant
	err = o.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, RemoteControl, name).Store(&v)
	return v, err
}
