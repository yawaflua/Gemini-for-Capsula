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

package models

import (
	"fmt"
	"time"
)

// ProtocolVersion is bumped on every incompatible wire protocol change.
const ProtocolVersion = 1

// MessageType discriminates the payload of a [Message].
type MessageType string

const (
	// TypeHello is the first message sent by the device.
	TypeHello MessageType = "hello"
	// TypeReady acknowledges [TypeHello].
	TypeReady MessageType = "ready"
	// TypeTurnStart tells the server that the user started speaking.
	TypeTurnStart MessageType = "turn_start"
	// TypeTurnEnd tells the server that the user finished speaking.
	TypeTurnEnd MessageType = "turn_end"
	// TypeTurnCancel aborts the current turn (e.g. no speech was detected).
	TypeTurnCancel MessageType = "turn_cancel"
	// TypeListenEnd tells the device that the server detected the end of
	// speech and the microphone stream may stop (server-side VAD).
	TypeListenEnd MessageType = "listen_end"
	// TypeTranscript carries recognized user speech or the model's reply text.
	TypeTranscript MessageType = "transcript"
	// TypeCommand asks the device to perform an action.
	TypeCommand MessageType = "command"
	// TypeCommandResult answers a [TypeCommand].
	TypeCommandResult MessageType = "command_result"
	// TypeInterrupted tells the device to stop playing the current response.
	TypeInterrupted MessageType = "interrupted"
	// TypeTurnComplete marks the end of the server's response.
	TypeTurnComplete MessageType = "turn_complete"
	// TypeError reports a non-fatal error.
	TypeError MessageType = "error"
	// TypeLog forwards a device log record to the server.
	TypeLog MessageType = "log"
	// TypeRemoteTurn tells the device that the server started a turn on its
	// own (a question typed in the web panel): a reply for TurnID follows
	// and must be played although the device did not record anything. Only
	// sent to devices announcing [FeatureRemoteTurn].
	TypeRemoteTurn MessageType = "remote_turn"
	// TypeWakeStream turns streaming of idle microphone audio on or off: the
	// server listens for the wake word itself (see [FeatureServerWake]).
	TypeWakeStream MessageType = "wake_stream"
	// TypeWake tells the device that the server heard the wake word.
	TypeWake MessageType = "wake"
	// TypeAnnounce tells the device that the server speaks on its own (a
	// question from Claude Code, a Home Assistant notification): the audio
	// for TurnID follows, then [TypeTurnComplete]. With Announce.Listen the
	// device then listens for an answer and starts that turn with ReplyTo
	// set to TurnID; it sends [TypeTurnCancel] with ReplyTo if nobody
	// answers. A busy device declines with [TypeTurnCancel] for TurnID.
	// Only sent to devices announcing [FeatureAnnounce].
	TypeAnnounce MessageType = "announce"
	// TypeAlarms gives the device the complete list of upcoming alarm rings,
	// replacing the previous one. The device rings them on its own, also
	// while it is disconnected. Sent on connect and on every change, only to
	// devices announcing [FeatureAlarms].
	TypeAlarms MessageType = "alarms"
)

// Feature names an optional protocol extension supported by the device.
type Feature string

// FeatureRemoteTurn means the device understands [TypeRemoteTurn].
const FeatureRemoteTurn Feature = "remote_turn"

// FeatureServerWake means the device understands [TypeWakeStream] and
// [TypeWake]: while idle and asked to, it streams its microphone and starts
// listening when the server reports the wake word.
const FeatureServerWake Feature = "server_wake"

// FeatureAnnounce means the device understands [TypeAnnounce].
const FeatureAnnounce Feature = "announce"

// FeatureAlarms means the device understands [TypeAlarms].
const FeatureAlarms Feature = "alarms"

// Message is the envelope of every JSON text frame.
//
// Exactly one payload field matching Type is expected to be set.
type Message struct {
	Type   MessageType `json:"type"`
	TurnID string      `json:"turn_id,omitempty"`
	// ReplyTo marks a turn answering an announcement (see [TypeAnnounce]).
	ReplyTo string `json:"reply_to,omitempty"`

	Hello         *Hello         `json:"hello,omitempty"`
	Ready         *Ready         `json:"ready,omitempty"`
	Transcript    *Transcript    `json:"transcript,omitempty"`
	Command       *Command       `json:"command,omitempty"`
	CommandResult *CommandResult `json:"command_result,omitempty"`
	Error         *Error         `json:"error,omitempty"`
	Log           *Log           `json:"log,omitempty"`
	WakeStream    *WakeStream    `json:"wake_stream,omitempty"`
	Wake          *Wake          `json:"wake,omitempty"`
	Announce      *Announce      `json:"announce,omitempty"`
	Alarms        *Alarms        `json:"alarms,omitempty"`
}

