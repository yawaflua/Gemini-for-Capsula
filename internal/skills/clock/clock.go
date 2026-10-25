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

// Package clock implements the time_now skill: the current local time at
// the speaker or at any place in the world.
package clock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/openmeteo"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Skill tells the time.
type Skill struct {
	Geo *openmeteo.Client
	// Now overrides the clock (for tests).
	Now func() time.Time
}

var _ skills.Skill = (*Skill)(nil)

// Tool implements [skills.Skill].
func (s *Skill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("time.now"),
		Description: "Returns the current local time and date. Without a place it is the time where the speaker is. " +
			"Answer as \"HH:MM в <place>\", e.g. \"21:05 в Ашдоде\".",
		Parameters: []tools.Parameter{{
			Name: "place", Type: tools.TypeString,
			Description: "City or country to tell the time in; omit for the speaker's own location.",
		}},
	}
}

// Run implements [skills.Skill].
func (s *Skill) Run(ctx context.Context, dev skills.Device, args map[string]any) (map[string]any, error) {
	place, zone := dev.Location, dev.TimeZone
	if q := strings.TrimSpace(skills.String(args, "place")); q != "" {
		p, err := s.Geo.Geocode(ctx, q, dev.Lang())
		if err != nil {
			return nil, err
		}
		place, zone = p.String(), p.TimeZone
	}
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", zone, err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().In(loc)
	return map[string]any{
		"place":      place,
		"time":       t.Format("15:04"),
		"date":       t.Format("2006-01-02"),
		"weekday":    t.Weekday().String(),
		"timezone":   zone,
		"utc_offset": t.Format("-07:00"),
	}, nil
}
