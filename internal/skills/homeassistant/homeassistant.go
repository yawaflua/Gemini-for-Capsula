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

// Package homeassistant implements the home_find and home_control skills:
// voice control of a Home Assistant instance over its REST API.
package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// DefaultDomains are the entity domains that may be controlled by voice.
// Locks, alarms and the like are left out on purpose: anyone near the
// speaker could open the door.
var DefaultDomains = []string{
	"light", "switch", "fan", "cover", "climate", "media_player", "scene",
	"script", "input_boolean", "vacuum", "humidifier", "button",
}

// readOnly are domains whose state can be asked about.
var readOnly = []string{"sensor", "binary_sensor", "weather", "person", "sun"}

// Client talks to Home Assistant.
type Client struct {
	// URL is the base URL, e.g. http://homeassistant.local:8123.
	URL string
	// Token is a long-lived access token (profile → Security).
	Token string
	// Domains may be controlled; nil means [DefaultDomains].
	Domains []string
	// HTTP overrides the HTTP client.
	HTTP *http.Client
}

// Skills returns home_find and home_control.
func (c *Client) Skills() skills.Set { return skills.Set{findSkill{c}, controlSkill{c}} }

func (c *Client) domains() []string {
	if c.Domains != nil {
		return c.Domains
	}
	return DefaultDomains
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("home assistant: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("home assistant: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type state struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

func (s state) domain() string { d, _, _ := strings.Cut(s.EntityID, "."); return d }

func (s state) name() string {
	if n, ok := s.Attributes["friendly_name"].(string); ok && n != "" {
		return n
	}
	return s.EntityID
}

// summary is what the model gets about an entity.
func (s state) summary() map[string]any {
	m := map[string]any{"entity_id": s.EntityID, "name": s.name(), "state": s.State}
	for _, k := range []string{"unit_of_measurement", "current_temperature", "temperature", "hvac_action",
		"current_position", "media_title", "media_artist", "volume_level", "device_class"} {
		if v, ok := s.Attributes[k]; ok && v != nil {
			m[k] = v
		}
	}
	if b, ok := s.Attributes["brightness"].(float64); ok {
		m["brightness_pct"] = int(b/255*100 + 0.5)
	}
	return m
}

const maxFound = 25

type findSkill struct{ c *Client }

func (s findSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("home.find"),
		Description: "Smart home (Home Assistant): finds devices and sensors and returns their entity_id and current state. " +
			"Call it before home_control to get the entity_id, and to answer questions like \"какая температура в спальне\" or \"свет на кухне включён?\". " +
			"Names are usually in the user's language; search by a word of the name or room.",
		Parameters: []tools.Parameter{
			{Name: "query", Type: tools.TypeString, Description: "Words of the device or room name, e.g. \"кухня\" or \"торшер\"; empty lists everything."},
			{Name: "domain", Type: tools.TypeString, Description: "Optional entity domain filter, e.g. light, switch, cover, climate, sensor."},
		},
	}
}

func (s findSkill) Run(ctx context.Context, _ skills.Device, args map[string]any) (map[string]any, error) {
	var all []state
	if err := s.c.do(ctx, "GET", "/api/states", nil, &all); err != nil {
		return nil, err
	}
	query := strings.Fields(strings.ToLower(skills.String(args, "query")))
	domain := strings.ToLower(strings.TrimSpace(skills.String(args, "domain")))
	visible := append(slices.Clone(s.c.domains()), readOnly...)
	var found []map[string]any
	total := 0
	for _, st := range all {
		d := st.domain()
		if !slices.Contains(visible, d) || (domain != "" && d != domain) {
			continue
		}
		hay := strings.ToLower(st.name() + " " + st.EntityID)
		if !matches(hay, query) {
			continue
		}
		total++
		if len(found) < maxFound {
			sum := st.summary()
			sum["controllable"] = slices.Contains(s.c.domains(), d)
			found = append(found, sum)
		}
	}
	res := map[string]any{"entities": found, "total": total}
	if total > len(found) {
		res["note"] = "too many matches; narrow the query"
	}
	return res, nil
}

// matches reports whether every word of the query (or its stem: Russian
// names decline, "кухне" should find "Кухня") occurs in hay.
func matches(hay string, query []string) bool {
	for _, w := range query {
		r := []rune(w)
		if len(r) > 4 {
			r = r[:len(r)-2]
		}
		if !strings.Contains(hay, string(r)) {
			return false
		}
	}
	return true
}

type controlSkill struct{ c *Client }

var actions = []string{
	"turn_on", "turn_off", "toggle", "open", "close", "stop", "set_brightness",
	"set_temperature", "set_position", "activate", "play", "pause", "set_volume",
}

func (s controlSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("home.control"),
		Description: "Smart home (Home Assistant): controls a device found with home_find. " +
			"activate runs a scene, script or button. value is brightness % for set_brightness, degrees for set_temperature, " +
			"position % (0 closed, 100 open) for set_position, volume % for set_volume. Confirm briefly after it succeeds.",
		Parameters: []tools.Parameter{
			{Name: "entity_id", Type: tools.TypeString, Required: true, Description: "Entity from home_find, e.g. light.kitchen."},
			{Name: "action", Type: tools.TypeString, Required: true, Enum: actions, Description: "What to do."},
			{Name: "value", Type: tools.TypeInteger, Description: "Number for the set_* actions."},
		},
	}
}

