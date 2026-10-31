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

// Command airplay is an example plugin with both halves in one Go binary:
// MARUSYA_PLUGIN_HOST tells which half to run.
//
// The speaker half reads the metadata pipe of shairport-sync (the AirPlay
// receiver the panel installs on speakers), lights the LED ring while a
// phone is connected or plays, keeps the phone's volume slider and the
// speaker's volume together (unless capsule-mini-tweaker runs: then the
// bundle's ap2-bridge does it) and sends what plays to the server half. The server half
// remembers it for every speaker and gives the model the airplay_status
// function: "Маруся, что играет по эйрплею на кухне?".
//
// The speaker needs metadata on in /etc/shairport-sync.conf, and the
// receiver's own software volume off so the slider moves only the speaker's
// volume (the panel's installer sets both):
//
//	metadata = { enabled = "yes"; include_cover_art = "no"; pipe_name = "/tmp/shairport-sync-metadata"; };
//	general = { ignore_volume_control = "yes"; ... };
//
// The speaker and the server differ in architecture, so the same code is
// built twice (see plugin.json):
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o examples/plugins/airplay/airplay-agent ./examples/plugins/airplay
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o examples/plugins/airplay/airplay-server ./examples/plugins/airplay
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// Status is what the speaker half sends to the server half.
type Status struct {
	// Connected means a phone has chosen the speaker, playing or not.
	Connected bool   `json:"connected,omitempty"`
	Playing   bool   `json:"playing"`
	Paused    bool   `json:"paused,omitempty"`
	Title     string `json:"title,omitempty"`
	Artist    string `json:"artist,omitempty"`
	Album     string `json:"album,omitempty"`
	Client    string `json:"client,omitempty"` // the phone's name
}

func main() {
	p := pluginsdk.New()
	if os.Getenv("MARUSYA_PLUGIN_HOST") == pluginsdk.HostAgent {
		speaker(p)
	} else {
		server(p)
	}
	if err := p.Run(); err != nil {
		log.Fatal(err)
	}
}

// speaker runs on the speaker: it watches shairport-sync.
func speaker(p *pluginsdk.Plugin) {
	cfg := struct {
		Pipe  string `json:"metadata_pipe"`
		Color string `json:"color"`
	}{Pipe: "/tmp/shairport-sync-metadata", Color: "ffffff"}
	vol := NewVolumes()
	vol.Bridged = bridged
	p.OnInit(func(context.Context, pluginsdk.Init) error {
		if err := p.Config(&cfg); err != nil {
			return err
		}
		if bridged() {
			p.Log("info", "%s found: the AirPlay bundle syncs the volume, the plugin only lights the ring", tweakerSocket)
		}
		go follow(p, cfg.Pipe, cfg.Color, vol)
		return nil
	})
	// Voice, keys and the panel move the phone's slider too.
	p.OnEvent(func(ctx context.Context, e pluginsdk.Event) {
		level, _ := e.Data["level"].(float64)
		if muted, _ := e.Data["muted"].(bool); muted {
			level = 0
		}
		if !vol.speaker(int(level)) {
			return
		}
		if err := setPhoneVolume(ctx, int(level)); err != nil {
			p.Log("warn", "phone volume: %v", err)
		}
	}, pluginsdk.EventVolume)
}

// tweakerSocket is the IPC of capsule-mini-tweaker. With it the AirPlay
// bundle installs its own two-way volume bridge (ap2-bridge), and two
// bridges would answer each other's echoes.
const tweakerSocket = "/run/capsule-controls.sock"

