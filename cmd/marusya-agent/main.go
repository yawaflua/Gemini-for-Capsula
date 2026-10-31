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

// Command marusya-agent runs on the speaker. It waits for the wake word,
// streams the request to marusya-server, plays the reply and executes
// volume, media and Bluetooth commands.
//
// Usage:
//
//	marusya-agent -server ws://192.168.1.10:8080/v1/stream -token secret \
//	    -wake_command "/opt/wakeword/detect --model marusya.onnx"
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yawaflua/marusya-gemini/internal/agent"
	"github.com/yawaflua/marusya-gemini/internal/audio/alsa"
	"github.com/yawaflua/marusya-gemini/internal/config"
	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/controller/airplay"
	"github.com/yawaflua/marusya-gemini/internal/controller/amixer"
	"github.com/yawaflua/marusya-gemini/internal/controller/bluez"
	"github.com/yawaflua/marusya-gemini/internal/controller/capsulaipc"
	"github.com/yawaflua/marusya-gemini/internal/controller/mpv"
	"github.com/yawaflua/marusya-gemini/internal/controller/playerctl"
	"github.com/yawaflua/marusya-gemini/internal/controller/stream"
	"github.com/yawaflua/marusya-gemini/internal/dbusapi"
	"github.com/yawaflua/marusya-gemini/internal/display/seg7"
	"github.com/yawaflua/marusya-gemini/internal/evdev"
	"github.com/yawaflua/marusya-gemini/internal/indicator"
	"github.com/yawaflua/marusya-gemini/internal/indicator/face"
	"github.com/yawaflua/marusya-gemini/internal/indicator/ringled"
	"github.com/yawaflua/marusya-gemini/internal/logging"
	"github.com/yawaflua/marusya-gemini/internal/media"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/selfupdate"
	"github.com/yawaflua/marusya-gemini/internal/sensor/light"
	"github.com/yawaflua/marusya-gemini/internal/surface"
	"github.com/yawaflua/marusya-gemini/internal/touchkeys"
	"github.com/yawaflua/marusya-gemini/internal/vad"
	"github.com/yawaflua/marusya-gemini/internal/wakeword"
	wwbutton "github.com/yawaflua/marusya-gemini/internal/wakeword/button"
	wwexec "github.com/yawaflua/marusya-gemini/internal/wakeword/exec"
	wwsignal "github.com/yawaflua/marusya-gemini/internal/wakeword/signal"
	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

