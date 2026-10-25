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

// Package openmeteo is a minimal client for the free Open-Meteo geocoding
// and forecast APIs (https://open-meteo.com). No API key is required.
package openmeteo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client calls Open-Meteo.
type Client struct {
	// GeocodingURL and ForecastURL override the endpoints (for tests).
	GeocodingURL string
	ForecastURL  string
	HTTP         *http.Client

	mu    sync.Mutex
	cache map[string]Place
}

// Place is a geocoded location.
type Place struct {
	Name      string  `json:"name"`
	Country   string  `json:"country"`
	Admin1    string  `json:"admin1"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	TimeZone  string  `json:"timezone"`
}

// String returns "Name, Country".
func (p Place) String() string {
	if p.Country == "" {
		return p.Name
	}
	return p.Name + ", " + p.Country
}

// ErrNotFound is returned when a place cannot be geocoded.
var ErrNotFound = errors.New("place not found")

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *Client) get(ctx context.Context, base string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("open-meteo: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Geocode finds a place by name. language is a two-letter code used to
// match and localize names. Results are cached.
func (c *Client) Geocode(ctx context.Context, name, language string) (Place, error) {
	name = strings.TrimSpace(name)
	key := strings.ToLower(name) + "|" + language
	c.mu.Lock()
	if p, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()

	base := c.GeocodingURL
	if base == "" {
		base = "https://geocoding-api.open-meteo.com/v1/search"
	}
	var res struct {
		Results []Place `json:"results"`
	}
	q := url.Values{"name": {name}, "count": {"1"}, "format": {"json"}}
	if language != "" {
		q.Set("language", language)
	}
	if err := c.get(ctx, base, q, &res); err != nil {
		return Place{}, err
	}
	if len(res.Results) == 0 {
		return Place{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	p := res.Results[0]
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]Place{}
	}
	c.cache[key] = p
	c.mu.Unlock()
	return p, nil
}

// Forecast is current conditions and a daily outlook.
type Forecast struct {
	Current struct {
		Time                string  `json:"time"`
		Temperature         float64 `json:"temperature_2m"`
		ApparentTemperature float64 `json:"apparent_temperature"`
		Humidity            float64 `json:"relative_humidity_2m"`
		WindSpeed           float64 `json:"wind_speed_10m"`
		Precipitation       float64 `json:"precipitation"`
		WeatherCode         int     `json:"weather_code"`
	} `json:"current"`
	Daily struct {
		Time                     []string  `json:"time"`
		WeatherCode              []int     `json:"weather_code"`
		TemperatureMax           []float64 `json:"temperature_2m_max"`
		TemperatureMin           []float64 `json:"temperature_2m_min"`
		PrecipitationProbability []float64 `json:"precipitation_probability_max"`
	} `json:"daily"`
}

// Forecast fetches the weather at p for the given number of days (1..16).
func (c *Client) Forecast(ctx context.Context, p Place, days int) (*Forecast, error) {
	base := c.ForecastURL
	if base == "" {
		base = "https://api.open-meteo.com/v1/forecast"
	}
	q := url.Values{
		"latitude":        {fmt.Sprint(p.Latitude)},
		"longitude":       {fmt.Sprint(p.Longitude)},
		"current":         {"temperature_2m,apparent_temperature,relative_humidity_2m,wind_speed_10m,precipitation,weather_code"},
		"daily":           {"weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max"},
		"timezone":        {"auto"},
		"forecast_days":   {fmt.Sprint(max(1, min(16, days)))},
		"wind_speed_unit": {"ms"},
	}
	f := new(Forecast)
	if err := c.get(ctx, base, q, f); err != nil {
		return nil, err
	}
	return f, nil
}

// Describe returns an English description of a WMO weather code.
func Describe(code int) string {
	switch {
	case code == 0:
		return "clear sky"
	case code == 1:
		return "mainly clear"
	case code == 2:
		return "partly cloudy"
	case code == 3:
		return "overcast"
	case code == 45 || code == 48:
		return "fog"
	case code >= 51 && code <= 57:
		return "drizzle"
	case code >= 61 && code <= 67:
		return "rain"
	case code >= 71 && code <= 77:
		return "snow"
	case code >= 80 && code <= 82:
		return "rain showers"
	case code == 85 || code == 86:
		return "snow showers"
	case code >= 95:
		return "thunderstorm"
	}
	return "unknown"
}
