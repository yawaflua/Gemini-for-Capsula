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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/plugin"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// speakerPlugins runs the plugins on the speaker. It connects them to the
// agent ([agent.Plugins]), serves their requests ([plugin.Handler]) and
// tells them the assistant state ([indicator.Indicator]).
type speakerPlugins struct {
	m          *plugin.Manager
	dispatcher *controller.Dispatcher
	dev        pluginsdk.Device
	agent      atomic.Pointer[agent.Agent]

	mu    sync.Mutex
	tools string // names of the announced tools
}

func newSpeakerPlugins(dir string, dev pluginsdk.Device, d *controller.Dispatcher, log *slog.Logger) *speakerPlugins {
	p := &speakerPlugins{dispatcher: d, dev: dev}
	p.m = plugin.New(plugin.Config{
		Dir:         dir,
		Host:        pluginsdk.HostAgent,
		HostVersion: version,
		Device:      &p.dev,
		Handler:     p,
		Reserved: func(name string) bool {
			_, ok := tools.Lookup(name)
			return ok
		},
		OnChange: p.changed,
		Logger:   log,
	})
	return p
}

// start runs the plugins; it returns once they are initialized, so their
// tools are in the first hello.
func (p *speakerPlugins) start(ctx context.Context, a *agent.Agent) {
	p.m.Start(ctx)
	p.mu.Lock()
	p.tools = p.names()
	p.mu.Unlock()
	p.agent.Store(a)
}

func (p *speakerPlugins) names() string {
	var names []string
	for _, t := range p.m.Tools() {
		names = append(names, t.Name)
	}
	return strings.Join(names, ",")
}

// changed reconnects to the server when the tools changed, e.g. a plugin
// crashed or came back.
func (p *speakerPlugins) changed() {
	names := p.names()
	p.mu.Lock()
	changed := names != p.tools
	p.tools = names
	p.mu.Unlock()
	if a := p.agent.Load(); a != nil && changed {
		a.Reconnect("plugin functions changed")
	}
}

// Tools implements [agent.Plugins].
func (p *speakerPlugins) Tools() []models.PluginTool { return plugin.Announce(p.m.Tools()) }

// Rescan rereads the plugins directory after the agent installed or
// removed a plugin from the catalog.
func (p *speakerPlugins) Rescan() { p.m.Rescan() }

// Call implements [agent.Plugins].
func (p *speakerPlugins) Call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	return p.m.Call(ctx, name, args, p.dev)
}

// Deliver implements [agent.Plugins].
func (p *speakerPlugins) Deliver(plugin string, data json.RawMessage) error {
	return p.m.Deliver(plugin, p.dev.ID, data)
}

// Show implements [indicator.Indicator].
func (p *speakerPlugins) Show(st indicator.State) {
	p.m.Event(pluginsdk.Event{Kind: pluginsdk.EventState, Device: p.dev.ID, Data: map[string]any{"state": st.String()}})
}

// Volume tells the plugins that the speaker's volume changed; it is the
// OnChange of a [controller.ObservedVolume].
func (p *speakerPlugins) Volume(st models.VolumeState) {
	p.m.Event(pluginsdk.Event{Kind: pluginsdk.EventVolume, Device: p.dev.ID, Data: map[string]any{"level": st.Level, "muted": st.Muted}})
}

// Command implements [plugin.Handler]: actions run on this speaker.
func (p *speakerPlugins) Command(ctx context.Context, plugin, _, action string, args map[string]any) (map[string]any, error) {
	res := p.dispatcher.Execute(ctx, &models.Command{ID: "plugin-" + plugin, Action: models.Action(action), Args: args})
	if !res.OK {
		return nil, errors.New(res.Error)
	}
	return res.Data, nil
}

// Say implements [plugin.Handler].
func (p *speakerPlugins) Say(context.Context, string, string, string, bool) (string, error) {
	return "", errors.New("host.say есть только у серверной половины плагина")
}

// Devices implements [plugin.Handler].
func (p *speakerPlugins) Devices(context.Context) []pluginsdk.Device {
	return []pluginsdk.Device{p.dev}
}

// Send implements [plugin.Handler].
func (p *speakerPlugins) Send(ctx context.Context, plugin, _ string, data json.RawMessage) error {
	a := p.agent.Load()
	if a == nil {
		return errors.New("агент ещё не запущен")
	}
	return a.SendPlugin(ctx, plugin, data)
}
