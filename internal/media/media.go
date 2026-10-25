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

// Package media finds music and videos (YouTube and anything else yt-dlp
// understands) and delivers their sound as raw PCM. The server decodes the
// audio (see media/ytdlp) and streams it to speakers over HTTP, so speakers
// need no decoder of their own; [Client] is the speaker's end of that stream.
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yawaflua/marusya-gemini/internal/models"
)

// Path is the HTTP endpoint of the media stream on the server.
const Path = "/v1/media"

// Format is the format of streamed audio.
var Format = models.AudioFormat{Encoding: models.EncodingLinear16, SampleRateHertz: 48000, Channels: 1}

// Request selects what to play: a page or stream URL, or a search query.
type Request struct {
	URL   string
	Query string
}

// Validate reports whether r names exactly one thing to play. Only http(s)
// URLs are accepted: anything else could make the decoder read local files.
func (r Request) Validate() error {
	switch {
	case (r.URL == "") == (r.Query == ""):
		return errors.New("media: need exactly one of url or query")
	case r.Query != "":
		if len(r.Query) > 300 {
			return errors.New("media: query is too long")
		}
		return nil
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("media: not an http(s) URL: %q", r.URL)
	}
	return nil
}

func (r Request) String() string {
	if r.Query != "" {
		return "search " + strconv.Quote(r.Query)
	}
	return r.URL
}

// Track is an opened stream of [Format] audio.
type Track struct {
	Title string
	// Duration is zero for live streams and when unknown.
	Duration time.Duration
	// Audio is the PCM stream. Closing it stops decoding.
	Audio io.ReadCloser
}

// Source opens tracks.
type Source interface {
	Open(ctx context.Context, r Request) (*Track, error)
}

// ErrNotFound is returned when a search finds nothing.
var ErrNotFound = errors.New("media: nothing found")

// Response headers carrying the track metadata.
const (
	HeaderTitle    = "X-Media-Title" // URL-encoded
	HeaderDuration = "X-Media-Duration"
	HeaderFormat   = "X-Media-Format"
)

// ContentType is the MIME type of [Format] streams.
var ContentType = fmt.Sprintf("audio/L16;rate=%d;channels=%d", Format.SampleRateHertz, Format.Channels)

var formatTag = fmt.Sprintf("s16le;rate=%d;channels=%d", Format.SampleRateHertz, Format.Channels)

// Serve writes t to w, flushing regularly so playback starts at once.
func Serve(w http.ResponseWriter, t *Track) error {
	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Set(HeaderTitle, url.QueryEscape(t.Title))
	h.Set(HeaderFormat, formatTag)
	if t.Duration > 0 {
		h.Set(HeaderDuration, strconv.Itoa(int(t.Duration.Seconds())))
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	buf := make([]byte, Format.FrameSize(100*time.Millisecond))
	for {
		n, err := io.ReadFull(t.Audio, buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			rc.Flush()
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Client opens tracks served by marusya-server.
type Client struct {
	// URL is the media endpoint, e.g. http://192.168.1.10:8080/v1/media.
	URL string
	// Token is the bearer token of the speaker.
	Token string
	// HTTP defaults to a client without a total timeout (streams are long).
	HTTP *http.Client
}

var _ Source = (*Client)(nil)

// EndpointFor derives the media endpoint from the streaming URL of the
// server, e.g. ws://host:8080/v1/stream -> http://host:8080/v1/media.
func EndpointFor(streamURL string) (string, error) {
	u, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "http"
	case "wss", "https":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("media: unsupported server URL %q", streamURL)
	}
	u.Path, u.RawQuery, u.Fragment = Path, "", ""
	return u.String(), nil
}

// Open implements [Source].
func (c *Client) Open(ctx context.Context, r Request) (*Track, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{}
	if r.URL != "" {
		q.Set("url", r.URL)
	} else {
		q.Set("q", r.Query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		text := strings.TrimSpace(string(msg))
		if resp.StatusCode == http.StatusNotFound && text != "" {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, text)
		}
		return nil, fmt.Errorf("media server: %s: %s", resp.Status, text)
	}
	if f := resp.Header.Get(HeaderFormat); f != "" && f != formatTag {
		resp.Body.Close()
		return nil, fmt.Errorf("media server: unsupported format %q", f)
	}
	t := &Track{Audio: resp.Body}
	t.Title, _ = url.QueryUnescape(resp.Header.Get(HeaderTitle))
	if s, err := strconv.Atoi(resp.Header.Get(HeaderDuration)); err == nil {
		t.Duration = time.Duration(s) * time.Second
	}
	return t, nil
}