var (
	serverURL = flag.String("server", config.Env("MARUSYA_SERVER", "ws://127.0.0.1:8080/v1/stream"), "Server streaming endpoint (ws:// or wss://).")
	token     = flag.String("token", config.Env("MARUSYA_AUTH_TOKEN", ""), "Bearer token for the server.")
	deviceID  = flag.String("device_id", config.Env("MARUSYA_DEVICE_ID", ""), "Device identifier. Defaults to the hostname.")
	language  = flag.String("language", config.Env("MARUSYA_LANGUAGE", "ru-RU"), "Speech language (BCP-47).")
	location  = flag.String("location", config.Env("MARUSYA_LOCATION", ""), `Where the speaker is, e.g. "Ашдод" (time and weather). Empty uses the server default.`)
	timezone  = flag.String("timezone", config.Env("MARUSYA_TIMEZONE", ""), `IANA time zone of the speaker, e.g. "Asia/Jerusalem". Empty uses the server default.`)

	captureDevice  = flag.String("capture_device", config.Env("MARUSYA_CAPTURE_DEVICE", "default"), "ALSA capture PCM.")
	playbackDevice = flag.String("playback_device", config.Env("MARUSYA_PLAYBACK_DEVICE", "default"), "ALSA playback PCM.")

	wakeCommand  = flag.String("wake_command", config.Env("MARUSYA_WAKE_COMMAND", ""), "Keyword spotter command: reads 16 kHz mono S16LE PCM on stdin, prints a line per detection.")
	wakeCooldown = flag.Duration("wake_cooldown", 1500*time.Millisecond, "Ignore repeated keyword detections within this period.")
	buttonDevice = flag.String("button_device", config.Env("MARUSYA_BUTTON_DEVICE", ""), "evdev node of the activation button, e.g. /dev/input/event0.")
	buttonCode   = flag.Uint("button_code", 0, "Linux key code of the activation button (0 = any key).")

	mixerCard    = flag.String("mixer_card", config.Env("MARUSYA_MIXER_CARD", ""), "ALSA card for volume control.")
	mixerControl = flag.String("mixer_control", config.Env("MARUSYA_MIXER_CONTROL", "Master"), "ALSA simple mixer control for volume. Empty disables volume control.")
	capsuleIPC   = flag.Bool("capsule_ipc", config.EnvBool("MARUSYA_CAPSULE_IPC", false), "Control volume through the Capsula capsule-ipc tool (keeps the device panel, ALSA and AirPlay in sync).")

	bluetooth     = flag.Bool("bluetooth", config.EnvBool("MARUSYA_BLUETOOTH", true), "Enable Bluetooth control via bluetoothctl.")
	btPairCommand = flag.String("bt_pair_command", config.Env("MARUSYA_BT_PAIR_COMMAND", "auto"), `Pairing tool, e.g. "bt-pair" or "bt-pair {timeout}". "auto" uses bt-pair if it is in PATH; empty uses the built-in bluetoothctl flow.`)

	mediaBackend = flag.String("media", config.Env("MARUSYA_MEDIA", "auto"), `Media player backend: "mpv", "playerctl" (MPRIS, e.g. spotifyd), "auto" (mpv if installed, else playerctl) or empty to disable.`)
	mpvSocket    = flag.String("mpv_socket", config.Env("MARUSYA_MPV_SOCKET", "/tmp/mpv.sock"), "mpv JSON IPC socket.")
	playerName   = flag.String("playerctl_player", config.Env("MARUSYA_PLAYERCTL_PLAYER", ""), "Restrict playerctl to one player, e.g. spotifyd.")
	mpvAutoStart = flag.Bool("mpv_autostart", true, "Start mpv --idle if the socket is not reachable.")

	earcons       = flag.Bool("earcons", true, "Play short sounds when listening starts and on errors.")
	followUp      = flag.Bool("follow_up", true, "Listen for a follow-up question after each reply without the wake word.")
	serverStreams = flag.Bool("server_streams", config.EnvBool("MARUSYA_SERVER_STREAMS", true), "Play YouTube and other http(s) streams decoded by the server (it needs yt-dlp and ffmpeg).")
	duckMedia     = flag.Bool("duck_media", true, "Pause music while the assistant is active.")
	autoUpdate    = flag.Bool("auto_update", config.EnvBool("MARUSYA_AUTO_UPDATE", true), "On every connection, compare the agent binary with the server's (/v1/agent/<arch>); if it differs, download it over this one and restart when idle (supervise-daemon starts the new one).")
	pluginsDir    = flag.String("plugins_dir", config.Env("MARUSYA_PLUGINS_DIR", ""), "Directory of plugins that run on the speaker, one subdirectory with a plugin.json each (see pkg/pluginsdk). Empty disables plugins.")
	airplayOn     = flag.Bool("airplay", config.EnvBool("MARUSYA_AIRPLAY", true), "Control AirPlay music played to the speaker (shairport-sync on the system D-Bus): pause, next track, status and ducking go to the phone while it plays.")

	vadThreshold = flag.Float64("vad_threshold_db", vad.DefaultConfig().ThresholdDB, "Speech threshold above the noise floor, dB.")
	vadSilence   = flag.Duration("vad_silence", vad.DefaultConfig().SilenceEnd, "Trailing silence that ends an utterance.")

	displayMode = flag.String("display", config.Env("MARUSYA_DISPLAY", "auto"), `7-segment face display: "auto", "on" or empty to disable.`)
	displayDev  = flag.String("display_dev", "", "line-display sysfs directory with map_seg7; auto-detected if empty.")
	displayRot  = flag.Bool("display_rotate", config.EnvBool("MARUSYA_DISPLAY_ROTATE", false), "Turn the face upside down on panels driven by numbered segment LEDs (clock-led1..30), if it shows upside down.")
	displayNose = flag.String("display_nose", "", `LED used as the nose (name under /sys/class/leds or "none"); auto-detected if empty.`)

	lightSensor = flag.String("light_sensor", config.Env("MARUSYA_LIGHT_SENSOR", "auto"), `Ambient light sensor: "auto", an IIO device directory, or empty to disable. Dims the display and puts the face to sleep in the dark.`)

	dbusService = flag.Bool("dbus", config.EnvBool("MARUSYA_DBUS", true), "Publish the LED ring, display and sensors on the system D-Bus as "+dbusapi.Name+" (needs deploy/agent/ru.marusya.Agent.conf).")

	touchKeys      = flag.String("touch_keys", config.Env("MARUSYA_TOUCH_KEYS", "auto"), `Touch panel: "auto" (find "Hynitron CSK05T"), an evdev node, or empty to disable.`)
	touchCenterKey = flag.Uint("touch_center_key", touchkeys.KeyVoice, "Key code of the centre key that activates the assistant.")

	ringMode          = flag.String("ring", config.Env("MARUSYA_RING", "auto"), `LED ring: "auto" (on a VK Capsula Mini, by its device tree), "on" or empty to disable.`)
	ringDev           = flag.String("ring_dev", config.Env("MARUSYA_RING_DEV", ringled.DefaultDevice), "spidev node of the LED ring.")
	ringledBrightness = flag.Int("ringled_brightness", 30, "LED ring brightness, per cent.")
	ringDemo          = flag.Bool("ring_demo", false, "Play every LED ring animation once and exit.")

	logLevel = flag.String("log_level", config.Env("MARUSYA_LOG_LEVEL", "info"), "Log level: debug, info, warn, error.")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "marusya-agent:", err)
		os.Exit(1)
	}
}

