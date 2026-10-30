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

// Package skills implements functions the model can call that are executed
// on the server rather than on the speaker, e.g. the current time or the
// weather. Subpackages provide the implementations.
package skills

import (
	"context"
	"slices"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Device describes the speaker a request comes from.
type Device struct {
	// ID is the device ID of the speaker.
	ID string
	// Features are the protocol extensions the speaker supports.
	Features []models.Feature
	// Location is a place name, e.g. "Ashdod".
	Location string
	// TimeZone is an IANA zone, e.g. "Asia/Jerusalem".
	TimeZone string
	// Language is a BCP-47 code, e.g. "ru-RU".
	Language string
	// Command runs an action on the speaker and returns its result data. It
	// is nil when there is no speaker to run it on.
	Command func(ctx context.Context, action models.Action, args map[string]any) (map[string]any, error)
}

// Skill is a server-side function.
type Skill interface {
	// Tool declares the function to the model.
	Tool() tools.Tool
	// Run executes a call.
	Run(ctx context.Context, dev Device, args map[string]any) (map[string]any, error)
}

// Optional is a skill that can be switched off at run time, e.g. one
// whose credentials are set in the web panel.
type Optional interface {
	Skill
	// Available reports whether the skill is offered to the model now.
	Available() bool
}

func available(sk Skill) bool {
	o, ok := sk.(Optional)
	return !ok || o.Available()
}

// Set is a collection of skills.
type Set []Skill

// Tools returns the declarations of every available skill.
func (s Set) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(s))
	for _, sk := range s {
		if available(sk) {
			out = append(out, sk.Tool())
		}
	}
	return out
}

// Lookup finds an available skill by function name.
func (s Set) Lookup(name string) (Skill, bool) {
	for _, sk := range s {
		if sk.Tool().Name() == name && available(sk) {
			return sk, true
		}
	}
	return nil, false
}

// String returns a string argument.
func String(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// Int returns a numeric argument or def.
func Int(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

// Lang returns the two-letter language of the device, "en" by default.
func (d Device) Lang() string {
	if len(d.Language) >= 2 {
		return d.Language[:2]
	}
	return "en"
}

// Supports reports whether the speaker supports f.
func (d Device) Supports(f models.Feature) bool { return slices.Contains(d.Features, f) }

// Bool returns a boolean argument.
func Bool(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}
