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

// Package alarm implements the alarm_set, alarm_list and alarm_delete
// skills: alarm clocks and timers of the speaker (see package alarms).
package alarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Skills returns the alarm skills backed by store.
func Skills(store *alarms.Store) skills.Set {
	b := &base{store: store}
	return skills.Set{&set{b}, &list{b}, &del{b}}
}

type base struct {
	store *alarms.Store
	// now overrides the clock in tests.
	now func() time.Time
}

func (b *base) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// ErrNoRinging is returned for speakers whose agent cannot ring alarms.
var ErrNoRinging = errors.New("this speaker cannot ring alarms yet: restart it to update its software")

func (b *base) prepare(dev skills.Device) (*time.Location, error) {
	if dev.ID == "" {
		return nil, errors.New("unknown speaker")
	}
	if !dev.Supports(models.FeatureAlarms) {
		return nil, ErrNoRinging
	}
	loc := time.UTC
	if dev.TimeZone != "" {
		l, err := time.LoadLocation(dev.TimeZone)
		if err != nil {
			return nil, err
		}
		loc = l
	}
	b.store.Prune(dev.ID, b.clock(), loc)
	return loc, nil
}

// describe presents an alarm to the model.
func (b *base) describe(a alarms.Alarm, loc *time.Location) map[string]any {
	m := map[string]any{"id": a.ID, "time": a.Time}
	if a.Label != "" {
		m["label"] = a.Label
	}
	if a.Music != "" {
		m["music"] = a.Music
	}
	if a.Repeats() {
		m["repeat"] = repeatName(a.Days, loc)
	} else {
		m["repeat"] = "once"
	}
	if a.Volume > 0 {
		m["volume"] = a.Volume
	}
	if a.Off {
		m["off"] = true
	}
	now := b.clock()
	if next, ok := a.Next(now, loc); ok {
		in := next.Sub(now).Round(time.Minute)
		m["next_ring"] = next.Format("2006-01-02 15:04 Monday")
		m["rings_in"] = fmt.Sprintf("%dh%02dm", int(in.Hours()), int(in.Minutes())%60)
	}
	return m
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

var (
	everyDay = []time.Weekday{0, 1, 2, 3, 4, 5, 6}
	workdays = []time.Weekday{1, 2, 3, 4, 5}
	weekends = []time.Weekday{0, 6}
)

// Israel works Sunday to Thursday.
var (
	workdaysIL = []time.Weekday{0, 1, 2, 3, 4}
	weekendsIL = []time.Weekday{5, 6}
)

func israeli(loc *time.Location) bool {
	return loc.String() == "Asia/Jerusalem" || loc.String() == "Asia/Tel_Aviv"
}

// parseRepeat understands "once", "daily", "weekdays", "weekends" (by the
// local work week) and lists like "mon,wed,fri".
func parseRepeat(s string, loc *time.Location) ([]time.Weekday, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "once":
		return nil, nil
	case "daily", "every day", "everyday":
		return everyDay, nil
	case "weekdays", "workdays":
		if israeli(loc) {
			return workdaysIL, nil
		}
		return workdays, nil
	case "weekends":
		if israeli(loc) {
			return weekendsIL, nil
		}
		return weekends, nil
	}
	var out []time.Weekday
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		d, ok := weekdays[f[:min(3, len(f))]]
		if !ok {
			return nil, fmt.Errorf("unknown weekday %q", f)
		}
		out = append(out, d)
	}
	return out, nil
}

func repeatName(days []time.Weekday, loc *time.Location) string {
	key := fmt.Sprint(days)
	if israeli(loc) {
		switch key {
		case fmt.Sprint(workdaysIL):
			return "weekdays (sun-thu)"
		case fmt.Sprint(weekendsIL):
			return "weekends (fri-sat)"
		}
	}
	switch key {
	case fmt.Sprint(everyDay):
		return "daily"
	case fmt.Sprint(workdays):
		return "weekdays"
	case fmt.Sprint(weekends):
		return "weekends"
	}
	names := make([]string, len(days))
	for i, d := range days {
		names[i] = strings.ToLower(d.String()[:3])
	}
	return strings.Join(names, ",")
}

// set is alarm_set.
type set struct{ *base }

func (s *set) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("alarm.set"),
		Description: "Sets an alarm clock or a timer on the speaker. The speaker rings (or plays music) at that time until the user taps it or says \"Маруся, стоп\". " +
			"\"Разбуди в 7:30\" → time 07:30; \"будильник по будням на 7\" → time 07:00, repeat weekdays; " +
			"\"таймер на 10 минут\" or \"напомни через полчаса\" → in_minutes, label \"таймер\". " +
			"Confirm with the time and day from the result (next_ring), e.g. \"Разбужу завтра в 7:30\".",
		Parameters: []tools.Parameter{
			{Name: "time", Type: tools.TypeString, Description: "Local time of day, HH:MM (24h). Omit when in_minutes or in_seconds is given."},
			{Name: "in_minutes", Type: tools.TypeInteger, Description: "Ring this many minutes from now (timers).", Minimum: ptr(0), Maximum: ptr(60 * 24 * 7)},
			{Name: "in_seconds", Type: tools.TypeInteger, Description: "Seconds added to in_minutes, e.g. 30 for \"таймер на 30 секунд\".", Minimum: ptr(0), Maximum: ptr(3600)},
			{Name: "repeat", Type: tools.TypeString, Description: "\"once\" (default), \"daily\", \"weekdays\" (the local work week; Sun-Thu in Israel), \"weekends\" or days like \"mon,wed,fri\"."},
			{Name: "date", Type: tools.TypeString, Description: "For a one-time alarm on a specific day: YYYY-MM-DD. Default: the next time this time of day comes."},
			{Name: "label", Type: tools.TypeString, Description: "What it is for, e.g. \"работа\", \"таймер\", \"пицца\"."},
			{Name: "volume", Type: tools.TypeInteger, Description: "Speaker volume while ringing, percent; only if the user asked for it.", Minimum: ptr(1), Maximum: ptr(100)},
			{Name: "music", Type: tools.TypeString, Description: "Song or video to wake up to (searched on YouTube) instead of beeps, only if the user asked for it."},
		},
	}
}

