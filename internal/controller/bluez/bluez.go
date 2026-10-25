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

// Package bluez implements [controller.Bluetooth] on top of BlueZ's
// bluetoothctl, optionally delegating pairing to a vendor tool such as
// bt-pair.
package bluez

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/controller"
	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/syscmd"
)

// TimeoutPlaceholder in [Bluetooth.PairCommand] is replaced with the pairing
// timeout in seconds.
const TimeoutPlaceholder = "{timeout}"

// DefaultPairCommand is the speaker firmware's tool that enables pairing and
// lets phones use the device as a Bluetooth (A2DP) speaker.
const DefaultPairCommand = "bt-pair"

// Bluetooth controls the BlueZ stack.
type Bluetooth struct {
	// PairCommand, if set, replaces the built-in pairing flow, e.g.
	// []string{"bt-pair"} or []string{"bt-pair", "{timeout}"}. The tool owns
	// the pairing lifecycle: it is never killed by the agent, because it may
	// keep the speaker in Bluetooth speaker mode for as long as it runs.
	PairCommand []string
	// Runner executes bluetoothctl. Defaults to [syscmd.Exec].
	Runner syscmd.Runner
	Logger *slog.Logger

	mu          sync.Mutex
	stopPairing context.CancelFunc // built-in flow
	external    bool               // PairCommand is running
}

var _ controller.Bluetooth = (*Bluetooth)(nil)

func (b *Bluetooth) ctl(ctx context.Context, args ...string) (string, error) {
	r := b.Runner
	if r == nil {
		r = syscmd.Exec{}
	}
	return r.Run(ctx, "bluetoothctl", args...)
}

func (b *Bluetooth) log() *slog.Logger {
	if b.Logger == nil {
		return slog.Default()
	}
	return b.Logger
}

// Pair implements [controller.Bluetooth].
func (b *Bluetooth) Pair(ctx context.Context, timeout time.Duration) error {
	if len(b.PairCommand) > 0 {
		return b.pairExternal(timeout)
	}

	b.mu.Lock()
	if b.stopPairing != nil {
		b.stopPairing()
	}
	pctx, cancel := context.WithTimeout(context.Background(), timeout)
	b.stopPairing = cancel
	b.mu.Unlock()

	if _, err := b.ctl(ctx, "power", "on"); err != nil {
		cancel()
		return err
	}
	return b.pairBuiltin(pctx, cancel, timeout)
}

// pairExternal starts the vendor pairing tool unless it is already running.
// Errors that happen right after start are reported synchronously; after
// that the tool runs detached until it exits on its own.
func (b *Bluetooth) pairExternal(timeout time.Duration) error {
	b.mu.Lock()
	if b.external {
		b.mu.Unlock()
		b.log().Info("bluetooth pairing tool already running")
		return nil
	}
	b.external = true
	b.mu.Unlock()

	args := make([]string, len(b.PairCommand))
	for i, a := range b.PairCommand {
		args[i] = strings.ReplaceAll(a, TimeoutPlaceholder, strconv.Itoa(int(timeout.Seconds())))
	}
	cmd := exec.Command(args[0], args[1:]...)
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		b.mu.Lock()
		b.external = false
		b.mu.Unlock()
		return fmt.Errorf("bluez: start %s: %w", args[0], err)
	}
	b.log().Info("bluetooth pairing tool started", "cmd", args)

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		b.mu.Lock()
		b.external = false
		b.mu.Unlock()
		b.log().Info("bluetooth pairing tool exited", "err", err)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("bluez: %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
		}
	case <-time.After(2 * time.Second):
	}
	return nil
}

