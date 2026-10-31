package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/yawaflua/marusya-gemini/registry/backend/internal/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pluginProtocolVersion is the Marusya plugin protocol version accepted in
// abi_version and api_version (pluginsdk.ProtocolVersion).
const pluginProtocolVersion = "1"

const manifestFile = "plugin.json"

// pluginIDPattern is the plugin id format of Marusya (internal/plugin).
var pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// pluginManifest is the part of plugin.json the registry checks.
type pluginManifest struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	Server  json.RawMessage `json:"server"`
	Agent   json.RawMessage `json:"agent"`
}

// environment is where the plugin runs, by the sides set in plugin.json.
func (m pluginManifest) environment() models.PluginEnvironment {
	server := len(m.Server) > 0 && string(m.Server) != "null"
	agent := len(m.Agent) > 0 && string(m.Agent) != "null"
	switch {
	case server && agent:
		return models.PluginEnvironmentBoth
	case agent:
		return models.PluginEnvironmentAgent
	case server:
		return models.PluginEnvironmentServer
	}
	return ""
}

// readPluginManifest reads plugin.json of a plugin ZIP archive. The manifest
// lies at the root or in the only top-level directory, the layout Marusya
// installs from.
func readPluginManifest(archive []byte) (pluginManifest, error) {
	var manifest pluginManifest
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return manifest, status.Error(codes.InvalidArgument, "archive must be a ZIP file")
	}
	var (
		found *zip.File
		roots = map[string]struct{}{}
	)
	for _, file := range reader.File {
		name := strings.TrimPrefix(file.Name, "./")
		clean := path.Clean(name)
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") ||
			clean != strings.TrimSuffix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			return manifest, status.Errorf(codes.InvalidArgument, "archive entry %q has an unsafe path", file.Name)
		}
		top, _, _ := strings.Cut(name, "/")
		roots[top] = struct{}{}
		if name == manifestFile || (strings.Count(name, "/") == 1 && path.Base(name) == manifestFile) {
			if found == nil || name == manifestFile {
				found = file
			}
		}
	}
	if found == nil {
		return manifest, status.Error(codes.InvalidArgument, "archive has no plugin.json at its root")
	}
	if strings.Contains(found.Name, "/") && len(roots) != 1 {
		return manifest, status.Error(codes.InvalidArgument, "plugin.json must be at the root or in the only top-level directory")
	}
	opened, err := found.Open()
	if err != nil {
		return manifest, status.Error(codes.InvalidArgument, "plugin.json cannot be read")
	}
	defer opened.Close()
	data, err := io.ReadAll(io.LimitReader(opened, 1<<20))
	if err != nil {
		return manifest, status.Error(codes.InvalidArgument, "plugin.json cannot be read")
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		return manifest, status.Errorf(codes.InvalidArgument, "plugin.json: %v", err)
	}
	if !pluginIDPattern.MatchString(manifest.ID) {
		return manifest, status.Error(codes.InvalidArgument, "plugin.json: id must match ^[a-z0-9][a-z0-9_.-]{0,63}$")
	}
	if manifest.environment() == "" {
		return manifest, status.Error(codes.InvalidArgument, "plugin.json: neither \"server\" nor \"agent\" is set")
	}
	return manifest, nil
}
