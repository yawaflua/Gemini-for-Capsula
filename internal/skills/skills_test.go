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

package skills_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/openmeteo"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/skills/clock"
	"github.com/yawaflua/marusya-gemini/internal/skills/weather"
)

func fakeMeteo(t *testing.T) *openmeteo.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/geo", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("name") {
		case "Ашдод":
			w.Write([]byte(`{"results":[{"name":"Ашдод","country":"Израиль","latitude":31.79,"longitude":34.65,"timezone":"Asia/Jerusalem"}]}`))
		case "Токио":
			w.Write([]byte(`{"results":[{"name":"Токио","country":"Япония","latitude":35.68,"longitude":139.69,"timezone":"Asia/Tokyo"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	mux.HandleFunc("/forecast", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latitude") != "31.79" {
			t.Errorf("forecast for %v", r.URL.Query())
		}
		w.Write([]byte(`{"current":{"temperature_2m":24.34,"apparent_temperature":25,"relative_humidity_2m":60,"wind_speed_10m":3.2,"precipitation":0,"weather_code":1},
			"daily":{"time":["2026-10-24"],"weather_code":[61],"temperature_2m_max":[27.1],"temperature_2m_min":[19.5],"precipitation_probability_max":[40]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &openmeteo.Client{GeocodingURL: srv.URL + "/geo", ForecastURL: srv.URL + "/forecast"}
}

var dev = skills.Device{Location: "Ашдод", TimeZone: "Asia/Jerusalem", Language: "ru-RU"}

func TestClock(t *testing.T) {
	now := time.Date(2026, 10, 24, 18, 5, 0, 0, time.UTC)
	s := &clock.Skill{Geo: fakeMeteo(t), Now: func() time.Time { return now }}
	ctx := context.Background()

	out, err := s.Run(ctx, dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["time"] != "21:05" || out["place"] != "Ашдод" {
		t.Errorf("local time = %v", out)
	}

	out, err = s.Run(ctx, dev, map[string]any{"place": "Токио"})
	if err != nil {
		t.Fatal(err)
	}
	if out["time"] != "03:05" || out["place"] != "Токио, Япония" || out["date"] != "2026-10-25" {
		t.Errorf("Tokyo time = %v", out)
	}

	if _, err := s.Run(ctx, dev, map[string]any{"place": "Нигде"}); err == nil {
		t.Error("unknown place: want error")
	}
}

func TestWeather(t *testing.T) {
	s := &weather.Skill{Geo: fakeMeteo(t)}
	out, err := s.Run(context.Background(), dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	cur := out["current"].(map[string]any)
	if cur["temperature_c"] != 24.3 || cur["conditions"] != "mainly clear" {
		t.Errorf("current = %v", cur)
	}
	days := out["daily"].([]map[string]any)
	if len(days) != 1 || days[0]["conditions"] != "rain" || days[0]["max_c"] != 27.1 {
		t.Errorf("daily = %v", days)
	}

	if _, err := s.Run(context.Background(), skills.Device{}, nil); err == nil {
		t.Error("no location: want error")
	}
}

func TestSetLookup(t *testing.T) {
	set := skills.Set{&clock.Skill{}, &weather.Skill{}}
	for _, name := range []string{"time_now", "weather_forecast"} {
		if _, ok := set.Lookup(name); !ok {
			t.Errorf("Lookup(%q) failed", name)
		}
	}
	if len(set.Tools()) != 2 {
		t.Errorf("Tools() = %v", set.Tools())
	}
}
