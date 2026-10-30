// Copyright 2026 The Marusya-Gemini Authors.
// SPDX-License-Identifier: Apache-2.0

// Package agentdeploy holds the files installed on a speaker: the OpenRC
// service that downloads the agent from marusya-server on every start.
package agentdeploy

import _ "embed"

// OpenRC is /etc/init.d/marusya-agent.
//
//go:embed marusya-agent.openrc
var OpenRC string
