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

package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yawaflua/marusya-gemini/internal/models"
	"github.com/yawaflua/marusya-gemini/internal/skills"
)

func fakeSpotify(t *testing.T, tokens *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			r.ParseForm()
			if id, secret, _ := r.BasicAuth(); id != "id" || secret != "secret" || r.Form.Get("grant_type") != "client_credentials" {
				http.Error(w, `{"error":"invalid_client"}`, 400)
				return
			}
			*tokens++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer at" || r.URL.Path != "/v1/search" {
			http.Error(w, "401", 401)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("type") == "playlist":
			w.Write([]byte(`{"playlists":{"items":[null,{"name":"Chill","uri":"spotify:playlist:p1","owner":{"display_name":"Spotify"}}]}}`))
		case q.Get("q") == "Кино Группа крови" && q.Get("market") == "IL":
			// The most popular result first, as Spotify does.
			w.Write([]byte(`{"tracks":{"items":[{"name":"Кукушка","uri":"spotify:track:t0","artists":[{"name":"Кино"}]},` +
				`{"name":"Группа крови","uri":"spotify:track:t1","artists":[{"name":"Кино"}]}]}}`))
		case q.Get("q") == "шахшахаха":
			w.Write([]byte(`{"tracks":{"items":[{"name":"יאסו","uri":"spotify:track:y1","artists":[{"name":"Static & Ben El"}]}]}}`))
		default:
			w.Write([]byte(`{"tracks":{"items":[]}}`))
		}
	}))
}

type speaker struct {
	opened  []string
	youtube []string
	err     error
}

func (s *speaker) device() skills.Device {
	return skills.Device{Command: func(_ context.Context, a models.Action, args map[string]any) (map[string]any, error) {
		switch a {
		case models.ActionMediaPlaySearch:
			s.youtube = append(s.youtube, args["query"].(string))
			return nil, nil
		case models.ActionMediaPlayURL:
			s.opened = append(s.opened, args["url"].(string))
			return nil, s.err
		}
		return nil, errors.New("unexpected " + string(a))
	}}
}

func TestPlay(t *testing.T) {
	var tokens int
	srv := fakeSpotify(t, &tokens)
	defer srv.Close()
	c := &Client{ClientID: "id", ClientSecret: "secret", Market: "IL", API: srv.URL + "/v1", Token: srv.URL + "/token"}
	play, ok := c.Skills().Lookup("spotify_play")
	if !ok {
		t.Fatal("no spotify_play")
	}
	sp := &speaker{}
	ctx := context.Background()

	res, err := play.Run(ctx, sp.device(), map[string]any{"query": "Кино Группа крови"})
	if err != nil {
		t.Fatal(err)
	}
	if res["playing"] != "Группа крови" || res["by"] != "Кино" {
		t.Errorf("result %v", res)
	}
	if _, err := play.Run(ctx, sp.device(), map[string]any{"query": "chill", "type": "playlist"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sp.opened, " "); got != "spotify:track:t1 spotify:playlist:p1" {
		t.Errorf("opened %s", got)
	}
	if tokens != 1 {
		t.Errorf("token fetched %d times", tokens)
	}

	// Not on Spotify: YouTube instead.
	for _, q := range []string{"nothing", "шахшахаха"} {
		res, err := play.Run(ctx, sp.device(), map[string]any{"query": q})
		if err != nil || res["source"] != "youtube" || !strings.Contains(res["why_not_spotify"].(string), "not on Spotify") {
			t.Errorf("%s: %v %v", q, res, err)
		}
	}
	if _, err := play.Run(ctx, sp.device(), map[string]any{"query": "x", "type": "podcast"}); err == nil {
		t.Error("unknown type must fail")
	}
	// spotifyd down: YouTube with the artist and title found.
	sp.err = errors.New("No players found")
	res, err = play.Run(ctx, sp.device(), map[string]any{"query": "Кино Группа крови"})
	if err != nil || res["source"] != "youtube" || !strings.Contains(res["why_not_spotify"].(string), "spotifyd") {
		t.Errorf("speaker failure: %v %v", res, err)
	}
	if got := strings.Join(sp.youtube, "|"); got != "nothing|шахшахаха|Кино Группа крови" {
		t.Errorf("youtube %s", got)
	}
	if _, err := play.Run(ctx, skills.Device{}, map[string]any{"query": "Кино Группа крови"}); err == nil {
		t.Error("no speaker must fail")
	}
}