// bridged reports whether the bundle's bridge syncs the volume already.
func bridged() bool {
	fi, err := os.Stat(tweakerSocket)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// Volumes keeps the phone's slider and the speaker's volume together
// without sending a change back to where it came from.
type Volumes struct {
	// Bridged, when it reports true, leaves the volume to someone else.
	Bridged func() bool

	mu        sync.Mutex
	level     int // the level both sides have; -1 when unknown
	connected bool
}

// NewVolumes returns Volumes before any phone connected.
func NewVolumes() *Volumes { return &Volumes{level: -1} }

func (v *Volumes) off() bool { return v.Bridged != nil && v.Bridged() }

// phone reports whether a level from the phone's slider is new to the
// speaker and has to be set.
func (v *Volumes) phone(level int) bool {
	if v.off() {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if level == v.level {
		return false
	}
	v.level = level
	return true
}

// speaker reports whether a new level of the speaker has to go to the
// phone: one is connected and the level did not come from it.
func (v *Volumes) speaker(level int) bool {
	if v.off() {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.connected || level == v.level {
		return false
	}
	v.level = level
	return true
}

// setConnected follows the phone; the next phone starts afresh.
func (v *Volumes) setConnected(c bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !c {
		v.level = -1
	}
	v.connected = c
}

// setPhoneVolume moves the phone's slider through shairport-sync's D-Bus
// interface; dbus-send is on every speaker with the receiver.
func setPhoneVolume(ctx context.Context, level int) error {
	out, err := exec.CommandContext(ctx, "dbus-send", "--system", "--print-reply", "--dest=org.gnome.ShairportSync",
		"/org/gnome/ShairportSync", "org.gnome.ShairportSync.RemoteControl.SetAirplayVolume",
		"double:"+strconv.FormatFloat(AirPlayDB(level), 'f', 2, 64)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// follow reads the pipe forever: it ends whenever shairport-sync restarts.
func follow(p *pluginsdk.Plugin, pipe, color string, vol *Volumes) {
	ctx := context.Background()
	var st Status
	for {
		f, err := os.Open(pipe) // a FIFO: blocks until shairport-sync opens it
		if err != nil {
			p.Log("warn", "%v; is metadata on in shairport-sync.conf?", err)
			time.Sleep(30 * time.Second)
			continue
		}
		err = Watch(f, func(it Item) {
			if level, ok := Volume(it); ok {
				if vol.phone(level) {
					if _, err := p.Command(ctx, "", "volume.set", map[string]any{"level": level}); err != nil {
						p.Log("warn", "volume: %v", err)
					}
				}
				return
			}
			if !st.apply(it) {
				return
			}
			vol.setConnected(st.Connected)
			light(ctx, p, st, color)
			if err := p.Send(ctx, "", st); err != nil {
				p.Log("warn", "send: %v", err)
			}
		})
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			p.Log("warn", "metadata: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// light shows playback on the ring and the display: speaker actions run
// through host.command as if the server sent them. A phone that has chosen
// the speaker but does not play keeps a dim ring.
func light(ctx context.Context, p *pluginsdk.Plugin, st Status, color string) {
	var err error
	switch {
	case st.Playing && !st.Paused:
		_, err = p.Command(ctx, "", "ring.set", map[string]any{"scene": "breathe", "color": color, "brightness": 30, "seconds": 12 * 3600})
		if err == nil {
			_, err = p.Command(ctx, "", "display.show", map[string]any{"mode": "text", "text": "AIR", "seconds": 5})
		}
	case st.Connected:
		_, err = p.Command(ctx, "", "ring.set", map[string]any{"scene": "solid", "color": color, "brightness": 8, "seconds": 12 * 3600})
	default:
		_, err = p.Command(ctx, "", "ring.reset", nil)
	}
	if err != nil {
		p.Log("warn", "ring: %v", err)
	}
}

// Item is one metadata item of shairport-sync: Type and Code are four
// letters, such as "ssnc" "pbeg" (playback begins) or "core" "minm" (title).
type Item struct {
	Type, Code string
	Data       []byte
}

type rawItem struct {
	Type string `xml:"type"`
	Code string `xml:"code"`
	Data string `xml:"data"`
}

// Watch decodes the items read from r until it ends.
func Watch(r io.Reader, f func(Item)) error {
	d := xml.NewDecoder(r)
	d.Strict = false
	for {
		var raw rawItem
		if err := d.Decode(&raw); err != nil {
			return err
		}
		t, err1 := hex.DecodeString(strings.TrimSpace(raw.Type))
		c, err2 := hex.DecodeString(strings.TrimSpace(raw.Code))
		if err1 != nil || err2 != nil {
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw.Data), ""))
		f(Item{Type: string(t), Code: string(c), Data: data})
	}
}

// apply updates st from it and reports whether st changed.
func (st *Status) apply(it Item) bool {
	old := *st
	s := string(it.Data)
	switch it.Type + "/" + it.Code {
	case "ssnc/abeg", "ssnc/conn":
		st.Connected = true
	case "ssnc/aend", "ssnc/disc":
		*st = Status{}
	case "ssnc/pbeg", "ssnc/prsm":
		st.Connected, st.Playing, st.Paused = true, true, false
	case "ssnc/pend":
		// The phone stopped streaming but may still have the speaker chosen.
		*st = Status{Connected: st.Connected, Client: st.Client}
	case "ssnc/pfls", "ssnc/paus":
		st.Paused = true
	case "ssnc/snam":
		st.Client = s
	case "core/minm":
		st.Title = s
	case "core/asar":
		st.Artist = s
	case "core/asal":
		st.Album = s
	}
	return *st != old
}

// Volume returns the speaker's level, 0..100, for a "pvol" item. Its data
// is "airplay,volume,lowest,highest" in dB; the AirPlay volume runs from
// -30 (quietest) to 0, and -144 means muted.
func Volume(it Item) (int, bool) {
	if it.Type != "ssnc" || it.Code != "pvol" {
		return 0, false
	}
	first, _, _ := strings.Cut(string(it.Data), ",")
	db, err := strconv.ParseFloat(strings.TrimSpace(first), 64)
	if err != nil {
		return 0, false
	}
	if db <= -144 {
		return 0, true
	}
	return int(math.Round(max(0, min(100, (db+30)*100/30)))), true
}

// AirPlayDB is the AirPlay volume in dB for the speaker's level: the
// inverse of [Volume].
func AirPlayDB(level int) float64 {
	if level <= 0 {
		return -144
	}
	return float64(min(level, 100))*0.3 - 30
}

// server runs on the server: it collects the speakers' statuses.
func server(p *pluginsdk.Plugin) {
	var mu sync.Mutex
	playing := map[string]Status{}
	p.OnMessage(func(_ context.Context, m pluginsdk.Message) {
		var st Status
		if json.Unmarshal(m.Data, &st) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if st.Playing {
			playing[m.Device] = st
		} else {
			delete(playing, m.Device)
		}
	})
	// A speaker that went away does not play.
	p.OnEvent(func(_ context.Context, e pluginsdk.Event) {
		mu.Lock()
		delete(playing, e.Device)
		mu.Unlock()
	}, pluginsdk.EventDisconnect)
	p.Tool(pluginsdk.ToolSpec{
		Name: "airplay_status",
		Description: "Tells what phones play to the speakers over AirPlay: the track, the artist and the phone, for every speaker. " +
			"Use it when the user asks what is playing over AirPlay or from whose phone.",
	}, func(_ context.Context, c pluginsdk.Call) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]Status{}
		for d, st := range playing {
			out[d] = st
		}
		return map[string]any{"speakers": out, "asked_from": c.Device.ID}, nil
	})
}
