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

// Package light reads an ambient light sensor through the Linux IIO
// subsystem, e.g. the BH1730 of the VK Capsula Mini.
package light

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Sensor is an IIO illuminance sensor.
type Sensor struct {
	dir string
}

// Find returns the first IIO device that reports illuminance.
func Find() (*Sensor, error) {
	devs, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	for _, d := range devs {
		if s := Open(d); s.supported() {
			return s, nil
		}
	}
	return nil, errors.New("light: no IIO illuminance sensor found")
}

// Open uses the IIO device directory dir.
func Open(dir string) *Sensor { return &Sensor{dir: dir} }

// Dir returns the device directory.
func (s *Sensor) Dir() string { return s.dir }

// Name returns the driver name, e.g. "bh1730".
func (s *Sensor) Name() string { return strings.TrimSpace(s.read("name")) }

func (s *Sensor) read(name string) string {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Sensor) supported() bool {
	return s.read("in_illuminance_input") != "" || s.read("in_illuminance_raw") != ""
}

// Lux returns the current illuminance.
func (s *Sensor) Lux() (float64, error) {
	if v := s.read("in_illuminance_input"); v != "" {
		return strconv.ParseFloat(v, 64)
	}
	raw, err := strconv.ParseFloat(s.read("in_illuminance_raw"), 64)
	if err != nil {
		return 0, errors.New("light: no illuminance reading")
	}
	scale := 1.0
	if v := s.read("in_illuminance_scale"); v != "" {
		if scale, err = strconv.ParseFloat(v, 64); err != nil {
			return 0, err
		}
	}
	return raw * scale, nil
}
