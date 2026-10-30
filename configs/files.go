// Copyright 2026 The Marusya-Gemini Authors.
// SPDX-License-Identifier: Apache-2.0

// Package configs holds the example configuration files.
package configs

import _ "embed"

// AgentConf is the default /etc/conf.d/marusya-agent of a speaker.
//
//go:embed agent.conf.d.example
var AgentConf string
