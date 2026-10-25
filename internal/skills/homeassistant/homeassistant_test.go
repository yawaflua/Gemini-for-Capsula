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

package homeassistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/skills"
)

func fakeHA(t *testing.T, calls *[]string, bodies *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "401", 401)
			return
		}
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/states" {
			w.Write([]byte(`[
				{"entity_id":"light.kitchen","state":"off","attributes":{"friendly_name":"Свет на кухне"}},
				{"entity_id":"light.bedroom","state":"on","attributes":{"friendly_name":"Спальня","brightness":128}},
				{"entity_id":"sensor.bedroom_temp","state":"22.5","attributes":{"friendly_name":"Температура спальня","unit_of_measurement":"°C"}},
				{"entity_id":"lock.front_door","state":"locked","attributes":{"friendly_name":"Входная дверь"}}]`))
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		*bodies = append(*bodies, b)
		w.Write([]byte(`[{"entity_id":"light.kitchen","state":"on","attributes":{}}]`))
	}))
}

func TestFindAndControl(t *testing.T) {
	var calls []string
	var bodies []map[string]any
	srv := fakeHA(t, &calls, &bodies)
	defer srv.Close()
	set := (&Client{URL: srv.URL, Token: "tok"}).Skills()
	ctx := context.Background()
	find, _ := set.Lookup("home_find")
	control, _ := set.Lookup("home_control")

	res, err := find.Run(ctx, skills.Device{}, map[string]any{"query": "кухне"})
	if err != nil {
		t.Fatal(err)
	}
	ents := res["entities"].([]map[string]any)
	if len(ents) != 1 || ents[0]["entity_id"] != "light.kitchen" {
		t.Errorf("find кухне = %v", ents)
	}
	res, _ = find.Run(ctx, skills.Device{}, map[string]any{"query": "спальня"})
	if n := res["total"]; n != 2 {
		t.Errorf("find спальня: %v", res)
	}
	res, _ = find.Run(ctx, skills.Device{}, map[string]any{"query": "дверь"})
	if n := res["total"]; n != 0 {
		t.Errorf("locks must stay hidden: %v", res)
	}

	res, err = control.Run(ctx, skills.Device{}, map[string]any{"entity_id": "light.kitchen", "action": "set_brightness", "value": float64(40)})
	if err != nil || res["state"] != "on" {
		t.Fatalf("control = %v %v", res, err)
	}
	if last := calls[len(calls)-1]; last != "POST /api/services/light/turn_on" || bodies[0]["brightness_pct"] != float64(40) {
		t.Errorf("call %s %v", last, bodies[0])
	}
	if _, err := control.Run(ctx, skills.Device{}, map[string]any{"entity_id": "lock.front_door", "action": "turn_off"}); err == nil {
		t.Error("locks must not be controllable")
	}
	if _, err := control.Run(ctx, skills.Device{}, map[string]any{"entity_id": "light.kitchen", "action": "set_temperature", "value": float64(20)}); err == nil {
		t.Error("temperature on a light must fail")
	}
}
