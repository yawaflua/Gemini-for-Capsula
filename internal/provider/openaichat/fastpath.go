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

package openaichat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/provider"
	"github.com/yawaflua/marusya-gemini/internal/provider/pipeline"
)

// The fast path handles short, unambiguous speaker commands and a few
// lookups without asking the text model. It keeps volume, AirPlay, weather
// and time responsive on slow or offline backends (a local LLM on an old CPU
// takes tens of seconds per turn).
var (
	// Go's \b is ASCII-only, so Cyrillic commands need an explicit
	// separator class instead of word boundaries.
	commandRE = regexp.MustCompile(`(?i)^(сделай|поставь|установи|включи|выключи|отключи|убери|верни|убавь|прибавь|увеличь|уменьши|громче|погромче|тише|потише|громкость|яркость|следующий|следующую|предыдущий|предыдущую|пауза|паузу|продолжи|продолжай|играй|дальше|назад|стоп|запусти|активируй|выполни|без|next|prev|play)(?:[\s,.:;!?-]|$)`)

	volumeSetRE  = regexp.MustCompile(`(?i)громкость\s+(?:на\s+)?(\d{1,3})`)
	volumeUpRE   = regexp.MustCompile(`(?i)(громче|погромче|прибавь|увеличь)`)
	volumeDownRE = regexp.MustCompile(`(?i)(тише|потише|убавь|уменьши)`)
	muteOnRE     = regexp.MustCompile(`(?i)(выключи|отключи|убери)\s+(звук|громкость)|без\s+звука`)
	muteOffRE    = regexp.MustCompile(`(?i)(включи|верни)\s+(звук|громкость)`)

	mediaNextRE  = regexp.MustCompile(`(?i)((следующий|следующую|следующая)\s+(трек|песн)\w*|дальше|next)`)
	mediaPrevRE  = regexp.MustCompile(`(?i)((предыдущий|предыдущую|предыдущая)\s+(трек|песн)\w*|назад|prev)`)
	mediaPauseRE = regexp.MustCompile(`(?i)(пауза|паузу|останови|стоп|поставь\s+на\s+паузу|выключи\s+музыку)`)
	mediaPlayRE  = regexp.MustCompile(`(?i)(продолжи|продолжай|играй|включи\s+музыку|play)`)

	weatherRE = regexp.MustCompile(`(?i)^(какая|что|скажи|узнай|погода|погоду|погоде)(?:\s|$)`)
	timeRE    = regexp.MustCompile(`(?i)(который\s+час|сколько\s+времени|какое\s+(сейчас\s+)?время)`)

	homeOnRE  = regexp.MustCompile(`(?i)^(включи|вруби|зажги)\s+(.+)`)
	homeOffRE = regexp.MustCompile(`(?i)^(выключи|отключи|погаси)\s+(.+)`)

	// Relative volume: "сделай громче на 20", "тише на 10".
	volumeDeltaRE = regexp.MustCompile(`(?i)(?:на|плюс|\+)\s*(\d{1,3})`)

	// Date and weekday: the time skill already returns both.
	dateRE = regexp.MustCompile(`(?i)(какое\s+(сегодня\s+)?число|какая\s+(сегодня\s+)?дата|какой\s+(сегодня\s+)?день(?:\s+недели)?)`)

	// Brightness: "яркость в зале 50", "поставь яркость торшера 50".
	brightnessPlaceRE  = regexp.MustCompile(`(?i)^(?:поставь|установи|сделай|включи)?\s*яркость\s+(?:на\s+)?(\d{1,3})(?:\s*(?:%|процент\w*))?(?:\s+(?:в|на|у)\s+(.+))?$`)
	brightnessDeviceRE = regexp.MustCompile(`(?i)^(?:поставь|установи|сделай|включи)?\s*яркость\s+(.+?)\s+(?:на\s+)?(\d{1,3})(?:\s*(?:%|процент\w*))?$`)

	// Room first: "яркость в зале 40" ("яркость на кухне 30").
	brightnessRoomRE = regexp.MustCompile(`(?i)^(?:поставь|установи|сделай|включи)?\s*яркость\s+(?:в|на|у)\s+(.+?)\s+(?:на\s+)?(\d{1,3})(?:\s*(?:%|процент\w*))?$`)

	// Scenes: "включи сцену Спокойной ночи".
	sceneRE = regexp.MustCompile(`(?i)^(запусти|включи|активируй|выполни)\s+сцену\s+(.+)$`)
)

