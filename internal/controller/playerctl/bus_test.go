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

package playerctl

import (
	"os"
	"path/filepath"
	"testing"
)

func proc(t *testing.T, root, pid, comm string, env ...string) {
	t.Helper()
	dir := filepath.Join(root, "proc", pid)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
	var b []byte
	for _, e := range env {
		b = append(append(b, e...), 0)
	}
	os.WriteFile(filepath.Join(dir, "environ"), b, 0o644)
}

func TestFindBusPrefersPlayer(t *testing.T) {
	root := t.TempDir()
	proc(t, root, "10", "sshd", "PATH=/bin", "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/other")
	proc(t, root, "20", "spotifyd", "HOME=/root", "DBUS_SESSION_BUS_ADDRESS=unix:abstract=/tmp/dbus-x")
	if got := FindBus(root, ""); got != "unix:abstract=/tmp/dbus-x" {
		t.Errorf("FindBus = %q", got)
	}
}

func TestFindBusAnyProcess(t *testing.T) {
	root := t.TempDir()
	proc(t, root, "10", "spotifyd", "HOME=/root")
	proc(t, root, "11", "dbus-daemon", "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/s")
	if got := FindBus(root, ""); got != "unix:path=/tmp/s" {
		t.Errorf("FindBus = %q", got)
	}
}

func TestFindBusSystemFallback(t *testing.T) {
	root := t.TempDir()
	proc(t, root, "10", "spotifyd", "HOME=/root")
	os.MkdirAll(filepath.Join(root, "run", "dbus"), 0o755)
	os.WriteFile(filepath.Join(root, "run", "dbus", "system_bus_socket"), nil, 0o644)
	if got := FindBus(root, ""); got != "unix:path=/run/dbus/system_bus_socket" {
		t.Errorf("FindBus = %q", got)
	}
	if got := FindBus(t.TempDir(), ""); got != "" {
		t.Errorf("empty root: FindBus = %q", got)
	}
}
