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

// Package playerctl implements [controller.Media] for MPRIS players such as
// spotifyd via the playerctl utility.
package playerctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// Player controls MPRIS players.
type Player struct {
	// Name restricts control to one player, e.g. "spotifyd"; empty uses
	// playerctl's default choice.
	Name string
	// Runner executes playerctl. Defaults to [syscmd.Exec] with the D-Bus
	// session address of the running players (see [FindBus]).
	Runner syscmd.Runner

	mu  sync.Mutex
	bus string // cached D-Bus address
}

var _ controller.Media = (*Player)(nil)

func (p *Player) run(ctx context.Context, args ...string) (string, error) {
	if p.Name != "" {
		args = append([]string{"-p", p.Name}, args...)
	}
	out, err := p.runner().Run(ctx, "playerctl", args...)
	if err != nil && strings.Contains(err.Error(), "D-Bus") {
		p.mu.Lock()
		p.bus = "" // the player may have restarted; look again next time
		p.mu.Unlock()
	}
	return out, err
}

func (p *Player) runner() syscmd.Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return p.exec()
}

// exec returns a runner that can reach the players' D-Bus.
func (p *Player) exec() syscmd.Exec {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return syscmd.Exec{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bus == "" {
		p.bus = FindBus("/", p.Name)
	}
	if p.bus == "" {
		return syscmd.Exec{}
	}
	return syscmd.Exec{Env: []string{"DBUS_SESSION_BUS_ADDRESS=" + p.bus}}
}

// FindBus finds the D-Bus address MPRIS players listen on, for services
// that, like the agent, run without a desktop session. In order:
//
//  1. DBUS_SESSION_BUS_ADDRESS of a running player process (player, or
//     spotifyd/mpd/mpv), then of any process;
//  2. the per-user bus socket /run/user/<uid>/bus;
//  3. the system bus (spotifyd with dbus_type = "system").
//
// root is the filesystem root ("/" except in tests). It returns "" if
// nothing is found.
func FindBus(root, player string) string {
	players := []string{"spotifyd", "mpd", "mpv"}
	if player != "" {
		players = append([]string{player}, players...)
	}
	const key = "DBUS_SESSION_BUS_ADDRESS="
	var fromPlayer, fromAny string
	envs, _ := filepath.Glob(filepath.Join(root, "proc", "[0-9]*", "environ"))
	for _, path := range envs {
		env, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var addr string
		for _, kv := range bytes.Split(env, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte(key)); ok && len(v) > 0 {
				addr = string(v)
			}
		}
		if addr == "" {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "comm"))
		name := strings.TrimSpace(string(comm))
		for _, pl := range players {
			if strings.HasPrefix(name, pl) && fromPlayer == "" {
				fromPlayer = addr
			}
		}
		if fromAny == "" {
			fromAny = addr
		}
	}
	switch {
	case fromPlayer != "":
		return fromPlayer
	case fromAny != "":
		return fromAny
	}
	sockets := []string{
		filepath.Join(root, "run", "user", fmt.Sprint(os.Getuid()), "bus"),
		filepath.Join(root, "run", "dbus", "system_bus_socket"),
		filepath.Join(root, "var", "run", "dbus", "system_bus_socket"),
	}
	for _, s := range sockets {
		if _, err := os.Stat(s); err == nil {
			rel, _ := filepath.Rel(root, s)
			return "unix:path=/" + rel
		}
	}
	return ""
}

var ops = map[models.MediaOperation]string{
	models.MediaPlay:     "play",
	models.MediaPause:    "pause",
	models.MediaToggle:   "play-pause",
	models.MediaStop:     "stop",
	models.MediaNext:     "next",
	models.MediaPrevious: "previous",
}

// Control implements [controller.Media].
func (p *Player) Control(ctx context.Context, op models.MediaOperation) error {
	cmd, ok := ops[op]
	if !ok {
		return fmt.Errorf("unsupported operation %q", op)
	}
	_, err := p.run(ctx, cmd)
	return err
}

// PlayURL implements [controller.Media]. Support depends on the player:
// spotifyd only opens spotify: URIs.
func (p *Player) PlayURL(ctx context.Context, url string) error {
	if strings.HasPrefix(url, "spotify:") {
		return p.openSpotify(ctx, url)
	}
	_, err := p.run(ctx, "open", url)
	if err != nil && (strings.Contains(err.Error(), "No players") || strings.Contains(p.Name, "spotifyd")) {
		return fmt.Errorf("%w (the MPRIS player can only open spotify: links; web streams and YouTube need mpv)", err)
	}
	return err
}

// spotifydWait bounds how long spotifyd takes to show its MPRIS player
// after TransferPlayback, and to start a track it was asked to open.
var spotifydWait = 5 * time.Second

