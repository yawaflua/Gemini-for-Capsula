// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package alarm

import (
	"context"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
)

func TestSetListDelete(t *testing.T) {
	store, _ := alarms.Open("")
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	// Saturday 2026-10-24 23:10 in Israel.
	now := time.Date(2026, 10, 24, 23, 10, 0, 0, loc)
	b := &base{store: store, now: func() time.Time { return now }}
	sk := skills.Set{&set{b}, &list{b}, &del{b}}
	dev := skills.Device{ID: "k1", TimeZone: "Asia/Jerusalem", Features: []models.Feature{models.FeatureAlarms}}
	ctx := context.Background()

	r, err := sk[0].Run(ctx, dev, map[string]any{"time": "7:30", "volume": float64(90)})
	if err != nil {
		t.Fatal(err)
	}
	if r["next_ring"] != "2026-10-25 07:30 Sunday" || r["volume"] != 90 || r["repeat"] != "once" {
		t.Errorf("alarm_set = %v", r)
	}
	r, err = sk[0].Run(ctx, dev, map[string]any{"time": "07:00", "repeat": "weekdays"})
	if err != nil || r["next_ring"] != "2026-10-25 07:00 Sunday" || r["repeat"] != "weekdays (sun-thu)" {
		t.Errorf("weekdays alarm = %v, %v", r, err)
	}
	r, err = sk[0].Run(ctx, dev, map[string]any{"in_minutes": float64(10)})
	if err != nil || r["next_ring"] != "2026-10-24 23:20 Saturday" || r["label"] != "таймер" {
		t.Errorf("timer = %v, %v", r, err)
	}
	l, _ := sk[1].Run(ctx, dev, nil)
	if n := len(l["alarms"].([]map[string]any)); n != 3 {
		t.Errorf("listed %d alarms", n)
	}
	d, err := sk[2].Run(ctx, dev, map[string]any{"time": "7:30"})
	if err != nil || d["deleted"] != 1 {
		t.Errorf("delete by time = %v, %v", d, err)
	}
	d, _ = sk[2].Run(ctx, dev, map[string]any{"all": true})
	if d["deleted"] != 2 {
		t.Errorf("delete all = %v", d)
	}
	if _, err := sk[0].Run(ctx, skills.Device{ID: "old"}, map[string]any{"time": "7:30"}); err != ErrNoRinging {
		t.Errorf("old speaker: err = %v", err)
	}
}
