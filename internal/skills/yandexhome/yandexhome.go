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

// Package yandexhome implements the yandex_home_find, yandex_home_control and
// yandex_home_scene skills: voice control of Yandex Smart Home (УДЯ) through
// the official user API at https://api.iot.yandex.net.
package yandexhome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

const defaultBaseURL = "https://api.iot.yandex.net"

// Client talks to the Yandex Smart Home user API with a user OAuth token.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// Skills returns the three Yandex Smart Home skills.
func (c *Client) Skills() []skills.Skill {
	return []skills.Skill{findSkill{c}, controlSkill{c}, sceneSkill{c}}
}

type room struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type scenario struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"is_active"`
}

type device struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Aliases      []string     `json:"aliases"`
	Type         string       `json:"type"`
	Room         string       `json:"room"`
	Capabilities []capability `json:"capabilities"`
	Properties   []property   `json:"properties"`
}

type capability struct {
	Type       string         `json:"type"`
	State      map[string]any `json:"state"`
	Parameters map[string]any `json:"parameters"`
}

type property struct {
	Type       string         `json:"type"`
	State      map[string]any `json:"state"`
	Parameters map[string]any `json:"parameters"`
}

type userInfo struct {
	Status    string     `json:"status"`
	RequestID string     `json:"request_id"`
	Rooms     []room     `json:"rooms"`
	Devices   []device   `json:"devices"`
	Scenarios []scenario `json:"scenarios"`
}

type actionResponse struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	Devices   []struct {
		ID           string `json:"id"`
		Capabilities []struct {
			Type  string `json:"type"`
			State struct {
				Instance     string `json:"instance"`
				ActionResult struct {
					Status       string `json:"status"`
					ErrorCode    string `json:"error_code"`
					ErrorMessage string `json:"error_message"`
				} `json:"action_result"`
			} `json:"state"`
		} `json:"capabilities"`
	} `json:"devices"`
}

// ---------- find ----------

var actions = []string{
	"turn_on", "turn_off", "toggle", "set_brightness", "set_temperature",
	"set_color", "set_color_temperature", "set_color_scene", "set_mode",
	"set_fan_speed", "set_toggle", "pause", "resume",
}

type findSkill struct{ c *Client }

func (s findSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("yandex_home.find"),
		Description: "Yandex Smart Home (УДЯ): finds devices, rooms and scenarios and returns their ids and current state. " +
			"Call it before yandex_home_control or yandex_home_scene. Use it to answer questions like " +
			"\"какая температура в спальне\", \"включён ли свет на кухне\" or \"какие есть сценарии\". " +
			"Search by a word of the device name or room; an empty query lists everything.",
		Parameters: []tools.Parameter{
			{Name: "query", Type: tools.TypeString, Description: "Words of the device, room or scenario name, e.g. \"кухня\" or \"торшер\"; empty lists everything."},
		},
	}
}

func (s findSkill) Run(ctx context.Context, _ skills.Device, args map[string]any) (map[string]any, error) {
	ui, err := s.c.info(ctx)
	if err != nil {
		return nil, err
	}
	rooms := roomNames(ui)
	q := queryWords(skills.String(args, "query"))
	var devices []map[string]any
	totalDevices := 0
	for i := range ui.Devices {
		d := &ui.Devices[i]
		if deniedType(d.Type) {
			continue
		}
		if !matchesDevice(d, rooms[d.Room], q) {
			continue
		}
		totalDevices++
		if len(devices) < 25 {
			devices = append(devices, summary(d, rooms))
		}
	}
	var scenarios []map[string]any
	for _, sc := range ui.Scenarios {
		if len(q) > 0 && !matchesText(sc.Name, q) {
			continue
		}
		scenarios = append(scenarios, map[string]any{"id": sc.ID, "name": sc.Name, "active": sc.Active})
	}
	var roomsOut []map[string]any
	for _, r := range ui.Rooms {
		roomsOut = append(roomsOut, map[string]any{"id": r.ID, "name": r.Name})
	}
	res := map[string]any{
		"devices":   devices,
		"scenarios": scenarios,
		"rooms":     roomsOut,
		"total":     totalDevices,
	}
	if totalDevices > len(devices) {
		res["note"] = "too many devices; narrow the query"
	}
	if len(devices) == 0 && len(scenarios) == 0 {
		res["note"] = "nothing found; try another word or an empty query"
	}
	return res, nil
}

func roomNames(ui *userInfo) map[string]string {
	out := make(map[string]string, len(ui.Rooms))
	for _, r := range ui.Rooms {
		out[r.ID] = r.Name
	}
	return out
}

func deniedType(t string) bool {
	short := shortType(t)
	for _, denied := range []string{"lock", "openable", "gate", "alarm", "camera", "video_stream", "other"} {
		if short == denied || strings.HasPrefix(short, denied+".") {
			return true
		}
	}
	return false
}

func shortType(t string) string { return strings.TrimPrefix(t, "devices.types.") }

func capKind(t string) string { return strings.TrimPrefix(t, "devices.capabilities.") }

func propKind(t string) string { return strings.TrimPrefix(t, "devices.properties.") }

func instance(m map[string]any) string {
	if m == nil {
		return ""
	}
	s, _ := m["instance"].(string)
	return s
}

func summary(d *device, rooms map[string]string) map[string]any {
	m := map[string]any{"id": d.ID, "name": d.Name, "type": shortType(d.Type)}
	if r := rooms[d.Room]; r != "" {
		m["room"] = r
	}
	if len(d.Aliases) > 0 {
		m["aliases"] = d.Aliases
	}
	var caps []map[string]any
	var controls []string
	for _, c := range d.Capabilities {
		kind := capKind(c.Type)
		item := map[string]any{"type": kind}
		if i := instance(c.State); i != "" {
			item["instance"] = i
		}
		if i := instance(c.Parameters); i != "" {
			item["instance"] = i
		}
		if v, ok := c.State["value"]; ok {
			item["value"] = v
		}
		for _, k := range []string{"min", "max", "precision", "color_model", "color_scenes"} {
			if v, ok := c.Parameters[k]; ok {
				item[k] = v
			}
		}
		if v, ok := c.Parameters["modes"]; ok {
			item["modes"] = v
		}
		switch kind {
		case "on_off":
			controls = appendUnique(controls, "turn_on", "turn_off", "toggle")
			if b, ok := c.State["value"].(bool); ok {
				m["on"] = b
			}
		case "range":
			switch instance(c.State) {
			case "brightness":
				controls = appendUnique(controls, "set_brightness")
			case "temperature":
				controls = appendUnique(controls, "set_temperature")
			}
		case "color_setting":
			controls = appendUnique(controls, "set_color", "set_color_temperature", "set_color_scene")
		case "mode":
			controls = appendUnique(controls, "set_mode")
		case "toggle":
			controls = appendUnique(controls, "set_toggle")
		}
		caps = append(caps, item)
	}
	var props []map[string]any
	for _, p := range d.Properties {
		item := map[string]any{"type": propKind(p.Type)}
		if i := instance(p.State); i != "" {
			item["instance"] = i
		}
		if v, ok := p.State["value"]; ok {
			item["value"] = v
		}
		if v, ok := p.Parameters["unit"]; ok {
			item["unit"] = v
		}
		props = append(props, item)
	}
	if len(caps) > 0 {
		m["capabilities"] = caps
	}
	if len(props) > 0 {
		m["properties"] = props
	}
	if len(controls) > 0 {
		m["controls"] = controls
	}
	return m
}

func appendUnique(list []string, v ...string) []string {
	for _, x := range v {
		if !slicesContains(list, x) {
			list = append(list, x)
		}
	}
	return list
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------- query matching ----------

var stopWords = map[string]bool{
	"в": true, "во": true, "на": true, "с": true, "со": true, "и": true,
	"а": true, "но": true, "или": true, "пожалуйста": true, "мне": true,
	"это": true, "тот": true, "та": true, "то": true, "те": true,
	"включи": true, "выключи": true, "поставь": true, "сделай": true,
	"ярче": true, "темнее": true, "громче": true, "тише": true,
	"какая": true, "какой": true, "какое": true, "какие": true,
	"сколько": true, "есть": true, "ли": true, "там": true, "здесь": true,
}

func queryWords(q string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		w = strings.Trim(w, ".,!?«»\"'()[]")
		if w == "" || stopWords[w] {
			continue
		}
		out = append(out, stem(w))
	}
	return out
}

func stem(w string) string {
	for _, suf := range []string{
		"ями", "ами", "ов", "ев", "ый", "ий", "ая", "ое", "ые", "ой", "ей",
		"ом", "ем", "ах", "ях", "ы", "и", "а", "я", "е", "у", "ю", "ь",
	} {
		if len(w) > len(suf) && len(w)-len(suf) >= 3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

func matchesText(text string, q []string) bool {
	hay := strings.ToLower(text)
	for _, w := range q {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

func matchesDevice(d *device, room string, q []string) bool {
	if len(q) == 0 {
		return true
	}
	var tokens []string
	tokens = append(tokens, d.Name, strings.Join(d.Aliases, " "), shortType(d.Type), room)
	for _, c := range d.Capabilities {
		tokens = append(tokens, capKind(c.Type), instance(c.State), instance(c.Parameters))
	}
	for _, p := range d.Properties {
		tokens = append(tokens, propKind(p.Type), instance(p.State), instance(p.Parameters))
	}
	tokens = append(tokens, synonyms(d)...)
	hay := strings.ToLower(strings.Join(tokens, " "))
	for _, w := range q {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

func synonyms(d *device) []string {
	t := shortType(d.Type)
	var out []string
	switch {
	case strings.HasPrefix(t, "light"):
		out = append(out, "свет", "лампа", "люстра", "торшер", "ночник")
	case strings.HasPrefix(t, "socket"):
		out = append(out, "розетка")
	case strings.Contains(t, "thermostat"), strings.Contains(t, "ac"):
		out = append(out, "кондиционер", "климат", "температура")
	case strings.Contains(t, "vacuum"):
		out = append(out, "пылесос", "уборка")
	case strings.Contains(t, "sensor.climate"):
		out = append(out, "датчик", "климат", "температура", "влажность")
	}
	for _, c := range d.Capabilities {
		switch capKind(c.Type) {
		case "range":
			if instance(c.State) == "temperature" {
				out = append(out, "температура")
			} else {
				out = append(out, "яркость")
			}
		case "color_setting":
			out = append(out, "цвет")
		case "mode":
			out = append(out, "режим")
		}
	}
	for _, p := range d.Properties {
		switch instance(p.State) {
		case "temperature":
			out = append(out, "температура")
		case "humidity":
			out = append(out, "влажность")
		}
	}
	return out
}

// ---------- control ----------

type controlSkill struct{ c *Client }

func (s controlSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("yandex_home.control"),
		Description: "Yandex Smart Home (УДЯ): controls a device found with yandex_home_find. " +
			"Supports lights, sockets, air conditioners, vacuum cleaners and other devices connected to the user's Yandex Smart Home. " +
			"value is brightness % for set_brightness, degrees for set_temperature, kelvin for set_color_temperature; " +
			"color is RRGGBB for set_color; mode is a mode value or colour scene id for set_mode/set_fan_speed/set_color_scene. " +
			"Use instance when a device has several modes (thermostat, fan_speed, cleanup_mode). " +
			"Confirm briefly after the API returns DONE.",
		Parameters: []tools.Parameter{
			{Name: "id", Type: tools.TypeString, Required: true, Description: "Device id from yandex_home_find."},
			{Name: "action", Type: tools.TypeString, Required: true, Enum: actions, Description: "What to do."},
			{Name: "value", Type: tools.TypeInteger, Description: "Number for set_brightness, set_temperature or set_color_temperature."},
			{Name: "color", Type: tools.TypeString, Description: "Colour as RRGGBB hex, e.g. ff8800, for set_color."},
			{Name: "mode", Type: tools.TypeString, Description: "Mode value or colour scene id for set_mode, set_fan_speed or set_color_scene."},
			{Name: "instance", Type: tools.TypeString, Description: "Capability instance when the device has several, e.g. thermostat, fan_speed, cleanup_mode."},
			{Name: "on", Type: tools.TypeBoolean, Description: "Target value for set_toggle; omit to invert."},
		},
	}
}

func (s controlSkill) Run(ctx context.Context, _ skills.Device, args map[string]any) (map[string]any, error) {
	ui, err := s.c.info(ctx)
	if err != nil {
		return nil, err
	}
	d, err := resolveDevice(ui, skills.String(args, "id"))
	if err != nil {
		return nil, err
	}
	if deniedType(d.Type) {
		return nil, fmt.Errorf("%s devices may not be controlled by voice", shortType(d.Type))
	}
	action, err := buildAction(d, args)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"devices": []any{map[string]any{"id": d.ID, "actions": []any{action}}}}
	var resp actionResponse
	if err := s.c.do(ctx, http.MethodPost, "/v1.0/devices/actions", body, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "" && resp.Status != "ok" {
		return nil, fmt.Errorf("yandex iot: %s", resp.Status)
	}
	for _, rd := range resp.Devices {
		for _, c := range rd.Capabilities {
			st := c.State.ActionResult.Status
			if st != "" && st != "DONE" {
				msg := c.State.ActionResult.ErrorMessage
				if msg == "" {
					msg = c.State.ActionResult.ErrorCode
				}
				return nil, fmt.Errorf("yandex iot: %s: %s", st, msg)
			}
		}
	}
	return map[string]any{"device": d.Name, "ok": true, "request_id": resp.RequestID}, nil
}

func resolveDevice(ui *userInfo, id string) (*device, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("id is empty; get it with yandex_home_find")
	}
	for i := range ui.Devices {
		if ui.Devices[i].ID == id {
			return &ui.Devices[i], nil
		}
	}
	var found []*device
	for i := range ui.Devices {
		d := &ui.Devices[i]
		if strings.EqualFold(d.Name, id) {
			found = append(found, d)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("%q matches several devices; pass the exact id from yandex_home_find", id)
	}
	return nil, fmt.Errorf("unknown device %q; call yandex_home_find", id)
}

func buildAction(d *device, args map[string]any) (map[string]any, error) {
	action := skills.String(args, "action")
	value := skills.Int(args, "value", 0)
	mode := skills.String(args, "mode")
	requestedInstance := skills.String(args, "instance")
	color := skills.String(args, "color")
	on := skills.Bool(args, "on")

	mk := func(kind string, state map[string]any) map[string]any {
		return map[string]any{"type": "devices.capabilities." + kind, "state": state}
	}

	switch action {
	case "turn_on", "turn_off":
		if !hasCap(d, "on_off", "") {
			return nil, errors.New("device does not support on/off")
		}
		return mk("on_off", map[string]any{"instance": "on", "value": action == "turn_on"}), nil
	case "toggle":
		if !hasCap(d, "on_off", "") {
			return nil, errors.New("device does not support on/off")
		}
		current, ok := d.onOff()
		if !ok {
			return nil, errors.New("device on/off state is unknown")
		}
		return mk("on_off", map[string]any{"instance": "on", "value": !current}), nil
	case "set_brightness":
		if !hasCap(d, "range", "brightness") {
			return nil, errors.New("device does not support brightness")
		}
		return mk("range", map[string]any{"instance": "brightness", "value": value}), nil
	case "set_temperature":
		if !hasCap(d, "range", "temperature") {
			return nil, errors.New("device does not support temperature")
		}
		return mk("range", map[string]any{"instance": "temperature", "value": value}), nil
	case "set_color":
		if !hasCap(d, "color_setting", "") {
			return nil, errors.New("device does not support colour")
		}
		r, g, b, err := hexRGB(color)
		if err != nil {
			return nil, err
		}
		if colorModel(d) == "hsv" {
			h, s, v := rgbToHSV(r, g, b)
			return mk("color_setting", map[string]any{"instance": "hsv", "value": map[string]any{"h": h, "s": s, "v": v}}), nil
		}
		return mk("color_setting", map[string]any{"instance": "rgb", "value": r<<16 | g<<8 | b}), nil
	case "set_color_temperature":
		if !hasCap(d, "color_setting", "") {
			return nil, errors.New("device does not support colour temperature")
		}
		return mk("color_setting", map[string]any{"instance": "temperature_k", "value": value}), nil
	case "set_color_scene":
		if !hasCap(d, "color_setting", "") {
			return nil, errors.New("device does not support colour scenes")
		}
		return mk("color_setting", map[string]any{"instance": "scene", "value": mode}), nil
	case "set_mode", "set_fan_speed":
		if !hasCap(d, "mode", "") {
			return nil, errors.New("device does not support modes")
		}
		inst := requestedInstance
		if inst == "" {
			if action == "set_fan_speed" {
				inst = "fan_speed"
			} else {
				inst = modeInstance(d)
			}
		}
		return mk("mode", map[string]any{"instance": inst, "value": mode}), nil
	case "set_toggle":
		if !hasCap(d, "toggle", "") {
			return nil, errors.New("device does not support toggles")
		}
		inst := requestedInstance
		if inst == "" {
			inst = toggleInstance(d)
		}
		return mk("toggle", map[string]any{"instance": inst, "value": on}), nil
	case "pause", "resume":
		if !hasCap(d, "on_off", "") {
			return nil, errors.New("device does not support pause/resume")
		}
		return mk("on_off", map[string]any{"instance": "on", "value": action == "resume"}), nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

func hasCap(d *device, kind, inst string) bool {
	for _, c := range d.Capabilities {
		if capKind(c.Type) != kind {
			continue
		}
		if inst == "" || instance(c.State) == inst || instance(c.Parameters) == inst {
			return true
		}
	}
	return false
}

func (d *device) onOff() (bool, bool) {
	for _, c := range d.Capabilities {
		if capKind(c.Type) == "on_off" {
			if b, ok := c.State["value"].(bool); ok {
				return b, true
			}
		}
	}
	return false, false
}

func modeInstance(d *device) string {
	for _, c := range d.Capabilities {
		if capKind(c.Type) == "mode" {
			if i := instance(c.State); i != "" {
				return i
			}
			if i := instance(c.Parameters); i != "" {
				return i
			}
		}
	}
	return "mode"
}

func toggleInstance(d *device) string {
	for _, c := range d.Capabilities {
		if capKind(c.Type) == "toggle" {
			if i := instance(c.State); i != "" {
				return i
			}
			if i := instance(c.Parameters); i != "" {
				return i
			}
		}
	}
	return "toggle"
}

func colorModel(d *device) string {
	for _, c := range d.Capabilities {
		if capKind(c.Type) == "color_setting" {
			if s, ok := c.Parameters["color_model"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func hexRGB(s string) (int, int, int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0, errors.New("color must be RRGGBB")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, errors.New("color must be RRGGBB")
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, nil
}

func rgbToHSV(r, g, b int) (int, int, int) {
	rf, gf, bf := float64(r)/255, float64(g)/255, float64(b)/255
	max := math.Max(rf, math.Max(gf, bf))
	min := math.Min(rf, math.Min(gf, bf))
	delta := max - min
	h := 0.0
	switch {
	case delta == 0:
		h = 0
	case max == rf:
		h = 60 * math.Mod((gf-bf)/delta, 6)
	case max == gf:
		h = 60 * ((bf-rf)/delta + 2)
	default:
		h = 60 * ((rf-gf)/delta + 4)
	}
	if h < 0 {
		h += 360
	}
	s := 0.0
	if max > 0 {
		s = delta / max * 100
	}
	return int(math.Round(h)), int(math.Round(s)), int(math.Round(max * 100))
}

// ---------- scene ----------

type sceneSkill struct{ c *Client }

func (s sceneSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("yandex_home.scene"),
		Description: "Yandex Smart Home (УДЯ): runs a user scenario found with yandex_home_find, e.g. \"Спокойной ночи\". " +
			"Pass id from yandex_home_find, or an exact name.",
		Parameters: []tools.Parameter{
			{Name: "id", Type: tools.TypeString, Description: "Scenario id from yandex_home_find."},
			{Name: "name", Type: tools.TypeString, Description: "Exact scenario name, if the id is unknown."},
		},
	}
}

func (s sceneSkill) Run(ctx context.Context, _ skills.Device, args map[string]any) (map[string]any, error) {
	ui, err := s.c.info(ctx)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(skills.String(args, "id"))
	name := strings.TrimSpace(skills.String(args, "name"))
	var found *scenario
	for i := range ui.Scenarios {
		sc := &ui.Scenarios[i]
		if (id != "" && sc.ID == id) || (id == "" && name != "" && strings.EqualFold(sc.Name, name)) {
			found = sc
			break
		}
	}
	if found == nil {
		return nil, errors.New("unknown scenario; get it with yandex_home_find")
	}
	var resp actionResponse
	if err := s.c.do(ctx, http.MethodPost, "/v1.0/scenarios/"+url.PathEscape(found.ID)+"/actions", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "" && resp.Status != "ok" {
		return nil, fmt.Errorf("yandex iot: %s", resp.Status)
	}
	return map[string]any{"scenario": found.Name, "ok": true, "request_id": resp.RequestID}, nil
}

// ---------- HTTP ----------

var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

func (c *Client) base() string {
	b := strings.TrimRight(c.BaseURL, "/")
	if b == "" {
		b = defaultBaseURL
	}
	return b
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func (c *Client) info(ctx context.Context) (*userInfo, error) {
	var ui userInfo
	if err := c.do(ctx, http.MethodGet, "/v1.0/user/info", nil, &ui); err != nil {
		return nil, err
	}
	return &ui, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.Token == "" {
		return errors.New("yandex iot: token is not configured")
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("yandex iot: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