var conditionsRU = map[string]string{
	"clear sky":             "ясно",
	"mainly clear":          "малооблачно",
	"partly cloudy":         "переменная облачность",
	"overcast":              "пасмурно",
	"fog":                   "туман",
	"depositing rime fog":   "изморозь",
	"light drizzle":         "слабая морось",
	"moderate drizzle":      "морось",
	"dense drizzle":         "сильная морось",
	"light rain":            "небольшой дождь",
	"slight rain":           "небольшой дождь",
	"moderate rain":         "дождь",
	"heavy rain":            "сильный дождь",
	"rain showers":          "ливень",
	"slight rain showers":   "небольшой ливень",
	"moderate rain showers": "ливень",
	"light snow":            "небольшой снег",
	"slight snow":           "небольшой снег",
	"moderate snow":         "снег",
	"heavy snow":            "сильный снег",
	"snow showers":          "снегопад",
	"thunderstorm":          "гроза",
}

// fastPath returns a spoken reply and true when text is a short speaker
// command or a weather/time question. Tools run through the usual server
// dispatcher, so the result, the agent command and the panel history stay
// the same as for a model-issued call.
func (c *conversation) fastPath(ctx context.Context, text string, out *pipeline.Output) (message, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	t = stripWakeWord(t)
	if t == "" {
		return message{}, false
	}

	if weatherRE.MatchString(t) && strings.Contains(t, "погод") {
		args := map[string]any{}
		if strings.Contains(t, "завтра") {
			args["days"] = 2
		}
		res, fail, ok := c.callTool(ctx, "weather_forecast", args, out)
		if !ok {
			return message{}, true
		}
		reply := fail
		if reply == "" {
			reply = weatherReply(res, strings.Contains(t, "завтра"))
		}
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}

	if timeRE.MatchString(t) {
		res, fail, ok := c.callTool(ctx, "time_now", map[string]any{}, out)
		if !ok {
			return message{}, true
		}
		reply := fail
		if reply == "" {
			reply = timeReply(res)
		}
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}

	if dateRE.MatchString(t) {
		res, fail, ok := c.callTool(ctx, "time_now", map[string]any{}, out)
		if !ok {
			return message{}, true
		}
		reply := fail
		if reply == "" {
			reply = dateReply(res)
		}
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}

	if !commandRE.MatchString(t) {
		return message{}, false
	}
	call := provider.ToolCall{ID: "fast-1"}
	reply := ""
	switch {
	case volumeSetRE.MatchString(t):
		m := volumeSetRE.FindStringSubmatch(t)
		level, _ := strconv.Atoi(m[1])
		if level > 100 {
			level = 100
		}
		call.Name, call.Args = "volume_set", map[string]any{"level": level}
		reply = fmt.Sprintf("Громкость %d процентов.", level)
	case muteOnRE.MatchString(t):
		call.Name, call.Args = "volume_mute", map[string]any{"muted": true}
		reply = "Звук выключен."
	case muteOffRE.MatchString(t):
		call.Name, call.Args = "volume_mute", map[string]any{"muted": false}
		reply = "Звук включён."
	case volumeUpRE.MatchString(t):
		call.Name, call.Args = "volume_adjust", map[string]any{"delta": volumeDelta(t)}
		reply = "Сделала громче."
	case volumeDownRE.MatchString(t):
		call.Name, call.Args = "volume_adjust", map[string]any{"delta": -volumeDelta(t)}
		reply = "Сделала тише."
	case mediaNextRE.MatchString(t):
		call.Name, call.Args = "media_control", map[string]any{"operation": "next"}
		reply = "Следующий трек."
	case mediaPrevRE.MatchString(t):
		call.Name, call.Args = "media_control", map[string]any{"operation": "previous"}
		reply = "Предыдущий трек."
	case mediaPauseRE.MatchString(t):
		call.Name, call.Args = "media_control", map[string]any{"operation": "pause"}
		reply = "Пауза."
	case mediaPlayRE.MatchString(t):
		call.Name, call.Args = "media_control", map[string]any{"operation": "play"}
		reply = "Продолжаю."
	default:
		if msg, ok := c.sceneFastPath(ctx, t, out); ok {
			return msg, true
		}
		if msg, ok := c.brightnessFastPath(ctx, t, out); ok {
			return msg, true
		}
		if msg, ok := c.homeFastPath(ctx, t, out); ok {
			return msg, true
		}
		return message{}, false
	}
	_, fail, ok := c.callTool(ctx, call.Name, call.Args, out)
	if !ok {
		return message{}, true // the turn was cancelled
	}
	if fail != "" {
		reply = fail
	}
	out.Say(reply)
	return message{Role: "assistant", Content: reply}, true
}

