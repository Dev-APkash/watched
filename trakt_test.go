package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Trimmed from real Trakt responses. The live API paginates these lists and the
// watched-shows list has no seasons or posters; those come from the history.
const (
	traktMoviesJSON = `[
	 {"plays":2,"last_watched_at":"2024-07-01T18:00:00.000Z","last_updated_at":"2024-07-01T18:00:00.000Z",
	  "movie":{"title":"Arrival","year":2016,"ids":{"trakt":1,"slug":"arrival-2016","imdb":"tt2543164","tmdb":329865},
	   "images":{"poster":["media.trakt.tv/images/movies/000/000/001/posters/medium/aa11.jpg.webp"]}}},
	 {"plays":1,"last_watched_at":"2020-01-01T00:00:00.000Z",
	  "movie":{"title":"Old Film","year":1950,"ids":{"trakt":2,"imdb":"tt0000002","tmdb":2},"images":{"poster":[]}}}]`

	traktShowsJSON = `[
	 {"plays":4,"last_watched_at":"2023-01-05T00:00:00.000Z","reset_at":null,
	  "show":{"title":"Show A","year":2010,"aired_episodes":3,"ids":{"trakt":10,"tvdb":121361,"imdb":"tt0000010","tmdb":10}}},
	 {"plays":3,"last_watched_at":"2022-05-01T00:00:00.000Z","reset_at":"2022-04-01T00:00:00.000Z",
	  "show":{"title":"Rewatching","year":2015,"aired_episodes":10,"ids":{"trakt":11,"tvdb":11}}},
	 {"plays":1,"last_watched_at":"2022-01-01T00:00:00.000Z",
	  "show":{"title":"Only Specials","year":2001,"aired_episodes":5,"ids":{"trakt":12}}}]`

	showA        = `{"title":"Show A","ids":{"trakt":10},"images":{"poster":["media.trakt.tv/images/shows/000/000/010/posters/medium/bb22.jpg.webp"]}}`
	traktHistory = `[
	 {"id":1,"watched_at":"2023-01-05T00:00:00.000Z","type":"episode","episode":{"season":1,"number":3},"show":` + showA + `},
	 {"id":2,"watched_at":"2023-01-03T00:00:00.000Z","type":"episode","episode":{"season":1,"number":1},"show":` + showA + `},
	 {"id":3,"watched_at":"2023-01-02T00:00:00.000Z","type":"episode","episode":{"season":1,"number":1},"show":` + showA + `},
	 {"id":4,"watched_at":"2023-01-01T00:00:00.000Z","type":"episode","episode":{"season":0,"number":1},"show":` + showA + `},
	 {"id":5,"watched_at":"2022-05-01T00:00:00.000Z","type":"episode","episode":{"season":1,"number":1},"show":{"title":"Rewatching","ids":{"trakt":11}}},
	 {"id":6,"watched_at":"2021-01-01T00:00:00.000Z","type":"episode","episode":{"season":1,"number":2},"show":{"title":"Rewatching","ids":{"trakt":11}}},
	 {"id":7,"watched_at":"2022-01-01T00:00:00.000Z","type":"episode","episode":{"season":0,"number":1},"show":{"title":"Only Specials","ids":{"trakt":12}}}]`
)

// fakeTrakt serves the fixtures one item per page, the way Trakt paginates.
func fakeTrakt(t *testing.T) *trakt {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("trakt-api-key") != "cid" || r.Header.Get("trakt-api-version") != "2" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		src := map[string]string{
			"/users/alice/watched/movies":   traktMoviesJSON,
			"/users/alice/watched/shows":    traktShowsJSON,
			"/users/alice/history/episodes": traktHistory,
		}[r.URL.Path]
		switch {
		case r.URL.Path == "/users/secret/watched/movies":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case src == "":
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var all []json.RawMessage
		if err := json.Unmarshal([]byte(src), &all); err != nil {
			t.Fatal(err)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("X-Pagination-Page-Count", strconv.Itoa(len(all)))
		json.NewEncoder(w).Encode(all[page-1 : page])
	}))
	t.Cleanup(srv.Close)
	tr := newTrakt("cid")
	tr.baseURL = srv.URL
	return tr
}

