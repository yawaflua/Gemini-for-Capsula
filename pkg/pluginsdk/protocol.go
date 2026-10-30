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

package pluginsdk

import "encoding/json"

// ProtocolVersion is the version of the plugin protocol described here.
const ProtocolVersion = 1

// Host names where a plugin process runs.
const (
	HostServer = "server"
	HostAgent  = "agent"
)

// Methods the host calls on a plugin.
const (
	// MethodInitialize is the first request: [InitializeParams] →
	// [InitializeResult].
	MethodInitialize = "initialize"
	// MethodToolCall runs a tool: [ToolCallParams] → any JSON object.
	MethodToolCall = "tool.call"
	// MethodShutdown asks the plugin to exit; it has a few seconds.
	MethodShutdown = "shutdown"
	// MethodEvent notifies of something that happened: [Event].
	MethodEvent = "event"
	// MethodMessage delivers data from the other half of a plugin that runs
	// on both the server and the speaker: [Message].
	MethodMessage = "message"
)

// Methods a plugin calls on the host.
const (
	// MethodLog writes to the host log: [LogParams] (notification).
	MethodLog = "log"
	// MethodCommand runs a speaker action, e.g. "volume.set":
	// [CommandParams] → result object.
	MethodCommand = "host.command"
	// MethodSay speaks text on a speaker (server only): [SayParams] →
	// [SayResult].
	MethodSay = "host.say"
	// MethodDevices lists the connected speakers (server only) →
	// [DevicesResult].
	MethodDevices = "host.devices"
	// MethodSend sends data to the other half of the plugin: [SendParams].
	MethodSend = "plugin.send"
)

// Param types.
const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
)

// Param is one argument of a tool.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
}

// ToolSpec declares a function the language model may call. Name must match
// ^[a-z][a-z0-9_]{0,47}$ and must not clash with a built-in tool. The
// description tells the model when to call it, in English.
type ToolSpec struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Params      []Param `json:"params,omitempty"`
}

// Device describes a speaker.
type Device struct {
	ID       string `json:"id"`
	Language string `json:"language,omitempty"`
	Location string `json:"location,omitempty"`
	TimeZone string `json:"timezone,omitempty"`
}

// InitializeParams are sent with [MethodInitialize].
type InitializeParams struct {
	Protocol    int    `json:"protocol"`
	Host        string `json:"host"`
	HostVersion string `json:"host_version,omitempty"`
	// PluginDir is the directory of the plugin, also its working directory.
	PluginDir string `json:"plugin_dir"`
	// Config is the "config" object of the manifest, as edited by the user.
	Config json.RawMessage `json:"config,omitempty"`
	// Device is the speaker the agent runs on (agent only).
	Device *Device `json:"device,omitempty"`
}

// InitializeResult is the answer to [MethodInitialize].
type InitializeResult struct {
	Protocol int        `json:"protocol"`
	Tools    []ToolSpec `json:"tools,omitempty"`
	// Events lists the event kinds the plugin wants; "*" is every kind.
	Events []string `json:"events,omitempty"`
}

// ToolCallParams are sent with [MethodToolCall].
type ToolCallParams struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Device Device         `json:"device"`
}

// Event kinds sent by the server.
const (
	EventConnect    = "connect"    // a speaker connected
	EventDisconnect = "disconnect" // a speaker disconnected
	EventTurn       = "turn"       // a conversation turn ended
	EventCommand    = "command"    // a speaker ran an action
	EventSkill      = "skill"      // a server function ran
	EventWake       = "wake"       // the wake word was heard
)

// Event kinds sent by the agent.
const (
	// EventState is a change of the assistant state; Data["state"] is
	// "idle", "listening", "thinking", "speaking", "error",
	// "disconnected" or "announcing".
	EventState = "state"
)

// Event is sent with [MethodEvent].
type Event struct {
	Kind   string         `json:"kind"`
	Device string         `json:"device,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// Message is sent with [MethodMessage]: Data comes from the other half of
// the plugin on Device.
type Message struct {
	Device string          `json:"device,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// LogParams are sent with [MethodLog]. Level is "debug", "info", "warn" or
// "error".
type LogParams struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// CommandParams are sent with [MethodCommand]. Device may be empty on the
// agent and for the only connected speaker.
type CommandParams struct {
	Device string         `json:"device,omitempty"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
}

// SayParams are sent with [MethodSay]. With Listen the speaker waits for a
// spoken answer, returned in [SayResult].
type SayParams struct {
	Device string `json:"device,omitempty"`
	Text   string `json:"text"`
	Listen bool   `json:"listen,omitempty"`
}

// SayResult is the answer to [MethodSay].
type SayResult struct {
	Answer string `json:"answer,omitempty"`
}

// DevicesResult is the answer to [MethodDevices].
type DevicesResult struct {
	Devices []Device `json:"devices"`
}

// SendParams are sent with [MethodSend]. On the server Device chooses the
// speaker (empty: every speaker); on the agent it is ignored.
type SendParams struct {
	Device string          `json:"device,omitempty"`
	Data   json.RawMessage `json:"data"`
}