// callTool runs one tool and turns transport or tool errors into a short
// spoken apology.
func (c *conversation) callTool(ctx context.Context, name string, args map[string]any, out *pipeline.Output) (map[string]any, string, bool) {
	results, ok := out.CallTools([]provider.ToolCall{{ID: "fast-1", Name: name, Args: args}})
	if !ok {
		return nil, "", false
	}
	if len(results) == 0 {
		return nil, "Не получилось.", true
	}
	if results[0].Err != "" {
		err := strings.ToLower(results[0].Err)
		if strings.Contains(err, "nothing is playing") {
			return nil, "Сейчас ничего не играет.", true
		}
		return nil, "Не получилось.", true
	}
	return results[0].Output, "", true
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func conditionRU(s string) string {
	return conditionsRU[strings.ToLower(strings.TrimSpace(s))]
}

func dailyAt(res map[string]any, i int) (map[string]any, bool) {
	switch d := res["daily"].(type) {
	case []any:
		if i < len(d) {
			m, ok := d[i].(map[string]any)
			return m, ok
		}
	case []map[string]any:
		if i < len(d) {
			return d[i], true
		}
	}
	return nil, false
}

func weatherReply(res map[string]any, tomorrow bool) string {
	if tomorrow {
		if d, ok := dailyAt(res, 1); ok {
			min, _ := number(d["min_c"])
			max, _ := number(d["max_c"])
			cond := conditionRU(fmt.Sprint(d["conditions"]))
			if cond != "" {
				return fmt.Sprintf("Завтра от %.0f до %.0f градусов, %s.", min, max, cond)
			}
			return fmt.Sprintf("Завтра от %.0f до %.0f градусов.", min, max)
		}
	}
	if cur, ok := res["current"].(map[string]any); ok {
		temp, _ := number(cur["temperature_c"])
		cond := conditionRU(fmt.Sprint(cur["conditions"]))
		if cond != "" {
			return fmt.Sprintf("Сейчас %.0f градусов, %s.", temp, cond)
		}
		return fmt.Sprintf("Сейчас %.0f градусов.", temp)
	}
	return "Не получилось узнать погоду."
}

func timeReply(res map[string]any) string {
	if t, ok := res["time"].(string); ok && t != "" {
		return "Сейчас " + t + "."
	}
	return "Не получилось узнать время."
}

// homeFastPath handles simple "turn on/off <device>" commands. It finds the
// device through the Yandex Smart Home skill and asks for a room when the
// name is ambiguous.
func (c *conversation) homeFastPath(ctx context.Context, t string, out *pipeline.Output) (message, bool) {
	m := homeOnRE.FindStringSubmatch(t)
	on := true
	if m == nil {
		m = homeOffRE.FindStringSubmatch(t)
		on = false
	}
	if m == nil {
		return message{}, false
	}
	query := strings.TrimSpace(m[2])
	query = strings.TrimSuffix(query, " пожалуйста")
	if query == "" {
		return message{}, false
	}
	res, fail, ok := c.callTool(ctx, "yandex_home_find", map[string]any{"query": query}, out)
	if !ok {
		return message{}, true
	}
	if fail != "" {
		out.Say(fail)
		return message{Role: "assistant", Content: fail}, true
	}
	devices := deviceList(res)
	if len(devices) == 0 {
		reply := "Не нашла такое устройство."
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}
	if len(devices) > 1 {
		names := make([]string, 0, min(len(devices), 5))
		for _, d := range devices[:min(len(devices), 5)] {
			names = append(names, deviceName(d))
		}
		reply := "Нашла несколько: " + strings.Join(names, ", ") + ". Уточни комнату."
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}
	id, _ := devices[0]["id"].(string)
	action := "turn_off"
	if on {
		action = "turn_on"
	}
	_, fail, ok = c.callTool(ctx, "yandex_home_control", map[string]any{"id": id, "action": action}, out)
	if !ok {
		return message{}, true
	}
	reply := "Готово."
	if fail != "" {
		reply = fail
	} else if on {
		reply = "Включила."
	} else {
		reply = "Выключила."
	}
	out.Say(reply)
	return message{Role: "assistant", Content: reply}, true
}

func deviceList(res map[string]any) []map[string]any {
	switch d := res["devices"].(type) {
	case []any:
		out := make([]map[string]any, 0, len(d))
		for _, v := range d {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return d
	}
	return nil
}

func deviceName(d map[string]any) string {
	name, _ := d["name"].(string)
	room, _ := d["room"].(string)
	if room != "" {
		return name + " (" + room + ")"
	}
	return name
}

// stripWakeWord removes a leading wake word that speech recognition kept in
// the transcript.
func stripWakeWord(t string) string {
	for _, w := range []string{"маруся"} {
		if strings.HasPrefix(t, w) {
			return strings.TrimLeft(strings.TrimPrefix(t, w), " ,.!?-")
		}
	}
	return t
}

// volumeDelta reads "на 20" from a relative volume command; the default step
// is ten, and the step is capped so one phrase cannot jump the whole range.
func volumeDelta(t string) int {
	m := volumeDeltaRE.FindStringSubmatch(t)
	if m == nil {
		return 10
	}
	d, err := strconv.Atoi(m[1])
	if err != nil || d <= 0 {
		return 10
	}
	return min(d, 50)
}

var monthsRU = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}

var weekdaysRU = map[string]string{
	"Monday": "понедельник", "Tuesday": "вторник", "Wednesday": "среда",
	"Thursday": "четверг", "Friday": "пятница", "Saturday": "суббота", "Sunday": "воскресенье",
}

// dateReply answers "какое сегодня число" and "какой сегодня день" from the
// time skill, which already returns the date and the weekday.
func dateReply(res map[string]any) string {
	date, _ := res["date"].(string)
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "Не получилось узнать дату."
	}
	day := fmt.Sprintf("%d %s", t.Day(), monthsRU[t.Month()-1])
	weekday, _ := res["weekday"].(string)
	if w := weekdaysRU[weekday]; w != "" {
		return fmt.Sprintf("Сегодня %s, %s.", w, day)
	}
	return "Сегодня " + day + "."
}

