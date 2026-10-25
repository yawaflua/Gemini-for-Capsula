// Copyright 2026 The Marusya-Gemini Authors.
// SPDX-License-Identifier: Apache-2.0

package wake

import (
	"slices"
	"testing"
)

func TestParseKeywords(t *testing.T) {
	got, err := ParseKeywords(" Антон ,  אנטון,антон, hey  jarvis ,, ")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"антон", "אנטון", "hey jarvis"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"a", "антон!", "123"} {
		if _, err := ParseKeywords(bad); err == nil {
			t.Errorf("ParseKeywords(%q) accepted", bad)
		}
	}
	if got, err := ParseKeywords(""); err != nil || len(got) != 0 {
		t.Errorf("empty = %q, %v", got, err)
	}
}
