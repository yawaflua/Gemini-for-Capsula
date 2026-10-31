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

// Package buildinfo names the build of marusya-server and marusya-agent.
// The server and the agents it serves are built from the same commit, so
// equal versions mean the speaker runs the agent the server hands out.
package buildinfo

import "runtime/debug"

// Version returns v, the version set with -ldflags "-X main.version=...",
// or, for a build without it ("dev" or empty), the commit Go stamped into
// the binary in the same form as git describe --always --dirty: a short
// hash with a "-dirty" suffix for uncommitted changes.
func Version(v string) string {
	if v != "" && v != "dev" {
		return v
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) < 7 {
		return "dev"
	}
	return rev[:7] + dirty
}

// Known reports whether v names a build, so two versions can be compared.
func Known(v string) bool { return v != "" && v != "dev" }
