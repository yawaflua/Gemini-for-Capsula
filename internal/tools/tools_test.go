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

package tools

import (
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

func TestLookupRoundTrip(t *testing.T) {
	for _, tool := range All() {
		got, ok := Lookup(tool.Name())
		if !ok || got.Action != tool.Action {
			t.Errorf("Lookup(%q) = %v, %v; want %v", tool.Name(), got.Action, ok, tool.Action)
		}
	}
}

func TestFor(t *testing.T) {
	got := For([]models.Action{models.ActionVolumeSet, models.ActionBluetoothPair})
	if len(got) != 2 || got[0].Name() != "volume_set" || got[1].Name() != "bluetooth_pair" {
		t.Errorf("For() = %v", got)
	}
	if len(For(nil)) != len(All()) {
		t.Error("For(nil) should return every tool")
	}
}
