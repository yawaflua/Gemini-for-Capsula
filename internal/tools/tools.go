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

// Package tools describes the device actions exposed to the language model
// as callable functions, in a provider-agnostic way.
//
// Each [Tool] maps one-to-one to a [models.Action]. Providers translate the
// declarations into their own function-calling schema.
package tools

import (
	"slices"
	"strings"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Type is a JSON schema primitive type.
type Type string

const (
	TypeString  Type = "string"
	TypeInteger Type = "integer"
	TypeBoolean Type = "boolean"
)

// Parameter is a single function argument.
type Parameter struct {
	Name        string
	Type        Type
	Description string
	Required    bool
	Enum        []string
	Minimum     *float64
	Maximum     *float64
}

// Tool is a function the model may call.
type Tool struct {
	Action      models.Action
	Description string
	Parameters  []Parameter
}

// Name returns the function name presented to the model.
func (t Tool) Name() string { return NameOf(t.Action) }

// NameOf converts an action into a function name ("volume.set" -> "volume_set").
func NameOf(a models.Action) string { return strings.ReplaceAll(string(a), ".", "_") }

func ptr(f float64) *float64 { return &f }

// registry holds every known tool.
var registry = []Tool{
	{
		Action:      models.ActionVolumeGet,
		Description: "Returns the current speaker volume (0-100) and mute state.",
	},
	{
		Action:      models.ActionVolumeSet,
		Description: "Sets the absolute speaker volume.",
		Parameters: []Parameter{{
			Name: "level", Type: TypeInteger, Required: true,
			Description: "Volume level in percent, 0-100.",
			Minimum:     ptr(0), Maximum: ptr(100),
		}},
	},
	{
		Action:      models.ActionVolumeAdjust,
		Description: "Makes the speaker louder or quieter relative to the current level. Use about 10 for \"a bit louder\" and -10 for \"a bit quieter\".",
		Parameters: []Parameter{{
			Name: "delta", Type: TypeInteger, Required: true,
			Description: "Change in percent points, negative to decrease.",
			Minimum:     ptr(-100), Maximum: ptr(100),
		}},
	},
	{
		Action:      models.ActionVolumeMute,
		Description: "Mutes or unmutes the speaker.",
		Parameters: []Parameter{{
			Name: "muted", Type: TypeBoolean, Required: true,
			Description: "True to mute, false to unmute.",
		}},
	},
	{
		Action:      models.ActionMediaControl,
		Description: "Controls music playback on the speaker: play/resume, pause, toggle, stop, next or previous track.",
		Parameters: []Parameter{{
			Name: "operation", Type: TypeString, Required: true,
			Description: "Playback operation.",
			Enum:        enumOf(models.MediaOperations),
		}},
	},
	{
		Action:      models.ActionMediaPlayURL,
		Description: "Plays the sound of a YouTube video or another video page, or an audio stream (internet radio, podcast, file), from its URL.",
		Parameters: []Parameter{{
			Name: "url", Type: TypeString, Required: true,
			Description: "http(s) URL: a YouTube link, a video page or a direct stream.",
		}},
	},
	{
		Action: models.ActionMediaPlaySearch,
		Description: "Finds a song, music video, podcast, audiobook or any other video on YouTube and plays its sound on the speaker. " +
			"Use it whenever the user asks to play or put on something by name (\"включи Кино — Группа крови\", \"поставь лоу-фай\", \"включи видео про котов с ютуба\"), " +
			"except music when spotify_play is available: then use it only for YouTube, videos, podcasts and audiobooks, or when the user asks for YouTube. " +
			"Stop, pause and resume it with media_control.",
		Parameters: []Parameter{{
			Name: "query", Type: TypeString, Required: true,
			Description: "Search query in the language of the title, e.g. \"Кино Группа крови\" or \"lofi hip hop radio\". " +
				"Keep the user's words as they are: titles are often slang or made-up words, never correct them into a familiar word " +
				"(\"шахшахаха\" stays \"шахшахаха\", not \"шакшука\"). Include the artist whenever the user named one.",
		}},
	},
	{
		Action:      models.ActionMediaStatus,
		Description: "Returns whether music is playing and the current track title.",
	},
	{
		Action:      models.ActionBluetoothPair,
		Description: "Puts the speaker into Bluetooth pairing mode so a phone or laptop can connect and use it as a Bluetooth speaker.",
		Parameters: []Parameter{{
			Name: "timeout_seconds", Type: TypeInteger,
			Description: "How long to stay discoverable, default 120. May be ignored by the device.",
			Minimum:     ptr(10), Maximum: ptr(600),
		}},
	},
	{
		Action:      models.ActionBluetoothConnect,
		Description: "Connects a previously paired Bluetooth device.",
		Parameters: []Parameter{{
			Name: "device", Type: TypeString, Required: true,
			Description: "Device name or MAC address. Call bluetooth_devices first if unsure.",
		}},
	},
	{
		Action:      models.ActionBluetoothDisconnect,
		Description: "Disconnects a Bluetooth device, or all devices if none is given.",
		Parameters: []Parameter{{
			Name: "device", Type: TypeString,
			Description: "Device name or MAC address.",
		}},
	},
	{
		Action:      models.ActionLightNight,
		Description: "Turns the speaker's LED ring night light on or off (\"включи ночник\"). Warm yellow by default.",
		Parameters: []Parameter{
			{Name: "on", Type: TypeBoolean, Required: true, Description: "True to turn the night light on, false to turn it off."},
			{Name: "color", Type: TypeString, Description: "Colour as RRGGBB hex, e.g. ffb000 for warm yellow, ff0000 for red. Omit for the default."},
			{Name: "brightness", Type: TypeInteger, Description: "Brightness in per cent, 1-100. Omit for the default.", Minimum: ptr(1), Maximum: ptr(100)},
		},
	},
	{
		Action: models.ActionConversationEnd,
		Description: "Ends the conversation and stops listening. Call it when the user says goodbye, " +
			"asks you to stop, be quiet or go away, or is rude and clearly done talking " +
			"(\"пока\", \"всё, хватит\", \"стоп\", \"отстань\", \"замолчи\", \"иди нахуй\"). " +
			"After calling it reply with at most a couple of words, e.g. \"Пока!\", or nothing at all.",
	},
	{
		Action:      models.ActionBluetoothDevices,
		Description: "Lists paired Bluetooth devices and whether they are connected.",
	},
}

func enumOf[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// All returns every known tool.
func All() []Tool { return slices.Clone(registry) }

// For returns the tools whose actions are in capabilities. An empty
// capability list yields every tool.
func For(capabilities []models.Action) []Tool {
	if len(capabilities) == 0 {
		return All()
	}
	var out []Tool
	for _, t := range registry {
		if slices.Contains(capabilities, t.Action) {
			out = append(out, t)
		}
	}
	return out
}

// Lookup finds a tool by its function name.
func Lookup(name string) (Tool, bool) {
	for _, t := range registry {
		if t.Name() == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Schema returns the JSON Schema of the tool's parameters, as used by the
// OpenAI function calling APIs.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.Parameters {
		s := map[string]any{"type": string(p.Type), "description": p.Description}
		if len(p.Enum) > 0 {
			s["enum"] = p.Enum
		}
		if p.Minimum != nil {
			s["minimum"] = *p.Minimum
		}
		if p.Maximum != nil {
			s["maximum"] = *p.Maximum
		}
		props[p.Name] = s
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}
