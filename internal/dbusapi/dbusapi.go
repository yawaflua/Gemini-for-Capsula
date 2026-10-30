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

// Package dbusapi publishes the speaker's LED ring, display and sensors on
// D-Bus, so that local programs (scripts, plugins, Home Assistant agents)
// can drive them without the server.
//
// Bus name [Name], object [Path], interfaces:
//
//	ru.marusya.Agent          State s, MicMuted b, Version s
//	ru.marusya.Agent.Ring     Play(s scene, s color, i brightness, u seconds)
//	                          SetColor(s color, i brightness, u seconds)
//	                          Reset()
//	                          Scenes as, Override a{sv}
//	ru.marusya.Agent.Display  ShowText(s text, u seconds)
//	                          ShowClock(u seconds)
//	                          SetMode(s mode, s text, i brightness, u seconds)
//	                          Reset()
//	                          Modes as, Override a{sv}, Asleep b
//	ru.marusya.Agent.Sensors  AmbientLight d, HasAmbientLight b, LastKey s
//	                          signal Key(s name, q code, b pressed)
//
// Every property emits org.freedesktop.DBus.Properties.PropertiesChanged.
// Zero seconds use [models.DefaultOverride], and overrides always expire.
// Override has the keys active b, scene s, mode s, text s, color s,
// brightness i and until x (Unix time); active is false when automatic.
package dbusapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/surface"
)

// Names on the bus.
const (
	Name             = "ru.marusya.Agent"
	Path             = dbus.ObjectPath("/ru/marusya/Agent")
	IfaceAgent       = Name
	IfaceRing        = Name + ".Ring"
	IfaceDisplay     = Name + ".Display"
	IfaceSensors     = Name + ".Sensors"
	ErrorFailed      = Name + ".Error.Failed"
	ErrorUnsupported = Name + ".Error.NotSupported"
)

// Service is the published object.
type Service struct {
	Surface *surface.Surface
	// Name is the bus name; default [Name]. Tests use their own.
	Name    string
	Version string
	Logger  *slog.Logger

	conn  *dbus.Conn
	props *prop.Properties
}

// Serve publishes the service on conn and keeps its properties up to
// date until ctx is done. It returns at once if the name is taken.
func (s *Service) Serve(ctx context.Context, conn *dbus.Conn) error {
	if s.Name == "" {
		s.Name = Name
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	s.conn = conn
	// Subscribe before reading the state so that no change is missed.
	events, cancel := s.Surface.Subscribe()
	defer cancel()
	st, _ := s.Surface.Surface(ctx)

	ring, display := &ringObj{s}, &displayObj{s}
	if err := conn.Export(ring, Path, IfaceRing); err != nil {
		return err
	}
	if err := conn.Export(display, Path, IfaceDisplay); err != nil {
		return err
	}
	ro := func(v any) *prop.Prop { return &prop.Prop{Value: v, Emit: prop.EmitTrue} }
	cst := func(v any) *prop.Prop { return &prop.Prop{Value: v, Emit: prop.EmitConst} }
	props, err := prop.Export(conn, Path, prop.Map{
		IfaceAgent: {
			"State":    ro(st.State),
			"MicMuted": ro(st.MicMuted),
			"Version":  cst(s.Version),
		},
		IfaceRing: {
			"Scenes":   cst(models.RingScenes),
			"Override": ro(overrideMap(st.Ring)),
		},
		IfaceDisplay: {
			"Modes":    cst(models.DisplayModes),
			"Override": ro(overrideMap(st.Display)),
			"Asleep":   ro(st.Asleep),
		},
		IfaceSensors: {
			"AmbientLight":    ro(lux(st)),
			"HasAmbientLight": ro(st.Lux != nil),
			"LastKey":         ro(st.Key),
		},
	})
	if err != nil {
		return err
	}
	s.props = props
	node := &introspect.Node{
		Name: string(Path),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: IfaceAgent, Properties: props.Introspection(IfaceAgent)},
			{Name: IfaceRing, Methods: introspect.Methods(ring), Properties: props.Introspection(IfaceRing)},
			{Name: IfaceDisplay, Methods: introspect.Methods(display), Properties: props.Introspection(IfaceDisplay)},
			{Name: IfaceSensors, Properties: props.Introspection(IfaceSensors), Signals: []introspect.Signal{{
				Name: "Key",
				Args: []introspect.Arg{{Name: "name", Type: "s"}, {Name: "code", Type: "q"}, {Name: "pressed", Type: "b"}},
			}}},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), Path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	reply, err := conn.RequestName(s.Name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("dbus: request name %s: %w", s.Name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("dbus: name %s is already taken", s.Name)
	}
	defer conn.ReleaseName(s.Name)
	s.Logger.Info("D-Bus service published", "name", s.Name, "path", Path)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			s.publish(ev)
		}
	}
}