// sceneFastPath runs a Yandex Smart Home scene by name, e.g. "включи сцену
// Спокойной ночи".
func (c *conversation) sceneFastPath(ctx context.Context, t string, out *pipeline.Output) (message, bool) {
	m := sceneRE.FindStringSubmatch(t)
	if m == nil {
		return message{}, false
	}
	query := strings.TrimSuffix(strings.TrimSpace(m[2]), " пожалуйста")
	if query == "" {
		return message{}, false
	}
	res, fail, ok := c.callTool(ctx, "yandex_home_find", map[string]any{"query": query}, out)
	if !ok {
		return message{}, true
	}
	if fail != "" {
		out.Say(fail)
		return message{Role: "assistant", Content: fail}, true
	}
	var found map[string]any
	for _, sc := range scenarioList(res) {
		name, _ := sc["name"].(string)
		if strings.Contains(strings.ToLower(name), strings.ToLower(query)) {
			found = sc
			break
		}
	}
	if found == nil {
		reply := "Не нашла такую сцену."
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}
	id, _ := found["id"].(string)
	name, _ := found["name"].(string)
	_, fail, ok = c.callTool(ctx, "yandex_home_scene", map[string]any{"id": id}, out)
	if !ok {
		return message{}, true
	}
	reply := fmt.Sprintf("Запустила сцену «%s».", name)
	if fail != "" {
		reply = fail
	}
	out.Say(reply)
	return message{Role: "assistant", Content: reply}, true
}