func ptr(f float64) *float64 { return &f }

func (s *set) Run(_ context.Context, dev skills.Device, args map[string]any) (map[string]any, error) {
	loc, err := s.prepare(dev)
	if err != nil {
		return nil, err
	}
	now := s.clock().In(loc)
	a := alarms.Alarm{Label: skills.String(args, "label"), Music: skills.String(args, "music"), Volume: skills.Int(args, "volume", 0)}
	if in := time.Duration(skills.Int(args, "in_minutes", 0))*time.Minute + time.Duration(skills.Int(args, "in_seconds", 0))*time.Second; in > 0 {
		t := now.Add(in).Round(time.Second)
		a.Time, a.Date = t.Format("15:04:05"), t.Format(time.DateOnly)
		if a.Label == "" {
			a.Label = "таймер"
		}
	} else {
		tm, err := time.Parse("15:04", strings.TrimSpace(skills.String(args, "time")))
		if err != nil {
			return nil, errors.New("give time as HH:MM, or in_minutes / in_seconds")
		}
		a.Time = tm.Format("15:04")
		if a.Days, err = parseRepeat(skills.String(args, "repeat"), loc); err != nil {
			return nil, err
		}
		if !a.Repeats() {
			a.Date = strings.TrimSpace(skills.String(args, "date"))
			if a.Date == "" {
				// The next time this time of day comes.
				t := time.Date(now.Year(), now.Month(), now.Day(), tm.Hour(), tm.Minute(), 0, 0, loc)
				if !t.After(now) {
					t = t.AddDate(0, 0, 1)
				}
				a.Date = t.Format(time.DateOnly)
			}
		}
	}
	a, err = s.store.Add(dev.ID, a)
	if err != nil {
		return nil, err
	}
	out := s.describe(a, loc)
	if _, ok := out["next_ring"]; !ok && !a.Repeats() {
		s.store.Delete(dev.ID, a.ID)
		return nil, fmt.Errorf("%s %s has already passed", a.Date, a.Time)
	}
	out["now"] = now.Format("2006-01-02 15:04 Monday")
	return out, nil
}

// list is alarm_list.
type list struct{ *base }

func (l *list) Tool() tools.Tool {
	return tools.Tool{
		Action:      models.Action("alarm.list"),
		Description: "Lists the alarms and timers of the speaker with their next ring time.",
	}
}

func (l *list) Run(_ context.Context, dev skills.Device, _ map[string]any) (map[string]any, error) {
	loc, err := l.prepare(dev)
	if err != nil {
		return nil, err
	}
	all := l.store.List(dev.ID)
	out := make([]map[string]any, 0, len(all))
	for _, a := range all {
		out = append(out, l.describe(a, loc))
	}
	return map[string]any{"alarms": out, "now": l.clock().In(loc).Format("2006-01-02 15:04 Monday")}, nil
}

// del is alarm_delete.
type del struct{ *base }

func (d *del) Tool() tools.Tool {
	return tools.Tool{
		Action:      models.Action("alarm.delete"),
		Description: "Deletes alarms or timers of the speaker: by id (from alarm_list), by time, or all of them. To silence an alarm that is ringing now, use media_control stop instead.",
		Parameters: []tools.Parameter{
			{Name: "id", Type: tools.TypeString, Description: "Alarm id from alarm_list or alarm_set."},
			{Name: "time", Type: tools.TypeString, Description: "Delete the alarms set for this time, HH:MM."},
			{Name: "all", Type: tools.TypeBoolean, Description: "Delete every alarm and timer."},
		},
	}
}

func (d *del) Run(_ context.Context, dev skills.Device, args map[string]any) (map[string]any, error) {
	if _, err := d.prepare(dev); err != nil {
		return nil, err
	}
	var ids []string
	id, at := strings.TrimSpace(skills.String(args, "id")), strings.TrimSpace(skills.String(args, "time"))
	switch {
	case id != "":
		ids = []string{id}
	case at != "":
		if t, err := time.Parse("15:04", at); err == nil {
			at = t.Format("15:04")
		}
		for _, a := range d.store.List(dev.ID) {
			if a.Time == at {
				ids = append(ids, a.ID)
			}
		}
	case skills.Bool(args, "all"):
	default:
		return nil, errors.New("give id, time or all")
	}
	if len(ids) == 0 && !skills.Bool(args, "all") {
		return map[string]any{"deleted": 0}, nil
	}
	n, err := d.store.Delete(dev.ID, ids...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": n}, nil
}