// openLightSensor opens the ambient light sensor from -light_sensor. The
// surface polls it, dims the face and puts it to sleep in the dark.
func openLightSensor() (*light.Sensor, error) {
	var s *light.Sensor
	if *lightSensor == "auto" {
		var err error
		if s, err = light.Find(); err != nil {
			return nil, err
		}
	} else {
		s = light.Open(*lightSensor)
	}
	if _, err := s.Lux(); err != nil {
		return nil, err
	}
	return s, nil
}

// serveDBus publishes the surface on the system bus until ctx is done.
// The service is optional: without a bus or a policy the agent goes on.
func serveDBus(ctx context.Context, s *surface.Surface, log *slog.Logger) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Info("D-Bus service disabled: no system bus", "err", err)
		return
	}
	defer conn.Close()
	svc := &dbusapi.Service{Surface: s, Version: version, Logger: log}
	if err := svc.Serve(ctx, conn); err != nil {
		log.Info("D-Bus service disabled; is /etc/dbus-1/system.d/ru.marusya.Agent.conf installed?", "err", err)
	}
}

// newMedia picks the media controller according to -media.
func newMedia(log *slog.Logger) controller.Media {
	mode := *mediaBackend
	if mode == "auto" {
		_, errBin := exec.LookPath("mpv")
		_, errSock := os.Stat(*mpvSocket)
		_, errCtl := exec.LookPath("playerctl")
		switch {
		case errBin == nil || errSock == nil:
			mode = "mpv"
		case errCtl == nil:
			mode = "playerctl"
		default:
			mode = ""
		}
	}
	switch mode {
	case "mpv":
		_, errBin := exec.LookPath("mpv")
		log.Info("media control", "backend", "mpv", "socket", *mpvSocket)
		return &mpv.Player{Socket: *mpvSocket, AutoStart: *mpvAutoStart && errBin == nil}
	case "playerctl":
		log.Info("media control", "backend", "playerctl", "player", cmp.Or(*playerName, "any"))
		return &playerctl.Player{Name: *playerName}
	}
	log.Info("media control disabled")
	return nil
}

