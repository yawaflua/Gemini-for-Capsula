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

// Package mpv implements [controller.Media] by talking to mpv over its JSON
// IPC socket (https://mpv.io/manual/stable/#json-ipc).
package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Player controls an mpv instance.
type Player struct {
	// Socket is the path passed to mpv's --input-ipc-server.
	Socket string
	// AutoStart launches "mpv --idle" if the socket is not reachable.
	AutoStart bool
	// AudioDevice is passed as --audio-device when auto-starting.
	AudioDevice string

	mu      sync.Mutex
	running bool
}

var _ controller.Media = (*Player)(nil)

type request struct {
	Command   []any `json:"command"`
	RequestID int   `json:"request_id"`
}

type response struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	RequestID int             `json:"request_id"`
	Event     string          `json:"event"`
}

// call executes a single IPC command and returns its data.
func (p *Player) call(ctx context.Context, args ...any) (json.RawMessage, error) {
	conn, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}

	b, _ := json.Marshal(request{Command: args, RequestID: 1})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var r response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Event != "" || r.RequestID != 1 {
			continue // asynchronous events
		}
		if r.Error != "success" {
			return nil, fmt.Errorf("mpv: %v: %s", args, r.Error)
		}
		return r.Data, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("mpv: connection closed")
}

func (p *Player) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", p.Socket)
	if err == nil || !p.AutoStart {
		if err != nil {
			return nil, fmt.Errorf("mpv is not running: %w", err)
		}
		return conn, nil
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	for i := 0; i < 30; i++ {
		if conn, err = d.DialContext(ctx, "unix", p.Socket); err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("mpv did not start: %w", err)
}

func (p *Player) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil // still starting up
	}
	os.Remove(p.Socket)
	args := []string{"--idle=yes", "--no-video", "--no-terminal", "--input-ipc-server=" + p.Socket}
	if p.AudioDevice != "" {
		args = append(args, "--audio-device="+p.AudioDevice)
	}
	cmd := exec.Command("mpv", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	p.running = true
	go func() {
		cmd.Wait()
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()
	return nil
}

// Control implements [controller.Media].
func (p *Player) Control(ctx context.Context, op models.MediaOperation) error {
	var err error
	switch op {
	case models.MediaPlay:
		_, err = p.call(ctx, "set_property", "pause", false)
	case models.MediaPause:
		_, err = p.call(ctx, "set_property", "pause", true)
	case models.MediaToggle:
		_, err = p.call(ctx, "cycle", "pause")
	case models.MediaStop:
		_, err = p.call(ctx, "stop")
	case models.MediaNext:
		_, err = p.call(ctx, "playlist-next", "force")
	case models.MediaPrevious:
		_, err = p.call(ctx, "playlist-prev", "force")
	default:
		err = fmt.Errorf("unsupported operation %q", op)
	}
	return err
}

// PlayURL implements [controller.Media].
func (p *Player) PlayURL(ctx context.Context, url string) error {
	if _, err := p.call(ctx, "loadfile", url, "replace"); err != nil {
		return err
	}
	_, err := p.call(ctx, "set_property", "pause", false)
	return err
}

// Status implements [controller.Media].
func (p *Player) Status(ctx context.Context) (models.MediaStatus, error) {
	var s models.MediaStatus
	idle, err := p.call(ctx, "get_property", "idle-active")
	if err != nil {
		return s, err
	}
	if string(idle) == "true" {
		return s, nil
	}
	if paused, err := p.call(ctx, "get_property", "pause"); err == nil {
		s.Playing = string(paused) == "false"
	}
	if title, err := p.call(ctx, "get_property", "media-title"); err == nil {
		json.Unmarshal(title, &s.Title)
	}
	return s, nil
}
