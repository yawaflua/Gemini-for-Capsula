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

// Package weather implements the weather_forecast skill backed by
// Open-Meteo.
package weather

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/openmeteo"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Skill reports the weather.
type Skill struct {
	Geo *openmeteo.Client
}

var _ skills.Skill = (*Skill)(nil)

// Tool implements [skills.Skill].
func (s *Skill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("weather.forecast"),
		Description: "Returns current weather and a daily forecast. Without a place it is the weather where the speaker is. " +
			"Temperatures are in °C, wind in m/s. Summarize briefly for speech.",
		Parameters: []tools.Parameter{
			{Name: "place", Type: tools.TypeString, Description: "City name; omit for the speaker's own location."},
			{Name: "days", Type: tools.TypeInteger, Description: "Number of forecast days including today, 1-7. Default 1; use 2 for tomorrow.",
				Minimum: ptr(1), Maximum: ptr(7)},
		},
	}
}

func ptr(f float64) *float64 { return &f }

func round(v float64) float64 { return math.Round(v*10) / 10 }

// Run implements [skills.Skill].
func (s *Skill) Run(ctx context.Context, dev skills.Device, args map[string]any) (map[string]any, error) {
	q := strings.TrimSpace(skills.String(args, "place"))
	if q == "" {
		q = dev.Location
	}
	if q == "" {
		return nil, errors.New("the speaker location is not configured; ask the user for a city")
	}
	p, err := s.Geo.Geocode(ctx, q, dev.Lang())
	if err != nil {
		return nil, err
	}
	f, err := s.Geo.Forecast(ctx, p, skills.Int(args, "days", 1))
	if err != nil {
		return nil, err
	}
	c := f.Current
	out := map[string]any{
		"place": p.String(),
		"current": map[string]any{
			"temperature_c":    round(c.Temperature),
			"feels_like_c":     round(c.ApparentTemperature),
			"humidity_pct":     c.Humidity,
			"wind_ms":          round(c.WindSpeed),
			"precipitation_mm": c.Precipitation,
			"conditions":       openmeteo.Describe(c.WeatherCode),
		},
	}
	var days []map[string]any
	d := f.Daily
	for i := range d.Time {
		day := map[string]any{"date": d.Time[i]}
		if i < len(d.WeatherCode) {
			day["conditions"] = openmeteo.Describe(d.WeatherCode[i])
		}
		if i < len(d.TemperatureMax) {
			day["max_c"] = round(d.TemperatureMax[i])
		}
		if i < len(d.TemperatureMin) {
			day["min_c"] = round(d.TemperatureMin[i])
		}
		if i < len(d.PrecipitationProbability) {
			day["precipitation_probability_pct"] = d.PrecipitationProbability[i]
		}
		days = append(days, day)
	}
	out["daily"] = days
	return out, nil
}
