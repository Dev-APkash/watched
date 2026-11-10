package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildLogbook(t *testing.T) {
	at := func(y int) time.Time { return time.Date(y, 6, 15, 12, 0, 0, 0, time.UTC) }
	items := []entry{
		{ID: "1", Type: "movie", Title: "Arrival", Year: 2016, LastPlayed: at(2024), Plays: 3, Minutes: 348, Genres: []string{"Drama", "Science Fiction"}},
		{ID: "2", Type: "movie", Title: "Brazil", Year: 1985, LastPlayed: at(2024), Plays: 2, Genres: []string{"drama", "Drama", "Comedy"}},
		{ID: "3", Type: "show", Title: "Dark", Year: 2017, LastPlayed: at(2023), Plays: 12, Played: 10, Minutes: 600, Genres: []string{"Science Fiction"}},
		{ID: "4", Type: "movie", Title: "Undated", Year: 1955, Plays: 1, Minutes: 90},
		{ID: "5", Type: "movie", Title: "Arrival 2", LastPlayed: at(2023), Plays: 3, Minutes: 100},
	}
	lb := buildLogbook(items)
	if lb.Titles != 5 || lb.Movies != 4 || lb.Shows != 1 || lb.Episodes != 10 || lb.Plays != 21 {
		t.Errorf("totals = %d titles, %d films, %d shows, %d eps, %d plays", lb.Titles, lb.Movies, lb.Shows, lb.Episodes, lb.Plays)
	}
	if lb.Minutes != 1138 || lb.Untimed != 1 || lb.Hours() != 19 || lb.Days() != "18 hours" {
		t.Errorf("time = %d min, %d untimed, %d h, %q", lb.Minutes, lb.Untimed, lb.Hours(), lb.Days())
	}
	if got := fmt.Sprint(lb.Years); got != "[{2024 2 0 348} {2023 1 1 700}]" || lb.MaxYear != 2 || lb.Undated != 1 {
		t.Errorf("years = %s max %d undated %d", got, lb.MaxYear, lb.Undated)
	}
	// Genres merge case-insensitively and count each title once.
	if got := fmt.Sprint(lb.Genres); got != "[{Drama 2} {Science Fiction 2} {Comedy 1}]" || lb.MaxGenre != 2 {
		t.Errorf("genres = %s", got)
	}
	if got := fmt.Sprint(lb.Decades); got != "[{1950 1} {1980 1} {2010 2}]" {
		t.Errorf("decades = %s (a title with no year is left out)", got)
	}
	var re []string
	for _, e := range lb.Rewatched {
		re = append(re, e.Title)
	}
	// Ties on plays go to the most recent watch; shows aren't listed.
	if got := strings.Join(re, ","); got != "Arrival,Arrival 2,Brazil" {
		t.Errorf("rewatched = %s", got)
	}
	if empty := buildLogbook(nil); empty.Titles != 0 || empty.Years != nil || empty.Days() != "0 minutes" {
		t.Errorf("empty logbook = %+v", empty)
	}
}

func TestSpanText(t *testing.T) {
	for min, want := range map[int]string{1: "1 minute", 59: "59 minutes", 60: "1 hour", 1440: "1 day", 1500: "1 day, 1 hour", 4380: "3 days, 1 hour"} {
		if got := spanText(min); got != want {
			t.Errorf("spanText(%d) = %q, want %q", min, got, want)
		}
	}
}

func TestLogbookPage(t *testing.T) {
	a := &app{cfg: config{dataDir: t.TempDir(), user: "alice"}, tr: newTrakt("")}
	get := func() string {
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logbook", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}
	if body := get(); !strings.Contains(body, "Nothing logged yet") {
		t.Error("an empty shelf should say so")
	}
	a.items = []entry{
		{ID: "1", Type: "movie", Title: "<Arrival>", Year: 2016, LastPlayed: time.Now(), Plays: 2, Minutes: 2 * 116, Genres: []string{"Drama"}},
		{ID: "2", Type: "show", Title: "Dark", Year: 2017, LastPlayed: time.Now(), Plays: 1500, Played: 1200},
	}
	body := get()
	for _, want := range []string{"alice’s shelf", "&lt;Arrival&gt;", "2 times", "Drama", "2010s", "1,502", "1 title has none yet", `style="width:100%"`} {
		if !strings.Contains(body, want) {
			t.Errorf("logbook page is missing %q", want)
		}
	}
	if strings.Contains(body, "<Arrival>") {
		t.Error("titles must be escaped")
	}
}
