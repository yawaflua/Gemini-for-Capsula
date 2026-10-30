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

package pipeline

import "testing"

func TestSentenceEnd(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"Да. Нет", -1}, // too short
		{"Сегодня в Ашдоде солнечно. Завтра", len("Сегодня в Ашдоде солнечно.")},
		{"Привет\nмир", len("Привет\n")},
		{"Сегодня в Ашдоде солнечно.", -1}, // may continue
	} {
		if got := sentenceEnd(tc.in); got != tc.want {
			t.Errorf("sentenceEnd(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
