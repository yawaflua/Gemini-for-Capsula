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

package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yawaflua/marusya-gemini/pkg/pluginsdk"
)

// ManifestFile is the name of the manifest in a plugin directory.
const ManifestFile = "plugin.json"

// Manifest describes a plugin:
//
//	{
//	  "id": "weather-station",
//	  "name": "Метеостанция",
//	  "version": "1.0.0",
//	  "description": "...",
//	  "server": {"command": ["./weather-station"]},
//	  "agent":  {"command": ["python3", "agent.py"], "env": {"X": "1"}},
//	  "config": {"city": "Ashdod"}
//	}
//
// A plugin runs on the server, on the speaker or on both, depending on which
// of "server" and "agent" are present.
type Manifest struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	Version     string          `json:"version,omitempty"`
	Description string          `json:"description,omitempty"`
	Server      *Side           `json:"server,omitempty"`
	Agent       *Side           `json:"agent,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`

	// Dir is the directory of the plugin.
	Dir string `json:"-"`
}

// Side is how to start a plugin on one host.
type Side struct {
	// Command is the program and its arguments. A program found in the
	// plugin directory is started from there, otherwise from PATH.
	Command []string `json:"command"`
	// Env adds environment variables.
	Env map[string]string `json:"env,omitempty"`
}

// Side returns the side for host, or nil.
func (m Manifest) Side(host string) *Side {
	if host == pluginsdk.HostAgent {
		return m.Agent
	}
	return m.Server
}

// Sides lists the hosts the plugin runs on.
func (m Manifest) Sides() []string {
	var out []string
	if m.Server != nil {
		out = append(out, pluginsdk.HostServer)
	}
	if m.Agent != nil {
		out = append(out, pluginsdk.HostAgent)
	}
	return out
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	toolRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
)

// Validate checks the manifest.
func (m Manifest) Validate() error {
	if !idRe.MatchString(m.ID) {
		return fmt.Errorf("plugin id %q: use lowercase letters, digits, '.', '_' and '-'", m.ID)
	}
	if m.Server == nil && m.Agent == nil {
		return fmt.Errorf("plugin %s: neither \"server\" nor \"agent\" is set", m.ID)
	}
	for _, s := range []*Side{m.Server, m.Agent} {
		if s != nil && (len(s.Command) == 0 || s.Command[0] == "") {
			return fmt.Errorf("plugin %s: empty command", m.ID)
		}
	}
	if len(m.Config) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(m.Config, &obj); err != nil {
			return fmt.Errorf("plugin %s: config must be a JSON object", m.ID)
		}
	}
	return nil
}

// Load reads the manifest in dir.
func Load(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", filepath.Join(dir, ManifestFile), err)
	}
	m.Dir, _ = filepath.Abs(dir)
	return m, m.Validate()
}

// Discover loads the manifests in the subdirectories of root, sorted by id.
// Broken manifests are returned as errors and skipped.
func Discover(root string) ([]Manifest, []error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var (
		out  []Manifest
		errs []error
		seen = map[string]string{}
	)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue // follows symlinks to directories
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err != nil {
			continue
		}
		m, err := Load(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if other, ok := seen[m.ID]; ok {
			errs = append(errs, fmt.Errorf("plugin %s in %s: already loaded from %s", m.ID, dir, other))
			continue
		}
		seen[m.ID] = dir
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int { return strings.Compare(a.ID, b.ID) })
	return out, errs
}

// ValidTool checks a tool declared by a plugin.
func ValidTool(t pluginsdk.ToolSpec) error {
	if !toolRe.MatchString(t.Name) {
		return fmt.Errorf("tool name %q: use ^[a-z][a-z0-9_]{0,47}$", t.Name)
	}
	if strings.TrimSpace(t.Description) == "" {
		return fmt.Errorf("tool %s: empty description", t.Name)
	}
	for _, p := range t.Params {
		switch p.Type {
		case pluginsdk.TypeString, pluginsdk.TypeInteger, pluginsdk.TypeNumber, pluginsdk.TypeBoolean:
		default:
			return fmt.Errorf("tool %s: parameter %q has type %q", t.Name, p.Name, p.Type)
		}
		if p.Name == "" {
			return fmt.Errorf("tool %s: unnamed parameter", t.Name)
		}
	}
	return nil
}