func TestTraktUser(t *testing.T) {
	for in, want := range map[string]string{
		"https://trakt.tv/users/alice":                "alice",
		"https://trakt.tv/users/alice/history?page=2": "alice",
		" trakt.tv/users/Al-ice_9.x/ratings#top ":     "Al-ice_9.x",
		"alice":                                "alice",
		"https://trakt.tv/movies/arrival-2016": "",
		"https://trakt.tv/users/../../oauth":   "",
		"":                                     "",
	} {
		got, err := traktUser(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("traktUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestTraktWatched(t *testing.T) {
	items, err := fakeTrakt(t).watched(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id, title     string
		played, total int
		eps, poster   string
	}
	want := []row{ // most recent first; "Only Specials" has nothing outside season 0
		{"trakt-movie-1", "Arrival", 0, 0, "", "https://media.trakt.tv/images/movies/000/000/001/posters/thumb/aa11.jpg.webp"},
		{"trakt-show-10", "Show A", 2, 3, "S01E01,S01E03", "https://media.trakt.tv/images/shows/000/000/010/posters/thumb/bb22.jpg.webp"},
		{"trakt-show-11", "Rewatching", 1, 10, "S01E01", ""}, // E02 was watched before the reset
		{"trakt-movie-2", "Old Film", 0, 0, "", ""},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		e := items[i]
		if got := (row{e.ID, e.Title, e.Played, e.Total, strings.Join(e.Episodes, ","), e.PosterURL}); got != w {
			t.Errorf("item %d:\n got  %+v\n want %+v", i, got, w)
		}
	}
	if items[0].Poster() != "traktmovie1-aa11jpgwebp.webp" {
		t.Errorf("poster file = %q", items[0].Poster())
	}
}

func TestTraktErrors(t *testing.T) {
	tr := fakeTrakt(t)
	for user, want := range map[string]string{"secret": "private", "nobody": "not found"} {
		if _, err := tr.watched(context.Background(), user); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want error containing %q", user, err, want)
		}
	}
	tr.clientID = "wrong"
	if _, err := tr.watched(context.Background(), "alice"); err == nil || !strings.Contains(err.Error(), "TRAKT_CLIENT_ID") {
		t.Errorf("bad client id: got %v", err)
	}
	if _, err := tr.image(context.Background(), "https://evil.example.com/x.webp"); err == nil {
		t.Error("image() should only fetch from Trakt's CDN")
	}
}

func TestMerge(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	jf := []entry{
		{ID: "m1", Type: "movie", Title: "Arrival", LastPlayed: day(5), PosterTag: "t", Ext: map[string]string{"imdb": "tt2543164"}},
		{ID: "sA", Type: "show", Title: "Show A", LastPlayed: day(3), Played: 2, Total: 2,
			Episodes: []string{"S01E01", "S01E02"}, Ext: map[string]string{"tvdb": "121361"}},
		{ID: "m9", Type: "movie", Title: "Same TMDB number, different kind", LastPlayed: day(1), Ext: map[string]string{"tmdb": "10"}},
	}
	tr := []entry{
		{ID: "trakt-movie-1", Type: "movie", Title: "Arrival", LastPlayed: day(9), PosterURL: "https://x.trakt.tv/a.webp", Ext: map[string]string{"imdb": "tt2543164", "tmdb": "329865"}},
		{ID: "trakt-show-10", Type: "show", Title: "Show A", LastPlayed: day(1), Played: 2, Total: 3,
			Episodes: []string{"S01E01", "S01E03"}, Ext: map[string]string{"tvdb": "121361", "tmdb": "10"}},
		{ID: "trakt-movie-2", Type: "movie", Title: "Old Film", LastPlayed: day(2)},
	}
	out := merge(jf, tr)
	var got []string
	for _, e := range out {
		got = append(got, e.ID)
	}
	if strings.Join(got, ",") != "m1,sA,trakt-movie-2,m9" {
		t.Fatalf("merged order = %v", got)
	}
	arrival, showA := out[0], out[1]
	if !arrival.LastPlayed.Equal(day(9)) || arrival.PosterURL != "" {
		t.Errorf("Arrival should keep Jellyfin's poster and take Trakt's later date: %+v", arrival)
	}
	if showA.Played != 3 || showA.Total != 3 || !showA.Completed() || !showA.LastPlayed.Equal(day(3)) {
		t.Errorf("Show A should combine episodes to 3/3: %+v", showA)
	}
	if len(jf[1].Episodes) != 2 {
		t.Error("merge must not modify its input")
	}
}