func run() error {
	log, err := logging.New(*logLevel, false)
	if err != nil {
		return err
	}
	// Warnings and errors are also sent to the server.
	fwd := logging.NewForwarder(log.Handler(), slog.LevelWarn, 256)
	log = slog.New(fwd)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, restart := context.WithCancel(ctx)
	defer restart()
	var updated atomic.Bool

	id := *deviceID
	if id == "" {
		if id, err = os.Hostname(); err != nil {
			id = "marusya"
		}
	}

	// Device controllers. Leave interfaces nil to disable an area.
	dispatcher := &controller.Dispatcher{}
	if *capsuleIPC {
		dispatcher.Volume = &capsulaipc.Volume{}
	} else if *mixerControl != "" {
		dispatcher.Volume = &amixer.Volume{Card: *mixerCard, Control: *mixerControl}
	}
	if *bluetooth {
		pair := *btPairCommand
		if pair == "auto" {
			pair = ""
			if _, err := exec.LookPath(bluez.DefaultPairCommand); err == nil {
				pair = bluez.DefaultPairCommand
			}
		}
		log.Info("bluetooth pairing", "command", cmp.Or(pair, "bluetoothctl (built-in)"))
		dispatcher.Bluetooth = &bluez.Bluetooth{PairCommand: strings.Fields(pair), Logger: log}
	}
	dispatcher.Media = newMedia(log)

	// State indicators: LED ring and the face on the 7-segment display.
	var (
		indicators indicator.Multi
		display    *face.Face
		ring       *ringled.Ring
	)
	if strip, desc := ringStrip(log); strip != nil {
		ring = ringled.New(strip, *ringledBrightness, log)
		defer ring.Close()
		indicators = append(indicators, ring)
		dispatcher.Light = ring // "включи ночник"
		log.Info("LED ring enabled", "driver", desc)
		if *ringDemo {
			playRingDemo(ring)
			return nil
		}
	} else if *ringDemo {
		return errors.New("no LED ring")
	}
	if *displayMode != "" {
		panel, err := seg7.Open(*displayDev, *displayNose)
		switch {
		case err == nil:
			if *displayRot {
				panel.Rotate180()
			}
			display = face.New(panel, log)
			indicators = append(indicators, display)
			log.Info("face display enabled", "device", panel.Dir(), "dimmable", panel.Dimmable(), "layout_calibrated", panel.Calibrated())
		case *displayMode == "auto":
			log.Info("no 7-segment display found", "err", err)
		default:
			return err
		}
	}

	// The surface lets the panel, D-Bus and the voice model take the ring
	// and the display over for a while, and reports the sensors.
	surf := &surface.Surface{Logger: log}
	if ring != nil {
		surf.Ring = ring
		dispatcher.Ring = surf
	}
	if display != nil {
		surf.Face = display
		dispatcher.Display = surf
	}
	if *lightSensor != "" {
		if s, err := openLightSensor(); err == nil {
			surf.Light = s
			log.Info("ambient light sensor enabled", "sensor", s.Name(), "dir", s.Dir())
		} else {
			log.Info("ambient light sensor disabled", "err", err)
		}
	}
	if surf.Ring != nil || surf.Face != nil || surf.Light != nil {
		dispatcher.Sensors = surf
	}
	indicators = append(indicators, surf)
	go surf.Run(ctx)
	if *dbusService {
		go serveDBus(ctx, surf, log)
	}

	// Show every volume change on the display and the ring: the hardware keys did it
	// already, now voice commands and the web panel do too.
	if dispatcher.Volume != nil && (display != nil || ring != nil) {
		dispatcher.Volume = &controller.ObservedVolume{Volume: dispatcher.Volume, OnChange: func(s models.VolumeState) {
			if display != nil {
				text := strconv.Itoa(s.Level)
				if s.Muted {
					text = "OFF"
				}
				display.Overlay(text, 1500*time.Millisecond)
			}
			if ring != nil {
				ring.ShowVolume(s.Level, s.Muted)
			}
		}}
	}

	// Activation: SIGUSR1 (always, for testing), touch panel, wake word, button.
	detectors := wakeword.Multi{wwsignal.Detector{}}
	var touch *touchkeys.Panel
	if *touchKeys != "" {
		cfg := touchkeys.DefaultConfig()
		cfg.CenterKey = uint16(*touchCenterKey)
		if *touchKeys != "auto" {
			cfg.Device = *touchKeys
		}
		if _, err := evdev.FindByName(cfg.Name); cfg.Device != "" || err == nil {
			touch = &touchkeys.Panel{Config: cfg, Dispatcher: dispatcher, Logger: log, OnKey: surf.KeyEvent}
			if display != nil {
				touch.Display = display
			}
			detectors = append(detectors, touch)
		} else {
			log.Info("touch panel not found", "err", err)
		}
	}
	if *wakeCommand != "" {
		detectors = append(detectors, &wwexec.Detector{
			Command:  strings.Fields(*wakeCommand),
			Cooldown: *wakeCooldown,
			Logger:   log,
		})
	}
	if *buttonDevice != "" {
		detectors = append(detectors, &wwbutton.Detector{Device: *buttonDevice, KeyCode: uint16(*buttonCode)})
	}
	if len(detectors) == 1 {
		log.Warn("no wake word, touch panel or button configured; only SIGUSR1 will activate the assistant")
	}

	alsaCfg := alsa.Config{CaptureDevice: *captureDevice, PlaybackDevice: *playbackDevice, Logger: log}
	player := alsa.NewPlayer(alsaCfg)
	// YouTube and other streams decoded by the server; Spotify URIs and
	// track skipping stay with the media player chosen above.
	var streams *stream.Player
	if *serverStreams {
		if ep, err := media.EndpointFor(*serverURL); err != nil {
			log.Warn("server streams disabled", "err", err)
		} else {
			streams = &stream.Player{Source: &media.Client{URL: ep, Token: *token}, Out: player, Next: dispatcher.Media, Logger: log}
			dispatcher.Media = streams
			log.Info("server streams (YouTube, radio)", "endpoint", ep)
		}
	}
	if *airplayOn {
		// Outermost: while a phone plays to the speaker, it owns the music.
		dispatcher.Media = airplay.Wrap(dispatcher.Media, log)
	}
	vadCfg := vad.DefaultConfig()
	vadCfg.ThresholdDB = *vadThreshold
	vadCfg.SilenceEnd = *vadSilence

	var plugins *speakerPlugins
	if *pluginsDir != "" {
		plugins = newSpeakerPlugins(*pluginsDir, pluginsdk.Device{ID: id, Language: *language, Location: *location, TimeZone: *timezone}, dispatcher, log)
		indicators = append(indicators, plugins)
	}
	deps := agent.Deps{
		Capture:    alsa.NewCapture(alsaCfg, models.DeviceInputFormat),
		Player:     player,
		Detector:   detectors,
		Dispatcher: dispatcher,
		Indicator:  indicators,
		Logs:       fwd.Records(),
	}
	if plugins != nil {
		deps.Plugins = plugins
	}
	var a *agent.Agent
	var onConnected func(context.Context)
	if *autoUpdate {
		onConnected = autoUpdater(log, func() bool { return a.Busy() }, func() {
			updated.Store(true)
			restart()
		})
	}
	a = agent.New(agent.Config{
		DeviceID:  id,
		ServerURL: *serverURL,
		Token:     *token,
		Language:  *language,
		Location:  *location,
		TimeZone:  *timezone,
		VAD:       vadCfg,
		Earcons:   *earcons,
		FollowUp:  *followUp,
		DuckMedia: *duckMedia,

		PluginsDir: *pluginsDir,

		OnConnected: onConnected,
	}, log, deps)
	if plugins != nil {
		plugins.start(ctx, a)
		log.Info("plugins", "dir", *pluginsDir, "tools", len(plugins.Tools()))
	}
	if streams != nil {
		streams.Hold = a.Busy
		defer streams.Stop()
	}
	if touch != nil {
		touch.OnMicMute = func(muted bool) {
			a.SetMicMuted(muted)
			surf.SetMicMuted(muted)
			if display != nil {
				display.SetMuted(muted)
			}
			if ring != nil {
				ring.SetMuted(muted)
			}
		}
	}
	if display != nil {
		defer display.Close()
	}
	log.Info("starting agent", "version", version, "device", id, "server", *serverURL, "capabilities", len(dispatcher.Capabilities()))
	if err := a.Run(ctx); err != nil {
		return err
	}
	if updated.Load() {
		// A non-zero status makes sure the supervisor starts the new binary.
		return selfupdate.ErrUpdated
	}
	return nil
}

