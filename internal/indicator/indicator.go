// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package indicator shows the assistant state to the user, e.g. on an LED
// ring. Implementations live in subpackages (see indicator/ringled).
package indicator

// State is what the assistant is doing.
type State int

const (
	// Idle waits for the wake word.
	Idle State = iota
	// Listening records the user's request.
	Listening
	// Thinking waits for the reply.
	Thinking
	// Speaking plays the reply.
	Speaking
	// Error reports a failed request. It is transient: implementations
	// return to Idle on their own after a short while.
	Error
	// Disconnected means the server is unreachable. It persists until the
	// next state change.
	Disconnected
	// Announcing plays a message the server sent on its own, e.g. a
	// question from Claude Code waiting for an answer.
	Announcing
)

func (s State) String() string {
	return [...]string{"idle", "listening", "thinking", "speaking", "error", "disconnected", "announcing"}[s]
}

// Indicator displays states. Show must not block.
type Indicator interface {
	Show(State)
}

// Nop is an indicator that shows nothing.
type Nop struct{}

// Show implements [Indicator].
func (Nop) Show(State) {}

// Multi shows states on several indicators.
type Multi []Indicator

// Show implements [Indicator].
func (m Multi) Show(s State) {
	for _, i := range m {
		i.Show(s)
	}
}