// pairBuiltin drives an interactive bluetoothctl session that registers a
// NoInputNoOutput agent, makes the adapter discoverable and auto-accepts
// incoming pairing and service authorization requests until ctx expires.
func (b *Bluetooth) pairBuiltin(ctx context.Context, cancel context.CancelFunc, timeout time.Duration) error {
	cmd := exec.Command("bluetoothctl")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("bluez: start bluetoothctl: %w", err)
	}

	var wmu sync.Mutex
	send := func(lines ...string) {
		wmu.Lock()
		defer wmu.Unlock()
		for _, l := range lines {
			io.WriteString(stdin, l+"\n")
		}
	}
	send(
		"agent NoInputNoOutput",
		"default-agent",
		"pairable on",
		fmt.Sprintf("discoverable-timeout %d", int(timeout.Seconds())),
		"discoverable on",
	)

	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.Contains(line, "(yes/no)"):
				send("yes")
			case strings.Contains(line, "Paired: yes"):
				b.log().Info("bluetooth device paired", "line", stripANSI(line))
				send("trust " + lastAddress(line))
			}
		}
	}()

	go func() {
		<-ctx.Done()
		send("discoverable off", "pairable off", "quit")
		stdin.Close()
		wait := make(chan struct{})
		go func() { cmd.Wait(); close(wait) }()
		select {
		case <-wait:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-wait
		}
		b.log().Info("bluetooth pairing mode finished")
	}()
	b.log().Info("bluetooth pairing mode on", "timeout", timeout)
	return nil
}

var (
	ansiRE    = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x01|\x02`)
	addressRE = regexp.MustCompile(`([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}`)
	deviceRE  = regexp.MustCompile(`^Device\s+((?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2})\s+(.*)$`)
)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func lastAddress(s string) string {
	all := addressRE.FindAllString(s, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

func parseDevices(out string) []models.BluetoothDevice {
	var devs []models.BluetoothDevice
	for _, line := range strings.Split(out, "\n") {
		if m := deviceRE.FindStringSubmatch(strings.TrimSpace(stripANSI(line))); m != nil {
			devs = append(devs, models.BluetoothDevice{Address: strings.ToUpper(m[1]), Name: m[2]})
		}
	}
	return devs
}

// Devices implements [controller.Bluetooth].
func (b *Bluetooth) Devices(ctx context.Context) ([]models.BluetoothDevice, error) {
	out, err := b.ctl(ctx, "devices", "Paired")
	if err != nil {
		// Older BlueZ versions do not support filters.
		if out, err = b.ctl(ctx, "devices"); err != nil {
			return nil, err
		}
	}
	devs := parseDevices(out)
	connected := map[string]bool{}
	if out, err := b.ctl(ctx, "devices", "Connected"); err == nil {
		for _, d := range parseDevices(out) {
			connected[d.Address] = true
		}
	} else {
		for _, d := range devs {
			info, err := b.ctl(ctx, "info", d.Address)
			connected[d.Address] = err == nil && strings.Contains(info, "Connected: yes")
		}
	}
	for i := range devs {
		devs[i].Connected = connected[devs[i].Address]
	}
	return devs, nil
}

// resolve finds a device by address or (partial, case-insensitive) name.
func (b *Bluetooth) resolve(ctx context.Context, query string) (models.BluetoothDevice, error) {
	devs, err := b.Devices(ctx)
	if err != nil {
		return models.BluetoothDevice{}, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	for _, d := range devs {
		if strings.ToLower(d.Address) == q || strings.ToLower(d.Name) == q {
			return d, nil
		}
	}
	for _, d := range devs {
		if strings.Contains(strings.ToLower(d.Name), q) {
			return d, nil
		}
	}
	return models.BluetoothDevice{}, fmt.Errorf("bluetooth device %q not found among paired devices", query)
}

// Connect implements [controller.Bluetooth].
func (b *Bluetooth) Connect(ctx context.Context, device string) error {
	d, err := b.resolve(ctx, device)
	if err != nil {
		return err
	}
	out, err := b.ctl(ctx, "connect", d.Address)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Connection successful") && !strings.Contains(out, "Connected: yes") {
		return fmt.Errorf("connect %s: %s", d, stripANSI(out))
	}
	return nil
}

// Disconnect implements [controller.Bluetooth].
func (b *Bluetooth) Disconnect(ctx context.Context, device string) error {
	if device != "" {
		d, err := b.resolve(ctx, device)
		if err != nil {
			return err
		}
		_, err = b.ctl(ctx, "disconnect", d.Address)
		return err
	}
	devs, err := b.Devices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.Connected {
			if _, err := b.ctl(ctx, "disconnect", d.Address); err != nil {
				return err
			}
		}
	}
	return nil
}
