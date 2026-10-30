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

package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

type fakePlugins struct{ delivered chan string }

func (fakePlugins) Tools() []models.PluginTool {
	return []models.PluginTool{{Plugin: "lamp", Name: "blink", Description: "Blinks."}}
}

func (fakePlugins) Call(_ context.Context, name string, args map[string]any) (map[string]any, error) {
	return map[string]any{"called": name, "times": args["times"]}, nil
}

func (p fakePlugins) Deliver(plugin string, data json.RawMessage) error {
	p.delivered <- plugin + " " + string(data)
	return nil
}

func TestAgentPlugins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plugins := fakePlugins{delivered: make(chan string, 1)}

	type report struct {
		hello  *models.Hello
		result *models.CommandResult
		sent   *models.PluginMessage
	}
	got := make(chan report, 1)
	var a *agent.Agent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close("bye")
		f, err := conn.Read(ctx)
		if err != nil || f.Message == nil || f.Message.Type != models.TypeHello {
			return
		}
		rep := report{hello: f.Message.Hello}
		conn.Send(ctx, &models.Message{Type: models.TypeReady, Ready: &models.Ready{SessionID: "s", OutputFormat: models.DefaultOutputFormat}})
		conn.Send(ctx, &models.Message{Type: models.TypeCommand, Command: &models.Command{ID: "c1", Action: "blink", Args: map[string]any{"times": float64(3)}}})
		conn.Send(ctx, &models.Message{Type: models.TypePlugin, Plugin: &models.PluginMessage{Plugin: "lamp", Data: json.RawMessage(`{"on":true}`)}})
		for rep.result == nil || rep.sent == nil {
			f, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if f.Message == nil {
				continue
			}
			switch f.Message.Type {
			case models.TypeCommandResult:
				rep.result = f.Message.CommandResult
				a.SendPlugin(ctx, "lamp", json.RawMessage(`"pong"`))
			case models.TypePlugin:
				rep.sent = f.Message.Plugin
			}
		}
		got <- rep
		<-r.Context().Done()
	}))
	defer srv.Close()

	a = agent.New(agent.Config{DeviceID: "test", ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + transport.Path},
		slog.New(slog.DiscardHandler),
		agent.Deps{Capture: &fakeMic{}, Player: &fakePlayer{done: make(chan struct{})}, Detector: make(fakeWake),
			Dispatcher: &controller.Dispatcher{}, Plugins: plugins})
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
	case rep := <-got:
		if !slices.Contains(rep.hello.Features, models.FeaturePlugins) || len(rep.hello.PluginTools) != 1 || rep.hello.PluginTools[0].Name != "blink" {
			t.Errorf("hello = %+v", rep.hello)
		}
		if !rep.result.OK || rep.result.Data["called"] != "blink" || rep.result.Data["times"] != float64(3) {
			t.Errorf("result = %+v", rep.result)
		}
		if rep.sent.Plugin != "lamp" || string(rep.sent.Data) != `"pong"` {
			t.Errorf("sent = %+v", rep.sent)
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
	if d := <-plugins.delivered; d != `lamp {"on":true}` {
		t.Errorf("delivered %q", d)
	}
}
