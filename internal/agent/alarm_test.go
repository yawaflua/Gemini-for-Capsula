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

package agent_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/alarms"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/provider/echo"
	"github.com/yawaflua/marusya-gemini/internal/server"
	"github.com/yawaflua/marusya-gemini/internal/transport"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
)

type fakeVolume struct {
	mu     sync.Mutex
	level  int
	levels []int
}

func (v *fakeVolume) Get(context.Context) (models.VolumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return models.VolumeState{Level: v.level}, nil
}

func (v *fakeVolume) Set(_ context.Context, l int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.level = l
	v.levels = append(v.levels, l)
	return nil
}

func (v *fakeVolume) SetMuted(context.Context, bool) error { return nil }

func (v *fakeVolume) history() []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.levels)
}

func TestAgentRingsAlarm(t *testing.T) {
	log := slog.New(slog.NewTextHandler(newTestWriter(t), &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := server.New(server.Config{Provider: echo.Provider{}, DefaultTimeZone: "Asia/Jerusalem",
		AlarmsFile: t.TempDir() + "/alarms.json", Logger: log})
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	at := time.Now().In(loc).Add(1500 * time.Millisecond).Round(time.Second)
	if _, err := s.Alarms().Add("test", alarms.Alarm{Time: at.Format("15:04:05"), Date: at.Format(time.DateOnly), Volume: 90, Label: "работа"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()

	player := &fakePlayer{done: make(chan struct{})}
	vol := &fakeVolume{level: 40}
	wake := make(fakeWake, 1)
	ind := &fakeIndicator{}
	a := agent.New(agent.Config{
		DeviceID:  "test",
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path,
	}, log, agent.Deps{Capture: &fakeMic{}, Player: player, Detector: wake, Dispatcher: &controller.Dispatcher{Volume: vol}, Indicator: ind})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	select {
	case <-player.done: // the first beep pattern played
	case <-ctx.Done():
		t.Fatal("the alarm did not ring")
	}
	if late := time.Since(at); late > time.Second || late < -100*time.Millisecond {
		t.Errorf("rang %v after the alarm time", late)
	}
	if got := vol.history(); !slices.Equal(got, []int{90}) {
		t.Errorf("volume while ringing = %v, want [90]", got)
	}
	// A tap silences it, restores the volume and does not start a request.
	wake <- wakeword.Detection{Source: "touch"}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(vol.history(), []int{90, 40}) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := vol.history(); !slices.Equal(got, []int{90, 40}) {
		t.Errorf("volume = %v, want restored to 40", got)
	}
	time.Sleep(100 * time.Millisecond)
	if slices.Contains(ind.get(), indicator.Listening) {
		t.Error("the tap that silenced the alarm also started listening")
	}
	// The one-time alarm is gone after it rang.
	time.Sleep(time.Second)
	s.Alarms().Prune("test", time.Now().Add(2*time.Minute), loc)
	if n := len(s.Alarms().List("test")); n != 0 {
		t.Errorf("%d alarms left", n)
	}
}
