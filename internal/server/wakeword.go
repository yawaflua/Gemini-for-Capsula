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
	"context"
	"slices"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/wake"
)

// Wake word states shown in the web panel.
const (
	wakeOff         = "off"         // no keywords, no spotter or an old agent
	wakeOn          = "on"          // the spotter listens to the idle speaker
	wakeUnavailable = "unavailable" // the spotter cannot be reached
)

// serverWake reports whether the server spots the wake word for this device.
func (d *deviceSession) serverWake() bool {
	return d.cfg.WakeURL != "" && slices.Contains(d.hello.Features, models.FeatureServerWake)
}

// kickWake makes runWake re-read the keywords.
func (d *deviceSession) kickWake() {
	select {
	case d.wakeKick <- struct{}{}:
	default:
	}
}

func (d *deviceSession) keywords() []string {
	kws, _ := wake.ParseKeywords(d.srv.Settings().WakeWords)
	return kws
}

func (d *deviceSession) setWake(st *wake.Stream, state string) {
	d.mu.Lock()
	changed := d.wakeState != state
	d.wake, d.wakeState = st, state
	d.mu.Unlock()
	if changed {
		d.srv.events.devicesChanged()
	}
}

// feedWake passes idle microphone audio to the spotter.
func (d *deviceSession) feedWake(pcm []byte) {
	d.mu.Lock()
	st := d.wake
	d.mu.Unlock()
	if st != nil {
		st.Write(pcm) // a failed stream is noticed by runWake
	}
}

// runWake keeps a spotter stream open while there are keywords and tells
// the device to stream its idle audio accordingly.
func (d *deviceSession) runWake(ctx context.Context) {
	backoff := time.Second
	failing := false
	for ctx.Err() == nil {
		kws := d.keywords()
		if len(kws) == 0 {
			d.setWake(nil, wakeOff)
			select {
			case <-ctx.Done():
				return
			case <-d.wakeKick:
			}
			continue
		}

		st, err := wake.Dial(ctx, d.cfg.WakeURL, kws, d.hello.InputFormat.SampleRateHertz)
		if err != nil {
			if !failing {
				d.log.Warn("wake word spotter unavailable; retrying", "url", d.cfg.WakeURL, "err", err)
				d.srv.events.publish(Event{Kind: EventError, Session: d.id, Device: d.hello.DeviceID,
					Error: "распознаватель ключевого слова недоступен: " + err.Error()})
				failing = true
			}
			d.setWake(nil, wakeUnavailable)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			case <-d.wakeKick:
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if failing {
			d.log.Info("wake word spotter is back")
		}
		failing, backoff = false, time.Second
		d.log.Info("listening for the wake word on the server", "keywords", kws)
		d.setWake(st, wakeOn)
		d.send(&models.Message{Type: models.TypeWakeStream, WakeStream: &models.WakeStream{On: true}})

		d.spot(ctx, st, kws)

		d.setWake(nil, wakeOff)
		st.Close()
		if ctx.Err() == nil {
			d.send(&models.Message{Type: models.TypeWakeStream, WakeStream: &models.WakeStream{On: false}})
		}
	}
}

// spot handles detections until the stream fails, ctx ends or the keywords
// are cleared.
func (d *deviceSession) spot(ctx context.Context, st *wake.Stream, kws []string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.Done():
			if ctx.Err() == nil {
				d.log.Warn("wake word spotter stream ended", "err", st.Err())
			}
			return
		case <-d.wakeKick:
			next := d.keywords()
			if len(next) == 0 {
				return
			}
			if !slices.Equal(next, kws) {
				if err := st.Configure(next, d.hello.InputFormat.SampleRateHertz); err != nil {
					return
				}
				kws = next
				d.log.Info("wake keywords changed", "keywords", kws)
			}
		case s := <-st.Status():
			if len(s.Unsupported) > 0 {
				msg := "распознаватель не знает слов: " + strings.Join(s.Unsupported, ", ")
				d.log.Warn("unsupported wake keywords", "keywords", s.Unsupported)
				d.srv.events.publish(Event{Kind: EventError, Session: d.id, Device: d.hello.DeviceID, Error: msg})
			}
		case det := <-st.Detections():
			d.onWakeWord(det)
		}
	}
}

func (d *deviceSession) onWakeWord(det wake.Detection) {
	d.mu.Lock()
	busy := d.inTurn || d.awaiting || d.ask != nil
	d.mu.Unlock()
	if busy {
		return
	}
	d.log.Info("wake word heard", "keyword", det.Keyword, "engine", det.Engine, "score", det.Score)
	d.srv.events.publish(Event{Kind: EventWake, Session: d.id, Device: d.hello.DeviceID, Name: det.Keyword,
		Result: map[string]any{"engine": det.Engine, "score": det.Score}})
	d.send(&models.Message{Type: models.TypeWake, Wake: &models.Wake{Keyword: det.Keyword}})
}
