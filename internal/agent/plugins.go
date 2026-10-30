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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Plugins are the plugins running on the speaker (see internal/plugin).
type Plugins interface {
	// Tools are announced to the server in the hello.
	Tools() []models.PluginTool
	// Call runs a tool for the server.
	Call(ctx context.Context, name string, args map[string]any) (map[string]any, error)
	// Deliver passes data from the server half of a plugin.
	Deliver(plugin string, data json.RawMessage) error
}

var errOffline = errors.New("нет связи с сервером")

// SendPlugin sends data to the server half of plugin.
func (a *Agent) SendPlugin(ctx context.Context, plugin string, data json.RawMessage) error {
	conn := a.conn.Load()
	if conn == nil {
		return errOffline
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return conn.Send(ctx, &models.Message{Type: models.TypePlugin, Plugin: &models.PluginMessage{Plugin: plugin, Data: data}})
}

// Reconnect drops the connection to the server, which is reopened at once.
// The tools of the plugins are announced on connect, so this is how changed
// tools reach the server.
func (a *Agent) Reconnect(reason string) {
	if conn := a.conn.Load(); conn != nil {
		a.log.Info("reconnecting", "reason", reason)
		conn.Close(reason)
	}
}

// execute runs a command from the server: a device action, or a function
// of a plugin.
func (a *Agent) execute(ctx context.Context, cmd *models.Command) *models.CommandResult {
	if a.plugins != nil && !slices.Contains(a.dispatcher.Capabilities(), cmd.Action) {
		for _, t := range a.plugins.Tools() {
			if t.Name != string(cmd.Action) {
				continue
			}
			out, err := a.plugins.Call(ctx, t.Name, cmd.Args)
			if err != nil {
				return cmd.Failure(err)
			}
			return cmd.Success(out)
		}
	}
	return a.dispatcher.Execute(ctx, cmd)
}

// features lists the protocol extensions of the agent.
func (a *Agent) features() []models.Feature {
	fs := []models.Feature{models.FeatureRemoteTurn, models.FeatureServerWake, models.FeatureAnnounce, models.FeatureAlarms}
	if a.plugins != nil {
		fs = append(fs, models.FeaturePlugins)
	}
	return fs
}

func (a *Agent) pluginTools() []models.PluginTool {
	if a.plugins == nil {
		return nil
	}
	return a.plugins.Tools()
}

func (s *session) onPluginMessage(m *models.PluginMessage) {
	if m == nil || s.a.plugins == nil {
		return
	}
	if err := s.a.plugins.Deliver(m.Plugin, m.Data); err != nil {
		s.a.log.Debug("plugin message", "plugin", m.Plugin, "err", err)
	}
}