// openSpotify opens a spotify: link in spotifyd and checks that it plays.
//
// spotifyd 0.4 ignores links while it is not the active Spotify Connect
// device (after a stop, or until someone picked it in a Spotify app), and
// its MPRIS player may be gone or left behind in that state; its own
// rs.spotifyd.Controls interface activates it without a phone. It also
// opens a track as its album from the track's position and gets that
// position wrong (one too far, seen with 0.4.2), so the next song plays.
func (p *Player) openSpotify(ctx context.Context, url string) error {
	id, isTrack := strings.CutPrefix(url, "spotify:track:")
	var err error
	for range 2 {
		if !p.spotifydActive(ctx) {
			p.wakeSpotifyd(ctx)
		}
		before := p.trackID(ctx)
		if isTrack && strings.Contains(before, id) {
			before = "" // the same song again: any track means it plays
		}
		if _, err = p.run(ctx, "open", url); err != nil {
			continue
		}
		cur := p.waitTrack(ctx, before)
		if cur == "" && p.spotifydActive(ctx) {
			// Taken, but it landed on the song that played before.
			cur = p.trackID(ctx)
		}
		if cur == "" {
			err = errors.New("spotifyd did not start playing; is it signed in?")
			continue
		}
		if isTrack {
			p.fixSpotifydTrack(ctx, id, cur)
		}
		return nil
	}
	return err
}

// spotifydActive reports whether spotifyd is the active device: its MPRIS
// player is up and not stopped.
func (p *Player) spotifydActive(ctx context.Context) bool {
	if !p.hasSpotifyd(ctx) {
		return false
	}
	st, err := p.run(ctx, "status")
	return err == nil && st != "Stopped"
}

// wakeSpotifyd makes spotifyd the active Spotify Connect device. Failures
// are left to the following open to report.
func (p *Player) wakeSpotifyd(ctx context.Context) {
	r := p.runner()
	names, err := r.Run(ctx, "dbus-send", "--session", "--print-reply", "--dest=org.freedesktop.DBus",
		"/org/freedesktop/DBus", "org.freedesktop.DBus.ListNames")
	if err != nil {
		return
	}
	var dest string
	for _, f := range strings.Fields(names) {
		if n := strings.Trim(f, `"`); strings.HasPrefix(n, "rs.spotifyd.") {
			dest = n
		}
	}
	if dest == "" {
		return
	}
	if _, err := r.Run(ctx, "dbus-send", "--session", "--print-reply", "--dest="+dest,
		"/rs/spotifyd/Controls", "rs.spotifyd.Controls.TransferPlayback"); err != nil {
		return
	}
	deadline := time.Now().Add(spotifydWait)
	for !p.spotifydActive(ctx) && time.Now().Before(deadline) {
		if !sleep(ctx, 200*time.Millisecond) {
			return
		}
	}
}

// fixSpotifydTrack steps back to track id if spotifyd started cur instead.
func (p *Player) fixSpotifydTrack(ctx context.Context, id, cur string) {
	for range 2 {
		if strings.Contains(cur, id) {
			return
		}
		if _, err := p.run(ctx, "previous"); err != nil {
			return
		}
		if cur = p.waitTrack(ctx, cur); cur == "" {
			return
		}
	}
}

// trackID returns spotifyd's mpris:trackid, "" when there is none.
func (p *Player) trackID(ctx context.Context) string {
	out, err := p.run(ctx, "metadata", "mpris:trackid")
	if err != nil || strings.HasSuffix(out, "/NoTrack") {
		return ""
	}
	return out
}

// waitTrack waits until spotifyd plays a track other than prev and returns
// its mpris:trackid, or "" if none comes in time.
func (p *Player) waitTrack(ctx context.Context, prev string) string {
	deadline := time.Now().Add(spotifydWait)
	for {
		if cur := p.trackID(ctx); cur != "" && cur != prev {
			return cur
		}
		if time.Now().After(deadline) || !sleep(ctx, 200*time.Millisecond) {
			return ""
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// hasSpotifyd reports whether spotifyd's MPRIS player is up.
func (p *Player) hasSpotifyd(ctx context.Context) bool {
	out, err := p.runner().Run(ctx, "playerctl", "-l")
	if err != nil {
		return false
	}
	for _, l := range strings.Fields(out) {
		if strings.HasPrefix(l, "spotifyd") {
			return true
		}
	}
	return false
}

// Status implements [controller.Media].
func (p *Player) Status(ctx context.Context) (models.MediaStatus, error) {
	var s models.MediaStatus
	out, err := p.run(ctx, "status")
	if err != nil {
		if strings.Contains(err.Error(), "No players found") {
			return s, nil
		}
		return s, err
	}
	s.Playing = out == "Playing"
	if title, err := p.run(ctx, "metadata", "--format", "{{ artist }} - {{ title }}"); err == nil {
		s.Title = strings.Trim(title, " -")
	}
	return s, nil
}
