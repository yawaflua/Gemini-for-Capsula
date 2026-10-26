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

// Package spotify implements the spotify_play skill: it finds music in the
// Spotify catalogue and has the speaker open it in spotifyd.
//
// The server only searches, with the client credentials of one Spotify app
// and no user account, so any number of speakers can use the same app.
// Playback runs on the account spotifyd on the speaker is signed in to
// (once, by picking the speaker in the Spotify app); it needs Premium.
package spotify

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
	"github.com/yawaflua/marusya-gemini/internal/tools"
)

// Spotify endpoints.
const (
	TokenURL = "https://accounts.spotify.com/api/token"
	APIURL   = "https://api.spotify.com/v1"
)

// ErrNotFound means Spotify does not have the music asked for.
var ErrNotFound = errors.New("not on Spotify")

// Kinds are the things that can be played.
var Kinds = []string{"track", "album", "artist", "playlist"}

// Client searches the Spotify catalogue.
type Client struct {
	// ClientID and ClientSecret are of an app at developer.spotify.com.
	ClientID     string
	ClientSecret string
	// Market is an ISO 3166 country code: only music playable there is
	// found. Empty searches every market.
	Market string

	// API and Token override the endpoints in tests.
	API   string
	Token string

	once sync.Once
	hc   *http.Client
}

// Skills returns spotify_play.
func (c *Client) Skills() skills.Set { return skills.Set{playSkill{c}} }

func (c *Client) http() *http.Client {
	c.once.Do(func() {
		conf := &clientcredentials.Config{
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			TokenURL:     cmp.Or(c.Token, TokenURL),
			AuthStyle:    oauth2.AuthStyleInHeader,
		}
		// The token source outlives any request: refresh with a context of its own.
		c.hc = conf.Client(context.Background())
	})
	return c.hc
}

// Item is a search result.
type Item struct {
	Name    string `json:"name"`
	URI     string `json:"uri"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Owner struct {
		DisplayName string `json:"display_name"`
	} `json:"owner"`
}

// By returns the artists, or the owner of a playlist.
func (it *Item) By() string {
	var names []string
	for _, a := range it.Artists {
		names = append(names, a.Name)
	}
	return cmp.Or(strings.Join(names, ", "), it.Owner.DisplayName)
}

// Search returns the best match of kind (one of [Kinds], track if empty)
// for q.
func (c *Client) Search(ctx context.Context, q, kind string) (*Item, error) {
	q = strings.TrimSpace(q)
	kind = cmp.Or(kind, "track")
	if q == "" {
		return nil, errors.New("empty query")
	}
	if !slices.Contains(Kinds, kind) {
		return nil, fmt.Errorf("unknown type %q", kind)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Spotify ranks by popularity too, so a few candidates are needed to
	// find the one that matches; playlists may also come back as nulls.
	query := url.Values{"q": {q}, "type": {kind}, "limit": {"10"}}
	if c.Market != "" {
		query.Set("market", c.Market)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(cmp.Or(c.API, APIURL), "/")+"/search?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("spotify: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("spotify: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var res map[string]struct {
		Items []*Item `json:"items"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("spotify: %w", err)
	}
	var best, first *Item
	bestScore := 0
	for _, it := range res[kind+"s"].Items {
		if it == nil || it.URI == "" {
			continue
		}
		if first == nil {
			first = it
		}
		if sc := score(q, it); sc > bestScore {
			best, bestScore = it, sc
		}
	}
	switch {
	case best != nil:
		return best, nil
	case first != nil:
		// Spotify always returns something; a result sharing no word with
		// the query is a different song, not the one asked for.
		return nil, fmt.Errorf("%q is %w (the closest result, %q by %s, is something else)", q, ErrNotFound, first.Name, first.By())
	}
	return nil, fmt.Errorf("%q is %w", q, ErrNotFound)
}

// score counts the words of the query found in the title and artists of it.
func score(q string, it *Item) int {
	text := normalize(it.Name + " " + it.By())
	joined := strings.ReplaceAll(text, " ", "")
	n := 0
	for _, w := range strings.Fields(normalize(q)) {
		if len([]rune(w)) >= 2 && (strings.Contains(text, w) || strings.Contains(joined, w)) {
			n++
		}
	}
	return n
}

// normalize lowercases s and keeps only letters and digits, separated by
// single spaces.
func normalize(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}

type playSkill struct{ c *Client }

func (s playSkill) Tool() tools.Tool {
	return tools.Tool{
		Action: models.Action("spotify.play"),
		Description: "Plays music on the speaker from Spotify: a song, an album, an artist or a public playlist. " +
			"This is the default for music: use it whenever the user asks to play or put on a song, an artist, an album or a playlist " +
			"(\"включи Кино — Группа крови\", \"поставь Nirvana\", \"включи в спотифае альбом Nevermind\"), " +
			"unless they ask for YouTube, a video, a podcast or the radio: those go to media_play_search or media_play_url. " +
			"If Spotify does not have it, the speaker plays it from YouTube by itself; the result says so in \"source\", tell the user. " +
			"Pause, resume and skip it with media_control.",
		Parameters: []tools.Parameter{
			{
				Name: "query", Type: tools.TypeString, Required: true,
				Description: "Artist and title in the language they are written in, e.g. \"Кино Группа крови\" or \"Nirvana Nevermind\". Include the artist whenever the user named one. " +
					"Keep the user's words as they are, never correct an unfamiliar title into a familiar word.",
			},
			{
				Name: "type", Type: tools.TypeString, Enum: Kinds,
				Description: "track (default), album, artist (plays their music) or playlist.",
			},
		},
	}
}

func (s playSkill) Run(ctx context.Context, dev skills.Device, args map[string]any) (map[string]any, error) {
	if dev.Command == nil {
		return nil, errors.New("no speaker to play on")
	}
	q := skills.String(args, "query")
	it, err := s.c.Search(ctx, q, skills.String(args, "type"))
	if errors.Is(err, ErrNotFound) {
		return s.youtube(ctx, dev, q, err)
	}
	if err != nil {
		return nil, err
	}
	if _, err := dev.Command(ctx, models.ActionMediaPlayURL, map[string]any{"url": it.URI}); err != nil {
		return s.youtube(ctx, dev, strings.TrimSpace(it.By()+" "+it.Name),
			fmt.Errorf("the speaker could not open it in spotifyd (is spotifyd running and signed in?): %w", err))
	}
	out := map[string]any{"source": "spotify", "playing": it.Name, "uri": it.URI}
	if by := it.By(); by != "" {
		out["by"] = by
	}
	return out, nil
}

// youtube plays q from YouTube instead, because of why.
func (s playSkill) youtube(ctx context.Context, dev skills.Device, q string, why error) (map[string]any, error) {
	if _, err := dev.Command(ctx, models.ActionMediaPlaySearch, map[string]any{"query": q}); err != nil {
		return nil, fmt.Errorf("%w; YouTube failed too: %w", why, err)
	}
	return map[string]any{"source": "youtube", "query": q, "why_not_spotify": why.Error()}, nil
}
