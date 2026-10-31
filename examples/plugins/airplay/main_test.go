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
	if !seen[4].Paused || seen[5] != (Status{}) {
		t.Errorf("pause and end = %+v %+v", seen[4], seen[5])
	}
}
