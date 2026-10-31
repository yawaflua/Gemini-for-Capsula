// Copyright 2026 The Marusya-Gemini Authors.
// SPDX-License-Identifier: Apache-2.0

// Package agentdeploy holds the files installed on a speaker: the OpenRC
// service that downloads the agent from marusya-server on every start, and
// spotifyd for the spotify_play skill.
package agentdeploy

import _ "embed"

// OpenRC is /etc/init.d/marusya-agent.
//
//go:embed marusya-agent.openrc
var OpenRC string

// SpotifydOpenRC is /etc/init.d/spotifyd.
//
//go:embed spotifyd.openrc
var SpotifydOpenRC string

// SpotifydConf is the default /etc/spotifyd.conf.
//
//go:embed spotifyd.conf
var SpotifydConf string

// SpotifydDBus is /etc/dbus-1/system.d/spotifyd.conf.
//
//go:embed spotifyd-dbus.conf
var SpotifydDBus string

// AgentDBus is /etc/dbus-1/system.d/ru.marusya.Agent.conf, the system bus
// policy of the agent's own D-Bus service.
//
//go:embed ru.marusya.Agent.conf
var AgentDBus string
