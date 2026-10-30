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

package dbusapi

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/surface"
)

type fakeRing struct{ req chan models.RingRequest }

func (r *fakeRing) Override(req models.RingRequest) (time.Time, error) {
	r.req <- req
	return time.Now().Add(req.Hold), nil
}
func (r *fakeRing) ResetOverride() {}

// sessionBus connects to the session bus or skips the test.
func sessionBus(t *testing.T) *dbus.Conn {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skip("no D-Bus session bus:", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestService(t *testing.T) {
	server, client := sessionBus(t), sessionBus(t)
	ring := &fakeRing{req: make(chan models.RingRequest, 1)}
	surf := &surface.Surface{Ring: ring}
	name := fmt.Sprintf("ru.marusya.Test.p%d", os.Getpid())
	svc := &Service{Surface: surf, Name: name, Version: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- svc.Serve(ctx, server) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Error("Serve:", err)
		}
	}()
	// Wait for the name.
	for i := 0; ; i++ {
		var owner string
		if err := client.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner); err == nil {
			break
		}
		if i == 100 {
			t.Fatal("the service did not take its name")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := client.AddMatchSignal(dbus.WithMatchObjectPath(Path)); err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 16)
	client.Signal(signals)
	obj := client.Object(name, Path)

	if err := obj.Call(IfaceRing+".Play", 0, "spin", "#00FF00", int32(40), uint32(30)).Err; err != nil {
		t.Fatal("Play:", err)
	}
	if req := <-ring.req; req.Scene != "spin" || req.Color != "00ff00" || req.Brightness != 40 || req.Hold != 30*time.Second {
		t.Errorf("ring request = %+v", req)
	}
	if err := obj.Call(IfaceRing+".Play", 0, "disco", "", int32(0), uint32(0)).Err; err == nil {
		t.Error("an unknown scene must fail")
	} else if e, ok := err.(dbus.Error); !ok || e.Name != ErrorFailed {
		t.Errorf("error = %#v, want %s", err, ErrorFailed)
	}
	if err := obj.Call(IfaceDisplay+".ShowText", 0, "HI", uint32(0)).Err; err == nil {
		t.Error("ShowText without a display must fail")
	} else if e, ok := err.(dbus.Error); !ok || e.Name != ErrorUnsupported {
		t.Errorf("error = %#v, want %s", err, ErrorUnsupported)
	}

	// PropertiesChanged for the ring override, then a Key signal.
	waitSignal := func(member string, match func(*dbus.Signal) bool) {
		t.Helper()
		timeout := time.After(3 * time.Second)
		for {
			select {
			case sig := <-signals:

				if sig.Name == member && match(sig) {
					return
				}
			case <-timeout:
				t.Fatalf("no %s signal", member)
			}
		}
	}
	waitSignal("org.freedesktop.DBus.Properties.PropertiesChanged", func(sig *dbus.Signal) bool {
		if sig.Body[0] != IfaceRing {
			return false
		}
		o, _ := sig.Body[1].(map[string]dbus.Variant)["Override"].Value().(map[string]dbus.Variant)
		return o["scene"].Value() == "spin"
	})
	surf.KeyEvent("mic", 248, true)
	waitSignal(IfaceSensors+".Key", func(sig *dbus.Signal) bool {
		return len(sig.Body) == 3 && sig.Body[0] == "mic" && sig.Body[1] == uint16(248) && sig.Body[2] == true
	})

	v, err := obj.GetProperty(IfaceSensors + ".LastKey")
	if err != nil || v.Value() != "mic" {
		t.Errorf("LastKey = %v, %v", v, err)
	}
	v, err = obj.GetProperty(IfaceRing + ".Scenes")
	if scenes, _ := v.Value().([]string); err != nil || len(scenes) != len(models.RingScenes) {
		t.Errorf("Scenes = %v, %v", v, err)
	}
	var xml string
	if err := obj.Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil || len(xml) < 100 {
		t.Errorf("Introspect: %v", err)
	}

	if err := obj.Call(IfaceRing+".Reset", 0).Err; err != nil {
		t.Error("Reset:", err)
	}
	waitSignal("org.freedesktop.DBus.Properties.PropertiesChanged", func(sig *dbus.Signal) bool {
		o, ok := sig.Body[1].(map[string]dbus.Variant)["Override"].Value().(map[string]dbus.Variant)
		return sig.Body[0] == IfaceRing && ok && o["active"].Value() == false && o["scene"].Value() == ""
	})
}
