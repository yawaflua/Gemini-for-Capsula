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

package yandexhome

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/skills"
)

const infoJSON = `{
  "status": "ok",
  "rooms": [{"id": "r1", "name": "Зал"}],
  "devices": [
    {
      "id": "d1", "name": "Люстра", "type": "devices.types.light.ceiling", "room": "r1",
      "capabilities": [
        {"type": "devices.capabilities.on_off", "state": {"instance": "on", "value": false}},
        {"type": "devices.capabilities.range", "state": {"instance": "brightness", "value": 80},
         "parameters": {"instance": "brightness", "min": 1, "max": 100, "precision": 1}}
      ]
    },
    {
      "id": "d2", "name": "Датчик климата", "type": "devices.types.sensor.climate", "room": "r1",
      "properties": [
        {"type": "devices.properties.float", "state": {"instance": "temperature", "value": 22.9},
         "parameters": {"instance": "temperature", "unit": "°C"}}
      ]
    },
    {
      "id": "d3", "name": "Замок", "type": "devices.types.lock", "room": "r1",
      "capabilities": [{"type": "devices.capabilities.on_off", "state": {"instance": "on", "value": true}}]
    },
    {
      "id": "d4", "name": "Розетка", "type": "devices.types.socket", "room": "r1",
      "capabilities": [{"type": "devices.capabilities.on_off", "state": {"instance": "on", "value": true}}]
    }
  ],
  "scenarios": [{"id": "s1", "name": "Спокойной ночи", "is_active": true}]
}`

const actionJSON = `{"status":"ok","request_id":"req1","devices":[{"id":"d1","capabilities":[{"type":"devices.capabilities.on_off","state":{"instance":"on","action_result":{"status":"DONE"}}}]}]}`

func fakeServer(t *testing.T) (*httptest.Server, *map[string]any, *string) {
	t.Helper()
	var action map[string]any
	var scenePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.0/user/info":
			io.WriteString(w, infoJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.0/devices/actions":
			if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
				t.Errorf("decode action: %v", err)
			}
			io.WriteString(w, actionJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.0/scenarios/s1/actions":
			scenePath = r.URL.Path
			io.WriteString(w, `{"status":"ok","request_id":"req2"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &action, &scenePath
}

func devices(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	raw, ok := res["devices"].([]map[string]any)
	if !ok {
		// The map is created in-process, but be tolerant if it was JSON-roundtripped.
		list, _ := res["devices"].([]any)
		for _, v := range list {
			if m, ok := v.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	}
	return raw
}

func TestFindMatchesRoomSynonymsAndDeniesLocks(t *testing.T) {
	srv, _, _ := fakeServer(t)
	c := &Client{Token: "t", BaseURL: srv.URL}
	ctx := context.Background()

	res, err := findSkill{c}.Run(ctx, skills.Device{}, map[string]any{"query": "свет в зале"})
	if err != nil {
		t.Fatal(err)
	}
	ds := devices(t, res)
	if len(ds) != 1 || ds[0]["name"] != "Люстра" {
		t.Fatalf("query свет в зале -> %+v", ds)
	}
	if ds[0]["on"] != false {
		t.Fatalf("light on = %v, want false", ds[0]["on"])
	}

	res, err = findSkill{c}.Run(ctx, skills.Device{}, map[string]any{"query": "зал"})
	if err != nil {
		t.Fatal(err)
	}
	ds = devices(t, res)
	if len(ds) != 3 {
		t.Fatalf("query зал -> %d devices, want 3 (lock denied)", len(ds))
	}

	res, err = findSkill{c}.Run(ctx, skills.Device{}, map[string]any{"query": "температура"})
	if err != nil {
		t.Fatal(err)
	}
	ds = devices(t, res)
	if len(ds) != 1 || ds[0]["name"] != "Датчик климата" {
		t.Fatalf("query температура -> %+v", ds)
	}
	props, _ := ds[0]["properties"].([]map[string]any)
	if len(props) != 1 || props[0]["instance"] != "temperature" {
		t.Fatalf("sensor properties = %+v", props)
	}

	res, err = findSkill{c}.Run(ctx, skills.Device{}, map[string]any{"query": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(devices(t, res)); got != 3 {
		t.Fatalf("empty query -> %d devices, want 3", got)
	}
	scenarios, _ := res["scenarios"].([]map[string]any)
	if len(scenarios) != 1 || scenarios[0]["name"] != "Спокойной ночи" {
		t.Fatalf("scenarios = %+v", scenarios)
	}
}

func TestControlBuildsActions(t *testing.T) {
	srv, action, _ := fakeServer(t)
	c := &Client{Token: "t", BaseURL: srv.URL}
	ctx := context.Background()

	out, err := controlSkill{c}.Run(ctx, skills.Device{}, map[string]any{"id": "d1", "action": "turn_on"})
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["device"] != "Люстра" {
		t.Fatalf("turn_on -> %+v", out)
	}
	if got := actionType(t, *action); got != "devices.capabilities.on_off" {
		t.Fatalf("turn_on type = %q", got)
	}
	if got := actionState(t, *action)["value"]; got != true {
		t.Fatalf("turn_on value = %v", got)
	}

	if _, err := (controlSkill{c}).Run(ctx, skills.Device{}, map[string]any{"id": "d1", "action": "set_brightness", "value": 50}); err != nil {
		t.Fatal(err)
	}
	if got := actionType(t, *action); got != "devices.capabilities.range" {
		t.Fatalf("brightness type = %q", got)
	}
	state := actionState(t, *action)
	if state["instance"] != "brightness" || state["value"] != float64(50) {
		t.Fatalf("brightness state = %+v", state)
	}

	if _, err := (controlSkill{c}).Run(ctx, skills.Device{}, map[string]any{"id": "d3", "action": "turn_on"}); err == nil {
		t.Fatal("lock control must fail")
	}
}

func TestSceneByName(t *testing.T) {
	srv, _, scenePath := fakeServer(t)
	c := &Client{Token: "t", BaseURL: srv.URL}
	out, err := sceneSkill{c}.Run(context.Background(), skills.Device{}, map[string]any{"name": "Спокойной ночи"})
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["scenario"] != "Спокойной ночи" {
		t.Fatalf("scene -> %+v", out)
	}
	if *scenePath != "/v1.0/scenarios/s1/actions" {
		t.Fatalf("scene path = %q", *scenePath)
	}
}

func actionType(t *testing.T, body map[string]any) string {
	t.Helper()
	state := actionState(t, body)
	_ = state
	action := firstAction(t, body)
	s, _ := action["type"].(string)
	return s
}

func actionState(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	action := firstAction(t, body)
	state, _ := action["state"].(map[string]any)
	return state
}

func firstAction(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	devices, _ := body["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices = %+v", body["devices"])
	}
	dev, _ := devices[0].(map[string]any)
	actions, _ := dev["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %+v", dev["actions"])
	}
	action, _ := actions[0].(map[string]any)
	return action
}
