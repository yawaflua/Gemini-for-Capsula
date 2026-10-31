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
// phone plays and sends what plays to the server half. The server half
// remembers it for every speaker and gives the model the airplay_status
// function: "Маруся, что играет по эйрплею на кухне?".
//
// The speaker needs metadata on in /etc/shairport-sync.conf:
//
//	metadata = { enabled = "yes"; include_cover_art = "no"; pipe_name = "/tmp/shairport-sync-metadata"; };
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
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// Status is what the speaker half sends to the server half.
type Status struct {
	Playing bool   `json:"playing"`
	Paused  bool   `json:"paused,omitempty"`
	Title   string `json:"title,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Album   string `json:"album,omitempty"`
	Client  string `json:"client,omitempty"` // the phone's name
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
	p.OnInit(func(context.Context, pluginsdk.Init) error {
		if err := p.Config(&cfg); err != nil {
			return err
		}
		go follow(p, cfg.Pipe, cfg.Color)
		return nil
	})
}

// follow reads the pipe forever: it ends whenever shairport-sync restarts.
func follow(p *pluginsdk.Plugin, pipe, color string) {
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
			if !st.apply(it) {
				return
			}
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
// through host.command as if the server sent them.
func light(ctx context.Context, p *pluginsdk.Plugin, st Status, color string) {
	var err error
	switch {
	case st.Playing && !st.Paused:
		_, err = p.Command(ctx, "", "ring.set", map[string]any{"scene": "breathe", "color": color, "brightness": 30, "seconds": 12 * 3600})
		if err == nil {
			_, err = p.Command(ctx, "", "display.show", map[string]any{"mode": "text", "text": "AIR", "seconds": 5})
		}
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
	case "ssnc/pbeg", "ssnc/prsm":
		st.Playing, st.Paused = true, false
	case "ssnc/pend":
		*st = Status{}
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
