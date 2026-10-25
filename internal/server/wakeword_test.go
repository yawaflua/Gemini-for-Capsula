// Copyright 2026 The Marusya-Gemini Authors.
// SPDX-License-Identifier: Apache-2.0

package server

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

	"github.com/coder/websocket"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/transport"
)

// fakeSpotter records configurations and audio and fires on demand.
type fakeSpotter struct {
	configs chan []string
	audio   chan int
	fire    chan string
}

func (f *fakeSpotter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx := r.Context()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case kw := <-f.fire:
				b, _ := json.Marshal(map[string]any{"keyword": kw, "engine": "fake"})
				ws.Write(ctx, websocket.MessageText, b)
			}
		}
	}()
	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			select {
			case f.audio <- len(b):
			default:
			}
			continue
		}
		var c struct {
			Keywords []string `json:"keywords"`
		}
		json.Unmarshal(b, &c)
		f.configs <- c.Keywords
		ws.Write(ctx, websocket.MessageText, []byte(`{"ready":true,"unsupported":[]}`))
	}
}

func TestServerWakeWord(t *testing.T) {
	spot := &fakeSpotter{configs: make(chan []string, 4), audio: make(chan int, 64), fire: make(chan string, 1)}
	sp := httptest.NewServer(spot)
	defer sp.Close()
	srv := New(Config{
		Provider: &toolProvider{}, Logger: slog.New(slog.DiscardHandler), Language: "ru-RU",
		WakeURL: "ws" + strings.TrimPrefix(sp.URL, "http"), WakeWords: "Антон, אנטון",
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "d", InputFormat: models.DeviceInputFormat,
		Features: []models.Feature{models.FeatureServerWake},
	}})
	next := func(typ models.MessageType) *models.Message {
		t.Helper()
		for {
			f, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("waiting for %s: %v", typ, err)
			}
			if f.Message != nil && f.Message.Type == typ {
				return f.Message
			}
		}
	}
	next(models.TypeReady)

	if kws := <-spot.configs; !slices.Equal(kws, []string{"антон", "אנטון"}) {
		t.Errorf("spotter keywords = %q", kws)
	}
	if m := next(models.TypeWakeStream); !m.WakeStream.On {
		t.Fatal("wake_stream is off")
	}
	conn.SendAudio(ctx, make([]byte, 640)) // idle audio goes to the spotter
	select {
	case n := <-spot.audio:
		if n != 640 {
			t.Errorf("spotter got %d bytes", n)
		}
	case <-ctx.Done():
		t.Fatal("no audio reached the spotter")
	}

	spot.fire <- "антон"
	if m := next(models.TypeWake); m.Wake.Keyword != "антон" {
		t.Errorf("wake keyword = %q", m.Wake.Keyword)
	}
	if ds := srv.Devices(); len(ds) != 1 || ds[0].Wake != wakeOn {
		t.Errorf("devices = %+v", ds)
	}

	// New keywords are sent to the spotter without reconnecting.
	set := srv.Settings()
	set.WakeWords = "маруся"
	if _, err := srv.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	if kws := <-spot.configs; !slices.Equal(kws, []string{"маруся"}) {
		t.Errorf("reconfigured keywords = %q", kws)
	}

	// No keywords: streaming stops.
	set.WakeWords = ""
	if _, err := srv.UpdateSettings(set); err != nil {
		t.Fatal(err)
	}
	if m := next(models.TypeWakeStream); m.WakeStream.On {
		t.Error("wake_stream still on without keywords")
	}
}

func TestServerWakeNotForOldAgents(t *testing.T) {
	srv := New(Config{Provider: &toolProvider{}, Logger: slog.New(slog.DiscardHandler),
		WakeURL: "ws://127.0.0.1:1", WakeWords: "антон"})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+transport.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close("done")
	conn.Send(ctx, &models.Message{Type: models.TypeHello, Hello: &models.Hello{
		ProtocolVersion: models.ProtocolVersion, DeviceID: "old", InputFormat: models.DeviceInputFormat,
	}})
	conn.Read(ctx) // ready
	time.Sleep(50 * time.Millisecond)
	if ds := srv.Devices(); len(ds) != 1 || ds[0].Wake != "" {
		t.Errorf("devices = %+v", ds)
	}
	// A timed out read closes the connection, so this comes last.
	rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer rcancel()
	if f, err := conn.Read(rctx); err == nil && f.Message != nil {
		t.Errorf("an old agent got %s", f.Message.Type)
	}
}
