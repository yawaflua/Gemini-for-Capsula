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

package server

import (
	"time"

	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// alarmWindow is how far ahead the rings are sent to speakers: they keep
// ringing for this long even if the server is down.
const alarmWindow = 8 * 24 * time.Hour

// alarmRefresh is how often speakers get their rings again, which moves
// the window and drops one-time alarms that have rung.
const alarmRefresh = time.Hour

// Alarms returns the alarm store.
func (s *Server) Alarms() *alarms.Store { return s.alarms }

// alarmsChanged sends the new rings to the speaker.
func (s *Server) alarmsChanged(device string) {
	for _, d := range s.sessions() {
		if d.hello != nil && d.hello.DeviceID == device {
			d.kickAlarms()
		}
	}
	s.events.devicesChanged()
}

func (d *deviceSession) kickAlarms() {
	select {
	case d.alarmKick <- struct{}{}:
	default:
	}
}

// location is the time zone of the speaker.
func (d *deviceSession) location() *time.Location {
	if tz := d.effective().TimeZone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.Local
}

// runAlarms keeps the speaker's list of rings current.
func (d *deviceSession) runAlarms() {
	t := time.NewTicker(alarmRefresh)
	defer t.Stop()
	for {
		d.pushAlarms()
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
		case <-d.alarmKick:
		}
	}
}

// pushAlarms sends the upcoming rings to the speaker.
func (d *deviceSession) pushAlarms() {
	id, loc, now := d.hello.DeviceID, d.location(), time.Now()
	store := d.srv.alarms
	store.Prune(id, now, loc) // may kick again; harmless
	var rings []models.AlarmRing
	for _, a := range store.List(id) {
		for _, at := range a.Rings(now, now.Add(alarmWindow), loc) {
			rings = append(rings, models.AlarmRing{
				AlarmID: a.ID, InMS: at.Sub(now).Milliseconds(),
				Label: a.Label, Music: a.Music, Volume: a.Volume,
			})
		}
	}
	if err := d.send(&models.Message{Type: models.TypeAlarms, Alarms: &models.Alarms{Rings: rings}}); err != nil {
		d.log.Warn("send alarms", "err", err)
		return
	}
	if len(rings) > 0 {
		d.log.Info("alarms sent", "rings", len(rings), "next_in", (time.Duration(rings[0].InMS) * time.Millisecond).Round(time.Second))
	}
}
