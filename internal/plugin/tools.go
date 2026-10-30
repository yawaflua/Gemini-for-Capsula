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

package plugin

import (
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/tools"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// ToolOf declares a plugin tool to the model. Its action is its name.
func ToolOf(spec pluginsdk.ToolSpec) tools.Tool {
	t := tools.Tool{Action: models.Action(spec.Name), Description: spec.Description}
	for _, p := range spec.Params {
		t.Parameters = append(t.Parameters, tools.Parameter{
			Name: p.Name, Type: tools.Type(p.Type), Description: p.Description,
			Required: p.Required, Enum: p.Enum, Minimum: p.Minimum, Maximum: p.Maximum,
		})
	}
	return t
}

// Announce converts the tools of the speaker's plugins for [models.Hello].
func Announce(ts []Tool) []models.PluginTool {
	out := make([]models.PluginTool, 0, len(ts))
	for _, t := range ts {
		pt := models.PluginTool{Plugin: t.Plugin, Name: t.Name, Description: t.Description}
		for _, p := range t.Params {
			pt.Params = append(pt.Params, models.PluginParam(p))
		}
		out = append(out, pt)
	}
	return out
}

// Announced converts a tool announced by a speaker back, checking it like
// a tool of a local plugin.
func Announced(pt models.PluginTool) (Tool, error) {
	t := Tool{Plugin: pt.Plugin, ToolSpec: pluginsdk.ToolSpec{Name: pt.Name, Description: pt.Description}}
	for _, p := range pt.Params {
		t.Params = append(t.Params, pluginsdk.Param(p))
	}
	return t, ValidTool(t.ToolSpec)
}
