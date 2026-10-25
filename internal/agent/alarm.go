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

package agent

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/audio"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

const (
	// ringFor stops an alarm nobody silenced.
	ringFor = 10 * time.Minute
	// ringLate skips rings that are more than this late.
	ringLate = 2 * time.Minute
	// musicTimeout bounds finding the music of an alarm; beeps follow on
	// failure.
	musicTimeout = 25 * time.Second
)

// alarmClock rings the alarms the server sends (models.TypeAlarms). It works
// on its own, also while the server is unreachable.
type alarmClock struct {
	a *Agent

	mu      sync.Mutex
	ctx     context.Context
	rings   []scheduledRing // by time
	timer   *time.Timer
	fired   map[string]time.Time // alarm ID → last ring, against duplicates
	ringing context.CancelFunc
	stopped chan struct{} // closed when the current ring has cleaned up
}

type scheduledRing struct {
	at time.Time
	models.AlarmRing
}

func newAlarmClock(a *Agent) *alarmClock {
	return &alarmClock{a: a, fired: map[string]time.Time{}}
}

// start enables ringing until ctx is done.
func (c *alarmClock) start(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.scheduleLocked()
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.stop()
	}()
}

// set replaces the upcoming rings.
func (c *alarmClock) set(list []models.AlarmRing) {
	now := time.Now()
	rings := make([]scheduledRing, 0, len(list))
	for _, r := range list {
		rings = append(rings, scheduledRing{at: now.Add(time.Duration(r.InMS) * time.Millisecond), AlarmRing: r})
	}
	slices.SortFunc(rings, func(x, y scheduledRing) int { return x.at.Compare(y.at) })
	c.mu.Lock()
	c.rings = rings
	c.scheduleLocked()
	c.mu.Unlock()
	if len(rings) > 0 {
		c.a.log.Info("alarms updated", "rings", len(rings), "next", rings[0].at.Format("2006-01-02 15:04:05"),
			"in", time.Until(rings[0].at).Round(time.Second), "label", rings[0].Label, "volume", rings[0].Volume)
	} else {
		c.a.log.Info("alarms updated", "rings", 0)
	}
}

func (c *alarmClock) scheduleLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.ctx == nil || len(c.rings) == 0 {
		return
	}
	c.timer = time.AfterFunc(max(0, time.Until(c.rings[0].at)), c.due)
}

// due rings the alarms whose time has come.
func (c *alarmClock) due() {
	now := time.Now()
	c.mu.Lock()
	var ring *scheduledRing
	for len(c.rings) > 0 && !c.rings[0].at.After(now.Add(500*time.Millisecond)) {
		r := c.rings[0]
		c.rings = c.rings[1:]
		if last, ok := c.fired[r.AlarmID]; ok && r.at.Sub(last).Abs() < ringLate {
			continue // the same ring sent again
		}
		if now.Sub(r.at) > ringLate {
			c.a.log.Warn("alarm skipped: too late", "alarm", r.AlarmID, "late", now.Sub(r.at).Round(time.Second))
			continue
		}
		c.fired[r.AlarmID] = r.at
		if ring == nil {
			ring = &r
		}
	}
	c.scheduleLocked()
	c.mu.Unlock()
	if ring != nil {
		c.ring(ring.AlarmRing)
	}
}

// ring starts ringing, replacing a ring in progress.
func (c *alarmClock) ring(r models.AlarmRing) {
	c.stop()
	c.mu.Lock()
	if c.ctx == nil || c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, ringFor)
	stopped := make(chan struct{})
	c.ringing, c.stopped = cancel, stopped
	c.mu.Unlock()

	c.a.log.Info("alarm ringing", "alarm", r.AlarmID, "label", r.Label, "music", r.Music, "volume", r.Volume)
	go func() {
		defer close(stopped)
		defer cancel()
		c.run(ctx, r)
	}()
}

// stop silences the alarm and reports whether one was ringing.
func (c *alarmClock) stop() bool {
	c.mu.Lock()
	cancel, stopped := c.ringing, c.stopped
	c.ringing, c.stopped = nil, nil
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	<-stopped
	c.a.log.Info("alarm stopped")
	return true
}

// run rings until ctx is done: the alarm's music, or beeps.
func (c *alarmClock) run(ctx context.Context, r models.AlarmRing) {
	a := c.a
	// Independent of ctx: cleanup runs after it is cancelled.
	bg := context.WithoutCancel(ctx)
	short := func() (context.Context, context.CancelFunc) { return context.WithTimeout(bg, 5*time.Second) }

	a.indicator.Show(indicator.Announcing)
	defer func() {
		if !a.Busy() {
			a.indicator.Show(indicator.Idle)
		}
	}()

	if m := a.dispatcher.Media; m != nil {
		sctx, cancel := short()
		if st, err := m.Status(sctx); err == nil && st.Playing {
			m.Control(sctx, models.MediaPause)
		}
		cancel()
	}
	if v := a.dispatcher.Volume; v != nil && r.Volume > 0 {
		sctx, cancel := short()
		prev, err := v.Get(sctx)
		if err := v.Set(sctx, r.Volume); err != nil {
			a.log.Warn("alarm volume", "err", err)
		}
		if prev.Muted {
			v.SetMuted(sctx, false)
		}
		cancel()
		if err == nil {
			defer func() {
				sctx, cancel := short()
				defer cancel()
				v.Set(sctx, prev.Level)
				if prev.Muted {
					v.SetMuted(sctx, true)
				}
			}()
		}
	}

	if r.Music != "" {
		if ms, ok := a.dispatcher.Media.(controller.MediaSearcher); ok {
			mctx, cancel := context.WithTimeout(ctx, musicTimeout)
			err := ms.PlaySearch(mctx, r.Music)
			cancel()
			if err == nil {
				<-ctx.Done()
				sctx, cancel := short()
				ms.Control(sctx, models.MediaStop)
				cancel()
				return
			}
			a.log.Warn("alarm music failed; beeping instead", "music", r.Music, "err", err)
		}
	}
	c.beep(ctx)
}

// beep plays the alarm pattern, getting louder, until ctx is done.
func (c *alarmClock) beep(ctx context.Context) {
	f := models.DefaultOutputFormat
	start := time.Now()
	for ctx.Err() == nil {
		gain := min(0.9, 0.35+0.55*time.Since(start).Seconds()/60)
		var pcm []byte
		for i := 0; i < 4; i++ {
			pcm = append(pcm, audio.Tone(f, 988, 110, gain)...)
			pcm = append(pcm, audio.Silence(f, 90)...)
		}
		pcm = append(pcm, audio.Silence(f, 600)...)
		if err := c.play(ctx, f, pcm); err != nil {
			c.a.log.Warn("alarm sound", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

// play plays pcm to the end, or until ctx is done.
func (c *alarmClock) play(ctx context.Context, f models.AudioFormat, pcm []byte) error {
	st, err := c.a.player.Open(ctx, f)
	if err != nil {
		return err
	}
	if err := st.Write(pcm); err != nil {
		st.Abort()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- st.Drain() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		st.Abort()
		<-done
		return nil
	}
}
