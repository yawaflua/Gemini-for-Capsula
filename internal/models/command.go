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
	"math"
)

// Action names an operation the device can perform.
type Action string

const (
	// ActionVolumeGet returns the current [VolumeState].
	ActionVolumeGet Action = "volume.get"
	// ActionVolumeSet sets the absolute volume. Args: "level" (0..100).
	ActionVolumeSet Action = "volume.set"
	// ActionVolumeAdjust changes the volume relatively. Args: "delta" (-100..100).
	ActionVolumeAdjust Action = "volume.adjust"
	// ActionVolumeMute mutes or unmutes the output. Args: "muted" (bool).
	ActionVolumeMute Action = "volume.mute"

	// ActionMediaControl controls the media player. Args: "operation"
	// (one of the [MediaOperation] values).
	ActionMediaControl Action = "media.control"
	// ActionMediaPlayURL starts playing a stream or the sound of a video
	// page (e.g. YouTube). Args: "url".
	ActionMediaPlayURL Action = "media.play_url"
	// ActionMediaPlaySearch finds a song or video (on YouTube) and plays
	// its sound. Args: "query".
	ActionMediaPlaySearch Action = "media.play_search"
	// ActionMediaStatus returns the current [MediaStatus].
	ActionMediaStatus Action = "media.status"

	// ActionBluetoothPair makes the device discoverable and pairable.
	// Args: "timeout_seconds" (optional).
	ActionBluetoothPair Action = "bluetooth.pair"
	// ActionBluetoothConnect connects a known device. Args: "device"
	// (address or name).
	ActionBluetoothConnect Action = "bluetooth.connect"
	// ActionBluetoothDisconnect disconnects a device. Args: "device"
	// (optional, all devices if empty).
	ActionBluetoothDisconnect Action = "bluetooth.disconnect"
	// ActionBluetoothDevices lists known devices.
	ActionBluetoothDevices Action = "bluetooth.devices"

	// ActionLightNight turns the night light on or off. Args: "on" (bool),
	// "color" (RRGGBB, optional), "brightness" (1..100, optional).
	ActionLightNight Action = "light.night"

	// ActionConversationEnd ends the dialogue: after the current reply the
	// device stops listening (no follow-up) and the server forgets the
	// conversation context. Executed by the agent itself, not a controller.
	ActionConversationEnd Action = "conversation.end"
)

// MediaOperation is an argument of [ActionMediaControl].
type MediaOperation string

const (
	MediaPlay     MediaOperation = "play"
	MediaPause    MediaOperation = "pause"
	MediaToggle   MediaOperation = "toggle"
	MediaStop     MediaOperation = "stop"
	MediaNext     MediaOperation = "next"
	MediaPrevious MediaOperation = "previous"
)

// MediaOperations lists every supported [MediaOperation].
var MediaOperations = []MediaOperation{MediaPlay, MediaPause, MediaToggle, MediaStop, MediaNext, MediaPrevious}

// Command asks the device to execute an [Action].
type Command struct {
	ID     string         `json:"id"`
	Action Action         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
}

// String returns the named string argument.
func (c *Command) String(key string) (string, bool) {
	v, ok := c.Args[key].(string)
	return v, ok
}

// Int returns the named numeric argument rounded to an int.
//
// JSON numbers decode as float64, so both float64 and int are accepted.
func (c *Command) Int(key string) (int, bool) {
	switch v := c.Args[key].(type) {
	case float64:
		return int(math.Round(v)), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// Bool returns the named boolean argument.
func (c *Command) Bool(key string) (bool, bool) {
	v, ok := c.Args[key].(bool)
	return v, ok
}

// CommandResult is the outcome of a [Command].
type CommandResult struct {
	ID    string         `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// Success builds a successful result for c.
func (c *Command) Success(data map[string]any) *CommandResult {
	return &CommandResult{ID: c.ID, OK: true, Data: data}
}

// Failure builds a failed result for c.
func (c *Command) Failure(err error) *CommandResult {
	return &CommandResult{ID: c.ID, OK: false, Error: err.Error()}
}

// VolumeState is the output volume of the device.
type VolumeState struct {
	Level int  `json:"level"`
	Muted bool `json:"muted"`
}

// Map converts the state into a generic result payload.
func (v VolumeState) Map() map[string]any {
	return map[string]any{"level": v.Level, "muted": v.Muted}
}

// MediaStatus is the state of the media player.
type MediaStatus struct {
	Playing bool   `json:"playing"`
	Title   string `json:"title,omitempty"`
}

// Map converts the status into a generic result payload.
func (s MediaStatus) Map() map[string]any {
	return map[string]any{"playing": s.Playing, "title": s.Title}
}

// BluetoothDevice is a remote Bluetooth device known to the speaker.
type BluetoothDevice struct {
	Address   string `json:"address"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

// String implements [fmt.Stringer].
func (d BluetoothDevice) String() string {
	return fmt.Sprintf("%s (%s)", d.Name, d.Address)
}
