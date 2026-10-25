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

package bluez

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeCtl map[string]string

func (f fakeCtl) Run(_ context.Context, _ string, args ...string) (string, error) {
	return f[strings.Join(args, " ")], nil
}

func TestDevicesAndResolve(t *testing.T) {
	b := &Bluetooth{Runner: fakeCtl{
		"devices Paired":    "Device 11:22:33:44:55:66 Pixel 9\nDevice aa:bb:cc:dd:ee:ff JBL Flip",
		"devices Connected": "Device AA:BB:CC:DD:EE:FF JBL Flip",
	}}
	ctx := context.Background()
	devs, err := b.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].Connected || !devs[1].Connected || devs[1].Address != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("Devices() = %+v", devs)
	}
	for q, want := range map[string]string{"pixel": "11:22:33:44:55:66", "aa:bb:cc:dd:ee:ff": "AA:BB:CC:DD:EE:FF", "JBL Flip": "AA:BB:CC:DD:EE:FF"} {
		d, err := b.resolve(ctx, q)
		if err != nil || d.Address != want {
			t.Errorf("resolve(%q) = %v, %v; want %s", q, d.Address, err, want)
		}
	}
	if _, err := b.resolve(ctx, "nokia"); err == nil {
		t.Error("resolve(nokia) succeeded, want error")
	}
}

func TestPairExternal(t *testing.T) {
	ctx := context.Background()
	failing := &Bluetooth{PairCommand: []string{"sh", "-c", "echo no adapter; exit 3"}}
	if err := failing.Pair(ctx, time.Minute); err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Errorf("Pair() with failing tool = %v, want error with its output", err)
	}

	running := &Bluetooth{PairCommand: []string{"sleep", "{timeout}"}}
	if err := running.Pair(ctx, 3*time.Second); err != nil {
		t.Fatalf("Pair() = %v", err)
	}
	running.mu.Lock()
	ext := running.external
	running.mu.Unlock()
	if !ext {
		t.Error("tool should still be running after Pair returned")
	}
	if err := running.Pair(ctx, 3*time.Second); err != nil {
		t.Errorf("second Pair() while running = %v, want nil", err)
	}
}