var errNoValue = errors.New("this action needs value")

// service maps an action on a domain to a Home Assistant service call.
func service(domain, action string, value int, hasValue bool) (string, map[string]any, error) {
	data := map[string]any{}
	need := func() error {
		if !hasValue {
			return errNoValue
		}
		return nil
	}
	switch action {
	case "turn_on", "turn_off", "toggle":
		switch domain {
		case "cover":
			return map[string]string{"turn_on": "open_cover", "turn_off": "close_cover", "toggle": "toggle"}[action], data, nil
		case "button":
			return "press", data, nil
		case "scene":
			return "turn_on", data, nil
		case "vacuum":
			return map[string]string{"turn_on": "start", "turn_off": "return_to_base", "toggle": "start"}[action], data, nil
		}
		return action, data, nil
	case "open", "close", "stop":
		if domain != "cover" {
			return "", nil, fmt.Errorf("%s works on covers, not %s", action, domain)
		}
		return action + "_cover", data, nil
	case "set_brightness":
		if domain != "light" {
			return "", nil, fmt.Errorf("brightness works on lights, not %s", domain)
		}
		data["brightness_pct"] = min(max(value, 0), 100)
		return "turn_on", data, need()
	case "set_temperature":
		if domain != "climate" {
			return "", nil, fmt.Errorf("temperature works on climate devices, not %s", domain)
		}
		data["temperature"] = value
		return "set_temperature", data, need()
	case "set_position":
		if domain != "cover" {
			return "", nil, fmt.Errorf("position works on covers, not %s", domain)
		}
		data["position"] = min(max(value, 0), 100)
		return "set_cover_position", data, need()
	case "activate":
		switch domain {
		case "scene", "script":
			return "turn_on", data, nil
		case "button":
			return "press", data, nil
		}
		return "", nil, fmt.Errorf("activate works on scenes, scripts and buttons, not %s", domain)
	case "play", "pause", "set_volume":
		if domain != "media_player" {
			return "", nil, fmt.Errorf("%s works on media players, not %s", action, domain)
		}
		if action == "set_volume" {
			data["volume_level"] = float64(min(max(value, 0), 100)) / 100
			return "volume_set", data, need()
		}
		return "media_" + action, data, nil
	}
	return "", nil, fmt.Errorf("unknown action %q", action)
}

func (s controlSkill) Run(ctx context.Context, _ skills.Device, args map[string]any) (map[string]any, error) {
	id := strings.TrimSpace(skills.String(args, "entity_id"))
	domain, _, ok := strings.Cut(id, ".")
	if !ok || domain == "" {
		return nil, fmt.Errorf("invalid entity_id %q; get it with home_find", id)
	}
	if !slices.Contains(s.c.domains(), domain) {
		return nil, fmt.Errorf("%s devices may not be controlled by voice", domain)
	}
	_, hasValue := args["value"]
	svc, data, err := service(domain, skills.String(args, "action"), skills.Int(args, "value", 0), hasValue)
	if err != nil {
		return nil, err
	}
	data["entity_id"] = id
	var changed []state
	if err := s.c.do(ctx, "POST", "/api/services/"+domain+"/"+svc, data, &changed); err != nil {
		return nil, err
	}
	res := map[string]any{"ok": true, "service": domain + "." + svc}
	for _, st := range changed {
		if st.EntityID == id {
			res["state"] = st.State
		}
	}
	return res, nil
}
