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

// Package models defines the data types shared by the device agent and the
// server: the wire protocol spoken over the WebSocket stream, audio formats
// and device commands.
//
// The protocol is intentionally simple:
//
//   - Text frames carry a JSON-encoded [Message].
//   - Binary frames carry raw LINEAR16 PCM audio. Device-to-server frames use
//     [DeviceInputFormat]; server-to-device frames use the [AudioFormat]
//     announced in [Ready].
//
// A typical exchange looks like:
//
//	device                          server
//	  | -- hello -------------------> |
//	  | <------------------- ready -- |
//	  | -- turn_start --------------> |
//	  | == PCM (mic) ===============> |  ---> provider (Gemini)
//	  | -- turn_end ----------------> |
//	  | <------------- transcript --- |
//	  | <---------------- command --- |  <--- function call
//	  | -- command_result ----------> |  ---> function response
//	  | <=============== PCM (tts) == |
//	  | <---------- turn_complete --- |
//	  | -- log ---------------------> |  device warnings and errors
package models
