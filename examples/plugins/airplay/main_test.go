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

package main

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// metadata is what shairport-sync writes to its pipe.
const metadata = `<item><type>73736e63</type><code>736e616d</code><length>6</length>
<data encoding="base64">
0JzQsNGI0LA=</data></item>
<item><type>73736e63</type><code>70626567</code><length>0</length></item>
<item><type>636f7265</type><code>6d696e6d</code><length>12</length>
<data encoding="base64">
0JrRg9GH0LXRgA==</data></item>
<item><type>636f7265</type><code>61736172</code><length>4</length>
<data encoding="base64">
0JrQuNC90L4=</data></item>
<item><type>73736e63</type><code>70666c73</code><length>0</length></item>
<item><type>73736e63</type><code>70656e64</code><length>0</length></item>
`

func TestWatch(t *testing.T) {
	var st Status
	var seen []Status
	err := Watch(strings.NewReader(metadata), func(it Item) {
		if st.apply(it) {
			seen = append(seen, st)
		}
	})
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(seen) != 6 {
		t.Fatalf("changes = %+v", seen)
	}
	if got := seen[3]; !got.Playing || got.Paused || got.Title != "Кучер" || got.Artist != "Кино" || got.Client != "Маша" {
		t.Errorf("playing = %+v", got)
	}
	if !seen[4].Paused || seen[5] != (Status{Connected: true, Client: "Маша"}) {
		t.Errorf("pause and end = %+v %+v", seen[4], seen[5])
	}
	// The phone lets the speaker go.
	if !st.apply(Item{Type: "ssnc", Code: "aend"}) || st != (Status{}) {
		t.Errorf("after aend = %+v", st)
	}
	if !st.apply(Item{Type: "ssnc", Code: "abeg"}) || st != (Status{Connected: true}) {
		t.Errorf("after abeg = %+v", st)
	}
}

func TestVolume(t *testing.T) {
	for data, want := range map[string]int{
		"0.00,0.00,-96.30,0.00":       100,
		"-15.00,-24.06,-96.30,0.00":   50,
		"-30.00,-96.30,-96.30,0.00":   0,
		"-144.00,-144.00,-96.30,0.00": 0,
		"-22.5,-1,-96,0":              25,
	} {
		got, ok := Volume(Item{Type: "ssnc", Code: "pvol", Data: []byte(data)})
		if !ok || got != want {
			t.Errorf("Volume(%q) = %d, %v; want %d", data, got, ok, want)
		}
	}
	if _, ok := Volume(Item{Type: "ssnc", Code: "pvol", Data: []byte("x")}); ok {
		t.Error("bad pvol accepted")
	}
	if _, ok := Volume(Item{Type: "core", Code: "minm", Data: []byte("1")}); ok {
		t.Error("not pvol accepted")
	}
}

func TestAirPlayDBRoundTrip(t *testing.T) {
	for level := 0; level <= 100; level++ {
		data := strconv.FormatFloat(AirPlayDB(level), 'f', 2, 64) + ",0,-96,0"
		if got, ok := Volume(Item{Type: "ssnc", Code: "pvol", Data: []byte(data)}); !ok || got != level {
			t.Errorf("level %d came back as %d", level, got)
		}
	}
}

func TestVolumesNoEcho(t *testing.T) {
	v := NewVolumes()
	if v.speaker(40) {
		t.Error("speaker level sent with no phone")
	}
	v.setConnected(true)
	// The phone drags its slider: the speaker follows and does not answer.
	if !v.phone(30) || v.speaker(30) {
		t.Error("phone level echoed back")
	}
	if v.phone(30) {
		t.Error("same phone level set twice")
	}
	// A voice command: the phone follows, and its echo is not set again.
	if !v.speaker(60) || v.phone(60) {
		t.Error("speaker level not sent or echoed")
	}
	// A new phone sets its own level first.
	v.setConnected(false)
	v.setConnected(true)
	if !v.phone(60) {
		t.Error("new phone's level ignored")
	}
}
