package main

import (
	"fmt"
	"testing"
)

func TestSeasons(t *testing.T) {
	cases := []struct {
		name string
		e    entry
		want string
	}{
		{"movie", entry{Type: "movie"}, ""},
		{"totals from Jellyfin, unstarted season listed",
			entry{Type: "show", Episodes: []string{"S01E01", "S01E02", "S02E01"}, SeasonTotals: map[int]int{1: 2, 2: 10, 3: 8}},
			"1:2:2,2:1:10,3:0:8"},
		{"Trakt only: no totals",
			entry{Type: "show", Episodes: []string{"S02E05", "S01E01"}},
			"1:1:0,2:1:0"},
		{"specials only when watched; a total never undercounts",
			entry{Type: "show", Episodes: []string{"S00E01", "S01E01", "S01E02", "S01E03"}, SeasonTotals: map[int]int{0: 4, 1: 2}},
			"0:1:4,1:3:3"},
		{"unwatched specials are left out",
			entry{Type: "show", Episodes: []string{"S01E01"}, SeasonTotals: map[int]int{0: 4, 1: 2}},
			"1:1:2"},
		{"unparsable keys are ignored", entry{Type: "show", Episodes: []string{"bogus"}}, ""},
	}
	for _, c := range cases {
		if got := c.e.SeasonsAttr(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMergeSeasonTotals(t *testing.T) {
	ext := map[string]string{"tvdb": "1"}
	jf := []entry{{ID: "a", Type: "show", Ext: ext, Episodes: []string{"S01E01"}, SeasonTotals: map[int]int{1: 5}}}
	tr := []entry{{ID: "t", Type: "show", Ext: ext, Episodes: []string{"S02E01"}}}
	out := merge(jf, tr)
	if got := out[0].SeasonsAttr(); got != "1:1:5,2:1:0" {
		t.Errorf("Trakt episodes should add to Jellyfin's seasons: %s", got)
	}
	// Both have totals (a second Jellyfin-like source): the larger count wins, inputs untouched.
	tr[0].SeasonTotals = map[int]int{1: 7, 2: 3}
	out = merge(jf, tr)
	if got := fmt.Sprint(out[0].SeasonTotals); got != "map[1:7 2:3]" {
		t.Errorf("merged totals = %s", got)
	}
	if jf[0].SeasonTotals[1] != 5 {
		t.Error("merge must not modify its input")
	}
}
