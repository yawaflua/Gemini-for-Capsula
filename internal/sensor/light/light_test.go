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

package light

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLux(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("name", "bh1730")
	write("in_illuminance_raw", "200")
	write("in_illuminance_scale", "0.5")
	s := Open(dir)
	if got, err := s.Lux(); err != nil || got != 100 {
		t.Errorf("raw*scale: Lux() = %v, %v; want 100", got, err)
	}
	write("in_illuminance_input", "42.5")
	if got, err := s.Lux(); err != nil || got != 42.5 {
		t.Errorf("processed: Lux() = %v, %v; want 42.5", got, err)
	}
	if s.Name() != "bh1730" {
		t.Errorf("Name() = %q", s.Name())
	}
}