// publish reflects a surface change in the properties and signals.
func (s *Service) publish(ev surface.Event) {
	set := s.props.SetMust
	st := ev.State
	switch ev.Kind {
	case surface.EventState:
		set(IfaceAgent, "State", st.State)
	case surface.EventMic:
		set(IfaceAgent, "MicMuted", st.MicMuted)
	case surface.EventRing:
		set(IfaceRing, "Override", overrideMap(st.Ring))
	case surface.EventDisplay:
		set(IfaceDisplay, "Override", overrideMap(st.Display))
	case surface.EventAsleep:
		set(IfaceDisplay, "Asleep", st.Asleep)
	case surface.EventLux:
		set(IfaceSensors, "AmbientLight", lux(st))
		set(IfaceSensors, "HasAmbientLight", st.Lux != nil)
	case surface.EventKey:
		if ev.Key.Pressed {
			set(IfaceSensors, "LastKey", st.Key)
		}
		if err := s.conn.Emit(Path, IfaceSensors+".Key", ev.Key.Name, ev.Key.Code, ev.Key.Pressed); err != nil {
			s.Logger.Debug("dbus: emit Key", "err", err)
		}
	}
}

func lux(st models.SurfaceState) float64 {
	if st.Lux == nil {
		return 0
	}
	return *st.Lux
}

// overrideMap converts an override for D-Bus. The keys are always the
// same (prop merges maps on update); "active" is false when automatic.
func overrideMap(o *models.Override) map[string]dbus.Variant {
	var (
		v     models.Override
		until int64
	)
	if o != nil {
		v, until = *o, o.Until.Unix()
	}
	return map[string]dbus.Variant{
		"active":     dbus.MakeVariant(o != nil),
		"scene":      dbus.MakeVariant(v.Scene),
		"mode":       dbus.MakeVariant(v.Mode),
		"text":       dbus.MakeVariant(v.Text),
		"color":      dbus.MakeVariant(v.Color),
		"brightness": dbus.MakeVariant(int32(v.Brightness)),
		"until":      dbus.MakeVariant(until),
	}
}

func fail(err error) *dbus.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, surface.ErrNoHardware) {
		return dbus.NewError(ErrorUnsupported, []any{err.Error()})
	}
	return dbus.NewError(ErrorFailed, []any{err.Error()})
}

func hold(seconds uint32) time.Duration { return time.Duration(seconds) * time.Second }

// ringObj implements ru.marusya.Agent.Ring.
type ringObj struct{ s *Service }

// Play shows a scene; color and brightness may be empty and zero.
func (r *ringObj) Play(scene, color string, brightness int32, seconds uint32) *dbus.Error {
	_, err := r.s.Surface.SetRing(context.Background(), models.RingRequest{Scene: scene, Color: color, Brightness: int(brightness), Hold: hold(seconds)})
	return fail(err)
}

// SetColor fills the ring with one colour.
func (r *ringObj) SetColor(color string, brightness int32, seconds uint32) *dbus.Error {
	return r.Play("solid", color, brightness, seconds)
}

// Reset returns the ring to the automatic state indicator.
func (r *ringObj) Reset() *dbus.Error { return fail(r.s.Surface.ResetRing(context.Background())) }

// displayObj implements ru.marusya.Agent.Display.
type displayObj struct{ s *Service }

// SetMode sets any display mode (see [models.DisplayModes]).
func (d *displayObj) SetMode(mode, text string, brightness int32, seconds uint32) *dbus.Error {
	_, err := d.s.Surface.ShowDisplay(context.Background(), models.DisplayRequest{Mode: mode, Text: text, Brightness: int(brightness), Hold: hold(seconds)})
	return fail(err)
}

// ShowText shows up to 4 characters.
func (d *displayObj) ShowText(text string, seconds uint32) *dbus.Error {
	return d.SetMode("text", text, 0, seconds)
}

// ShowClock shows the time.
func (d *displayObj) ShowClock(seconds uint32) *dbus.Error {
	return d.SetMode("clock", "", 0, seconds)
}

// Reset returns the display to the automatic face.
func (d *displayObj) Reset() *dbus.Error { return fail(d.s.Surface.ResetDisplay(context.Background())) }
