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

// Package selfupdate keeps the agent binary the same as the one
// marusya-server serves at /v1/agent/<arch>. The server tags the binary
// with its SHA-256 as the ETag; an agent with the same hash gets 304 Not
// Modified, otherwise it downloads the new binary next to itself and
// renames it over its own file. The caller then restarts the process
// (supervise-daemon respawns it).
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxBinary bounds the download.
const maxBinary = 128 << 20

// Updater replaces the running binary with the server's.
type Updater struct {
	// URL is the binary on the server, see [AgentURL].
	URL   string
	Token string
	// Exe is the running binary; empty uses os.Executable.
	Exe  string
	HTTP *http.Client // nil uses a client with a timeout

	mu   sync.Mutex
	hash string // of Exe, computed once
}

// AgentURL returns the binary's URL for the stream URL of the server
// (ws://host:8080/v1/stream -> http://host:8080/v1/agent/<GOARCH>).
func AgentURL(streamURL string) (string, error) {
	u, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("selfupdate: unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1/stream") + "/v1/agent/" + runtime.GOARCH
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// ETag returns the ETag the server sends for a binary with this hash.
func ETag(sha256hex string) string { return `"` + sha256hex + `"` }

// Check updates the binary if the server has a different one and reports
// whether it did. A server without the binary (404) is not an error.
func (u *Updater) Check(ctx context.Context) (bool, error) {
	exe, hash, err := u.self()
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("If-None-Match", ETag(hash))
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	hc := u.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNotFound:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("selfupdate: %s: %s", u.URL, resp.Status)
	}
	want := strings.Trim(resp.Header.Get("ETag"), `"`)
	if want == hash {
		return false, nil // a server that ignores If-None-Match
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".new-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBinary+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	switch {
	case n == 0 || n > maxBinary:
		return false, fmt.Errorf("selfupdate: bad size %d", n)
	case want != "" && got != want:
		return false, fmt.Errorf("selfupdate: download is corrupt: sha256 %s, want %s", got, want)
	case got == hash:
		return false, nil
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return false, err
	}
	u.mu.Lock()
	u.hash = got
	u.mu.Unlock()
	return true, nil
}

func (u *Updater) self() (exe, hash string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	exe = u.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return "", "", err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return "", "", err
		}
		u.Exe = exe
	}
	if u.hash == "" {
		if u.hash, err = FileHash(exe); err != nil {
			return "", "", err
		}
	}
	return exe, u.hash, nil
}

// FileHash returns the hex SHA-256 of a file.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrUpdated is returned by the agent's main once the binary was replaced.
var ErrUpdated = errors.New("agent binary updated, restarting")
