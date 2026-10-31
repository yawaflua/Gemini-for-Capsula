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

	// The methods below are called on server plugins that declared the
	// matching capability in [InitializeResult].

	// MethodProviderTurn asks a model provider of the plugin to answer:
	// [ProviderTurnParams] → [ProviderTurnResult].
	MethodProviderTurn = "provider.turn"
	// MethodProviderClose says a conversation ended, so the plugin can
	// drop its history: [ProviderCloseParams] (notification).
	MethodProviderClose = "provider.close"
	// MethodSynthesize turns text into speech: [SynthesizeParams] →
	// [SynthesizeResult].
	MethodSynthesize = "speech.synthesize"
	// MethodTranscribe turns speech into text: [TranscribeParams] →
	// [TranscribeResult].
	MethodTranscribe = "speech.transcribe"
	// MethodHTTP passes an HTTP request sent to /plugins/<id>/ on the
	// server: [HTTPRequest] → [HTTPResponse].
	MethodHTTP = "http.request"
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
	// MethodRequest makes a request to the assistant on a speaker as if
	// the user had said it; the answer is spoken there (server only):
	// [RequestParams].
	MethodRequest = "host.request"
	// MethodInstructions replaces what the plugin adds to the system
	// instruction of the model (server only): [InstructionsParams]. New
	// conversations get it at once, a running one after its turn.
	MethodInstructions = "host.instructions"
	// MethodProviderOutput streams part of the reply during
	// [MethodProviderTurn]: [ProviderOutputParams] (notification).
	MethodProviderOutput = "provider.output"
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

	// The fields below are server only.

	// Instructions are added to the system instruction of the model, e.g.
	// a mood or rules for the plugin's functions. See [MethodInstructions].
	Instructions string `json:"instructions,omitempty"`
	// Providers are model providers the plugin implements. They show up as
	// the type "Плагин" in the providers tab of the panel.
	Providers []ProviderSpec `json:"providers,omitempty"`
	// Speech are speech recognition and synthesis engines the plugin
	// implements, for text model providers.
	Speech []SpeechSpec `json:"speech,omitempty"`
	// HTTP, if set, routes requests to /plugins/<id>/ on the server to
	// the plugin, see [MethodHTTP].
	HTTP *HTTPSpec `json:"http,omitempty"`
}

// ProviderSpec declares a model provider: a text model that answers the
// words of the user, calls functions and keeps the history of each
// conversation. The server recognizes and synthesizes speech around it.
// Name must match ^[a-z0-9][a-z0-9_-]{0,31}$.
type ProviderSpec struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// SpeechSpec declares a speech engine. Name is as in [ProviderSpec].
type SpeechSpec struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Synthesize and Transcribe say what the engine can do.
	Synthesize bool `json:"synthesize,omitempty"`
	Transcribe bool `json:"transcribe,omitempty"`
	// SampleRate of synthesized speech: 16-bit little-endian mono PCM;
	// default 24000.
	SampleRate int `json:"sample_rate,omitempty"`
	// Voices are the voices to choose from, if known.
	Voices []string `json:"voices,omitempty"`
}

// DefaultSampleRate is the sample rate of synthesized speech when
// [SpeechSpec.SampleRate] is not set.
const DefaultSampleRate = 24000

// HTTPSpec configures the HTTP routes of a plugin. Without Public a
// request needs the panel login or the server token (Authorization:
// Bearer $MARUSYA_AUTH_TOKEN); public routes are for webhooks and pages
// that check access themselves.
type HTTPSpec struct {
	Public bool `json:"public,omitempty"`
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
	// EventVolume is a change of the speaker's volume by anyone: voice,
	// keys, the panel or a plugin. Data["level"] is 0..100 and
	// Data["muted"] is a bool.
	EventVolume = "volume"
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

// ProviderTurnParams are sent with [MethodProviderTurn]. A turn starts with
// the words of the user in Text. If the plugin asks for function calls,
// the host runs them and calls again with ToolResults and an empty Text,
// until a reply has no calls.
type ProviderTurnParams struct {
	Provider string `json:"provider"`
	// Session identifies the conversation; the plugin keeps its history.
	Session string `json:"session"`
	Text    string `json:"text,omitempty"`
	// ToolResults answer the ToolCalls of the previous reply.
	ToolResults []ProviderToolResult `json:"tool_results,omitempty"`
	// System is the system instruction of the conversation.
	System string `json:"system,omitempty"`
	// Language is a BCP-47 code, e.g. "ru-RU".
	Language string `json:"language,omitempty"`
	// Tools are the functions the model may call.
	Tools []ToolSpec `json:"tools,omitempty"`
}

// ProviderTurnResult is the answer to [MethodProviderTurn]. Text is spoken
// after the text streamed with [MethodProviderOutput].
type ProviderTurnResult struct {
	Text      string             `json:"text,omitempty"`
	ToolCalls []ProviderToolCall `json:"tool_calls,omitempty"`
}

// ProviderToolCall asks the host to run a function. ID is any string
// unique in the turn.
type ProviderToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ProviderToolResult is the outcome of a [ProviderToolCall].
type ProviderToolResult struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// ProviderOutputParams are sent with [MethodProviderOutput].
type ProviderOutputParams struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

// ProviderCloseParams are sent with [MethodProviderClose].
type ProviderCloseParams struct {
	Provider string `json:"provider"`
	Session  string `json:"session"`
}

// SynthesizeParams are sent with [MethodSynthesize].
type SynthesizeParams struct {
	Engine   string `json:"engine"`
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
	Voice    string `json:"voice,omitempty"`
}

// SynthesizeResult is the answer to [MethodSynthesize]: 16-bit
// little-endian mono PCM at the engine's sample rate (base64 in JSON).
type SynthesizeResult struct {
	Audio []byte `json:"audio"`
}

// TranscribeParams are sent with [MethodTranscribe]: 16-bit little-endian
// mono PCM (base64 in JSON) at SampleRate.
type TranscribeParams struct {
	Engine     string `json:"engine"`
	Audio      []byte `json:"audio"`
	SampleRate int    `json:"sample_rate"`
	Language   string `json:"language,omitempty"`
}

// TranscribeResult is the answer to [MethodTranscribe]; Text is empty if
// there was no speech.
type TranscribeResult struct {
	Text string `json:"text"`
}

// HTTPRequest is sent with [MethodHTTP]. Path is below /plugins/<id>, e.g.
// "/" or "/webhook"; Body is base64 in JSON, at most 4 MiB.
type HTTPRequest struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      string              `json:"query,omitempty"`
	Header     map[string][]string `json:"header,omitempty"`
	Body       []byte              `json:"body,omitempty"`
	RemoteAddr string              `json:"remote_addr,omitempty"`
}

// HTTPResponse is the answer to [MethodHTTP]. Status 0 is 200.
type HTTPResponse struct {
	Status int                 `json:"status,omitempty"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

// RequestParams are sent with [MethodRequest]. Device may be empty for the
// only connected speaker.
type RequestParams struct {
	Device string `json:"device,omitempty"`
	Text   string `json:"text"`
}

// InstructionsParams are sent with [MethodInstructions]; empty Text
// removes the addition.
type InstructionsParams struct {
	Text string `json:"text"`
}