// autoUpdater returns the agent's OnConnected: it checks the server for a
// new agent binary and, once downloaded, calls restart when the speaker is
// not busy.
func autoUpdater(log *slog.Logger, busy func() bool, restart func()) func(context.Context) {
	agentURL, err := selfupdate.AgentURL(*serverURL)
	if err != nil {
		log.Warn("auto update disabled", "err", err)
		return nil
	}
	up := &selfupdate.Updater{URL: agentURL, Token: *token}
	var running atomic.Bool
	return func(ctx context.Context) {
		if !running.CompareAndSwap(false, true) {
			return
		}
		defer running.Store(false)
		ok, err := up.Check(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("checking for a new agent failed", "err", err)
			}
			return
		}
		if !ok {
			log.Debug("agent is up to date")
			return
		}
		log.Info("new agent downloaded from the server; restarting when idle")
		// Not in the middle of a question; the binary on disk is already
		// the new one, so a restart for any other reason picks it up too.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for busy() {
			<-t.C
		}
		restart()
	}
}

// ringStrip returns the LED ring from the flags; nil disables the ring. The
// VK Capsula Mini's ring is a WS2812 chain on spidev; builds on the stock
// kernel expose it as LED class devices instead.
func ringStrip(log *slog.Logger) (ringled.Strip, string) {
	switch *ringMode {
	case "on", "auto":
	default:
		return nil, ""
	}
	if ringled.Detect() || (*ringMode == "on" && !ringled.LEDsAvailable()) {
		return ringled.NewSPI(*ringDev), "spi " + *ringDev
	}
	if ringled.LEDsAvailable() {
		strip, err := ringled.NewLEDs()
		if err != nil {
			log.Warn("LED ring unavailable", "err", err)
			return nil, ""
		}
		return strip, "leds /sys/class/leds/top-led1..36"
	}
	return nil, ""
}

// playRingDemo shows every look of the ring once (-ring_demo).
func playRingDemo(r *ringled.Ring) {
	step := func(name string, d time.Duration, f func()) {
		fmt.Println(name)
		f()
		time.Sleep(d)
	}
	ctx := context.Background()
	step("listening", 1500*time.Millisecond, func() { r.Show(indicator.Listening) })
	step("thinking", 3*time.Second, func() { r.Show(indicator.Thinking) })
	step("speaking", 3*time.Second, func() { r.Show(indicator.Speaking) })
	step("idle", time.Second, func() { r.Show(indicator.Idle) })
	step("mute", 2*time.Second, func() { r.SetMuted(true) })
	step("unmute", 1500*time.Millisecond, func() { r.SetMuted(false) })
	step("volume 25 → 75", 2500*time.Millisecond, func() {
		for v := 25; v <= 75; v += 10 {
			r.ShowVolume(v, false)
			time.Sleep(150 * time.Millisecond)
		}
	})
	step("error", 3500*time.Millisecond, func() { r.Show(indicator.Error) })
	step("night light", 2*time.Second, func() { r.SetNightLight(ctx, true, "", 0) })
	step("off", 500*time.Millisecond, func() { r.SetNightLight(ctx, false, "", 0) })
}