// brightnessFastPath sets a light's brightness, e.g. "яркость в зале 50" or
// "поставь яркость торшера 50". Without a device or a room it stays with the
// model, which can ask a question.
func (c *conversation) brightnessFastPath(ctx context.Context, t string, out *pipeline.Output) (message, bool) {
	level := 0
	query := ""
	switch {
	case brightnessRoomRE.MatchString(t):
		m := brightnessRoomRE.FindStringSubmatch(t)
		query = strings.TrimSpace(m[1])
		level, _ = strconv.Atoi(m[2])
	case brightnessDeviceRE.MatchString(t):
		m := brightnessDeviceRE.FindStringSubmatch(t)
		query = strings.TrimSpace(m[1])
		level, _ = strconv.Atoi(m[2])
	case brightnessPlaceRE.MatchString(t):
		m := brightnessPlaceRE.FindStringSubmatch(t)
		level, _ = strconv.Atoi(m[1])
		query = strings.TrimSpace(m[2])
	default:
		return message{}, false
	}
	query = strings.TrimSuffix(query, " пожалуйста")
	if query == "" {
		return message{}, false
	}
	level = max(1, min(100, level))
	res, fail, ok := c.callTool(ctx, "yandex_home_find", map[string]any{"query": query}, out)
	if !ok {
		return message{}, true
	}
	if fail != "" {
		out.Say(fail)
		return message{Role: "assistant", Content: fail}, true
	}
	devices := deviceList(res)
	if len(devices) == 0 {
		reply := "Не нашла такое устройство."
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}
	if len(devices) > 1 {
		names := make([]string, 0, min(len(devices), 5))
		for _, d := range devices[:min(len(devices), 5)] {
			names = append(names, deviceName(d))
		}
		reply := "Нашла несколько: " + strings.Join(names, ", ") + ". Уточни комнату."
		out.Say(reply)
		return message{Role: "assistant", Content: reply}, true
	}
	id, _ := devices[0]["id"].(string)
	_, fail, ok = c.callTool(ctx, "yandex_home_control", map[string]any{"id": id, "action": "set_brightness", "value": level}, out)
	if !ok {
		return message{}, true
	}
	reply := fmt.Sprintf("Яркость %d процентов.", level)
	if fail != "" {
		reply = fail
	}
	out.Say(reply)
	return message{Role: "assistant", Content: reply}, true
}

func scenarioList(res map[string]any) []map[string]any {
	switch s := res["scenarios"].(type) {
	case []any:
		out := make([]map[string]any, 0, len(s))
		for _, v := range s {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return s
	}
	return nil
}