// Alarms is the payload of [TypeAlarms].
type Alarms struct {
	Rings []AlarmRing `json:"rings"`
}

// AlarmRing is one upcoming ring of an alarm or a timer.
type AlarmRing struct {
	// AlarmID identifies the alarm; a repeating alarm has several rings.
	AlarmID string `json:"alarm_id"`
	// InMS is the time until the ring in milliseconds, counted from when the
	// message was sent. Relative times make the device's clock irrelevant.
	InMS int64 `json:"in_ms"`
	// Label is what the alarm is for, e.g. "работа" or "таймер".
	Label string `json:"label,omitempty"`
	// Music is a search query to play instead of the beeps, e.g.
	// "Кино Группа крови" (see [ActionMediaPlaySearch]).
	Music string `json:"music,omitempty"`
	// Volume is the speaker volume while ringing, 1..100; 0 keeps the
	// current one.
	Volume int `json:"volume,omitempty"`
}

// Announce is the payload of [TypeAnnounce].
type Announce struct {
	// Text is what is being said, for logs.
	Text string `json:"text,omitempty"`
	// Listen asks for a spoken answer after the announcement.
	Listen bool `json:"listen,omitempty"`
}

// WakeStream is the payload of [TypeWakeStream].
type WakeStream struct {
	On bool `json:"on"`
}

// Wake is the payload of [TypeWake].
type Wake struct {
	Keyword string `json:"keyword,omitempty"`
}

// Hello identifies the device and its capabilities.
type Hello struct {
	ProtocolVersion int         `json:"protocol_version"`
	DeviceID        string      `json:"device_id"`
	InputFormat     AudioFormat `json:"input_format"`
	// Capabilities lists the [Action] values the device can execute. The
	// server only exposes matching tools to the model.
	Capabilities []Action `json:"capabilities,omitempty"`
	// Language is a BCP-47 language code, e.g. "ru-RU".
	Language string `json:"language,omitempty"`
	// Location is where the speaker is, e.g. "Ashdod". Used for the time
	// and the weather.
	Location string `json:"location,omitempty"`
	// TimeZone is the IANA time zone of the speaker, e.g. "Asia/Jerusalem".
	TimeZone string `json:"timezone,omitempty"`
	// Features lists the optional protocol extensions the device supports.
	Features []Feature `json:"features,omitempty"`
}

// Ready is sent by the server once the device is accepted.
type Ready struct {
	SessionID    string      `json:"session_id"`
	OutputFormat AudioFormat `json:"output_format"`
	// ServerVAD reports that the server detects the end of speech. The device
	// then streams from the wake word until it receives [TypeListenEnd] and
	// uses its own endpointer only as a fallback.
	ServerVAD bool `json:"server_vad,omitempty"`
}

// Role identifies the author of a transcript.
type Role string

const (
	RoleUser  Role = "user"
	RoleModel Role = "model"
)

// Transcript is a fragment of recognized or generated text.
type Transcript struct {
	Role  Role   `json:"role"`
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

// ErrorCode classifies an [Error].
type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	ErrorCodeUnavailable     ErrorCode = "UNAVAILABLE"
	ErrorCodeInternal        ErrorCode = "INTERNAL"
)

// Error is a non-fatal error reported to the peer.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Log is a device log record.
type Log struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Validate checks that the payload matches the message type.
func (m *Message) Validate() error {
	var ok bool
	switch m.Type {
	case TypeHello:
		ok = m.Hello != nil
	case TypeReady:
		ok = m.Ready != nil
	case TypeTranscript:
		ok = m.Transcript != nil
	case TypeCommand:
		ok = m.Command != nil
	case TypeCommandResult:
		ok = m.CommandResult != nil
	case TypeError:
		ok = m.Error != nil
	case TypeLog:
		ok = m.Log != nil
	case TypeWakeStream:
		ok = m.WakeStream != nil
	case TypeWake:
		ok = m.Wake != nil
	case TypeAnnounce:
		ok = m.Announce != nil
	case TypeAlarms:
		ok = m.Alarms != nil
	case TypeTurnStart, TypeTurnEnd, TypeTurnCancel, TypeInterrupted, TypeTurnComplete, TypeListenEnd, TypeRemoteTurn:
		ok = true
	default:
		return fmt.Errorf("unknown message type %q", m.Type)
	}
	if !ok {
		return fmt.Errorf("message of type %q has no payload", m.Type)
	}
	return nil
}
