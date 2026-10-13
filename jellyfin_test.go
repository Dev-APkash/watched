package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Responses trimmed from a real Jellyfin server; extra fields are ignored by the client.
const (
	usersJSON = `[{"Name":"Alice","ServerId":"s1","Id":"a1b2c3","HasPassword":true},{"Name":"bob","Id":"d4e5f6"}]`

	moviesJSON = `[
	 {"Name":"Arrival","Id":"m1","Type":"Movie","ProductionYear":2016,"ImageTags":{"Primary":"tag1"},"ProviderIds":{"Imdb":"tt2543164","Tmdb":"329865"},
	  "UserData":{"PlayCount":1,"Played":true,"LastPlayedDate":"2024-03-01T20:00:00.0000000Z","Key":"m1"}},
	 {"Name":"Brazil","Id":"m2","Type":"Movie","ProductionYear":1985,"ImageTags":{},
	  "UserData":{"Played":true,"LastPlayedDate":"2024-05-10T21:30:15.1234567"}},
	 {"Name":"Coherence","Id":"m3","Type":"Movie","ImageTags":{"Primary":"tag3"},
	  "UserData":{"Played":true,"LastPlayedDate":null}}]`

	episodesJSON = `[
	 {"Name":"Pilot","Id":"e1","Type":"Episode","SeriesId":"sA","SeriesName":"Show A","ParentIndexNumber":1,"IndexNumber":1,"UserData":{"Played":true,"LastPlayedDate":"2024-01-01T00:00:00Z"}},
	 {"Name":"Two","Id":"e2","Type":"Episode","SeriesId":"sA","SeriesName":"Show A","ParentIndexNumber":1,"IndexNumber":2,"UserData":{"Played":true,"LastPlayedDate":"2024-06-01T00:00:00Z"}},
	 {"Name":"Opener","Id":"e3","Type":"Episode","SeriesId":"sB","SeriesName":"Show B","UserData":{"Played":true,"LastPlayedDate":"2024-02-01T00:00:00Z"}},
	 {"Name":"Orphan","Id":"e4","Type":"Episode","UserData":{"Played":true}}]`

	seriesJSON = `[
	 {"Name":"Show A","Id":"sA","Type":"Series","ProductionYear":2010,"ImageTags":{"Primary":"tagA"},"ProviderIds":{"Tvdb":"121361"},"UserData":{"UnplayedItemCount":0,"Played":true}},
	 {"Name":"Show B","Id":"sB","Type":"Series","ProductionYear":2020,"ImageTags":{"Primary":"tagB"},"UserData":{"UnplayedItemCount":3,"Played":false}}]`

	viewsJSON = `{"Items":[{"Name":"Movies","Id":"lib1","CollectionType":"movies"},{"Name":"Adult","Id":"lib2"}]}`

	// Played items inside library lib2.
	hiddenJSON = `[{"Name":"Brazil","Id":"m2","Type":"Movie"},{"Name":"Opener","Id":"e3","Type":"Episode","SeriesId":"sB"}]`
)

// fakeJellyfin serves the fixtures, paginating /Items like the real server.
func fakeJellyfin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="good-key"` {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/Users":
			w.Write([]byte(usersJSON))
		case r.URL.Path == "/UserViews":
			w.Write([]byte(viewsJSON))
		case r.URL.Path == "/Items":
			if q.Get("userId") != "a1b2c3" {
				t.Errorf("missing or wrong userId: %q", q.Get("userId"))
			}
			src := map[string]string{"Movie": moviesJSON, "Episode": episodesJSON}[q.Get("includeItemTypes")]
			switch {
			case q.Get("parentId") == "lib2":
				src = hiddenJSON
			case q.Get("parentId") != "":
				src = "[]"
			case q.Get("ids") != "":
				src = seriesJSON
			}
			var all []json.RawMessage
			if err := json.Unmarshal([]byte(src), &all); err != nil {
				t.Fatal(err)
			}
			start, _ := strconv.Atoi(q.Get("startIndex"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			end := min(start+limit, len(all))
			json.NewEncoder(w).Encode(map[string]any{"Items": all[start:end], "TotalRecordCount": len(all), "StartIndex": start})
		case strings.HasSuffix(r.URL.Path, "/Images/Primary"):
			if strings.Contains(r.URL.Path, "m3") {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte("jpeg-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClient(url, key string) *jellyfin {
	jf := newJellyfin(url+"/", key)
	jf.pageSize = 2 // force several pages
	return jf
}

func TestUserID(t *testing.T) {
	srv := fakeJellyfin(t)
	jf := testClient(srv.URL, "good-key")

	id, err := jf.userID(context.Background(), "ALICE")
	if err != nil || id != "a1b2c3" {
		t.Fatalf("userID(ALICE) = %q, %v; want a1b2c3", id, err)
	}
	_, err = jf.userID(context.Background(), "carol")
	if msg, transient := explain(err, config{}); transient || !strings.Contains(msg, "Alice, bob") {
		t.Errorf("unknown user: got %q (transient=%v)", msg, transient)
	}
}

func TestBadKeyIsFatal(t *testing.T) {
	srv := fakeJellyfin(t)
	_, err := testClient(srv.URL, "wrong").userID(context.Background(), "alice")
	if msg, transient := explain(err, config{}); transient || !strings.Contains(msg, "JELLYFIN_API_KEY") {
		t.Errorf("bad key: got %q (transient=%v)", msg, transient)
	}
}

func TestUnreachableIsTransient(t *testing.T) {
	srv := fakeJellyfin(t)
	srv.Close()
	_, err := testClient(srv.URL, "good-key").userID(context.Background(), "alice")
	if _, transient := explain(err, config{}); !transient {
		t.Errorf("connection refused should be transient, got %v", err)
	}
}

func TestFetchWatched(t *testing.T) {
	srv := fakeJellyfin(t)
	items, err := fetchWatched(context.Background(), testClient(srv.URL, "good-key"), "a1b2c3", nil)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id, typ, title string
		year           int
		played, total  int
		last           string
		tag            string
	}
	want := []row{ // most recently watched first
		{"sA", "show", "Show A", 2010, 2, 2, "2024-06-01T00:00:00Z", "tagA"},
		{"m2", "movie", "Brazil", 1985, 0, 0, "2024-05-10T21:30:15Z", ""},
		{"m1", "movie", "Arrival", 2016, 0, 0, "2024-03-01T20:00:00Z", "tag1"},
		{"sB", "show", "Show B", 2020, 1, 4, "2024-02-01T00:00:00Z", "tagB"},
		{"m3", "movie", "Coherence", 0, 0, 0, "0001-01-01T00:00:00Z", "tag3"},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		e := items[i]
		got := row{e.ID, e.Type, e.Title, e.Year, e.Played, e.Total, e.LastPlayed.Format(time.RFC3339), e.PosterTag}
		if got != w {
			t.Errorf("item %d:\n got  %+v\n want %+v", i, got, w)
		}
	}
	if !items[0].Completed() || items[3].Completed() {
		t.Error("Show A should be completed and Show B not")
	}
	if got := strings.Join(items[0].Episodes, ","); got != "S01E01,S01E02" {
		t.Errorf("Show A episodes = %s", got)
	}
	if items[2].Ext["imdb"] != "tt2543164" || items[0].Ext["tvdb"] != "121361" {
		t.Errorf("provider ids not parsed: %v %v", items[2].Ext, items[0].Ext)
	}
}

func TestJfTime(t *testing.T) {
	for in, want := range map[string]string{
		`"2024-05-10T21:30:15.1234567Z"`:      "2024-05-10T21:30:15Z",
		`"2024-05-10T21:30:15.1234567"`:       "2024-05-10T21:30:15Z",
		`"2024-05-10T23:30:15.1234567+02:00"`: "2024-05-10T21:30:15Z",
		`null`:                                "0001-01-01T00:00:00Z",
	} {
		var v jfTime
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("%s: %v", in, err)
		} else if got := v.Format(time.RFC3339); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestSyncPosters(t *testing.T) {
	srv := fakeJellyfin(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gone-old.jpg"), []byte("x"), 0o644) // deleted item

	items := []entry{
		{ID: "m1", Title: "Arrival", PosterTag: "tag1"},
		{ID: "m2", Title: "Brazil"},                       // no poster
		{ID: "m3", Title: "Coherence", PosterTag: "tag3"}, // download fails
	}
	jf := testClient(srv.URL, "good-key")
	fetch := func(ctx context.Context, e entry) (io.ReadCloser, error) { return jf.poster(ctx, e.ID, e.PosterTag) }
	if err := syncPosters(context.Background(), fetch, dir, items); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	if strings.Join(names, ",") != "m1-tag1.jpg" {
		t.Errorf("posters dir = %v, want only m1-tag1.jpg", names)
	}
	if items[2].PosterTag != "" {
		t.Error("failed poster should fall back to the placeholder")
	}
}

func TestLoadConfig(t *testing.T) {
	for _, k := range []string{"JELLYFIN_URL", "JELLYFIN_API_KEY", "JELLYFIN_USER", "REFRESH_MINUTES", "PORT"} {
		t.Setenv(k, "")
	}
	if _, problems := loadConfig(); len(problems) != 3 {
		t.Errorf("empty env: got %d problems, want 3: %v", len(problems), problems)
	}

	t.Setenv("JELLYFIN_URL", "http://jellyfin:8096/")
	t.Setenv("JELLYFIN_API_KEY", "k")
	t.Setenv("JELLYFIN_USER", "alice")
	c, problems := loadConfig()
	if len(problems) != 0 || c.url != "http://jellyfin:8096" || c.refresh != 30*time.Minute || c.port != "8080" {
		t.Errorf("defaults: %+v %v", c, problems)
	}

	t.Setenv("EXCLUDE_LIBRARIES", " Adult , ,Kids")
	t.Setenv("TRAKT_CLIENT_ID", "off")
	if c, _ := loadConfig(); strings.Join(c.excludeLibs, "|") != "Adult|Kids" || c.traktClientID != "" {
		t.Errorf("EXCLUDE_LIBRARIES / TRAKT_CLIENT_ID: %q %q", c.excludeLibs, c.traktClientID)
	}

	t.Setenv("JELLYFIN_URL", "jellyfin:8096")
	t.Setenv("REFRESH_MINUTES", "0")
	if _, problems := loadConfig(); len(problems) != 2 {
		t.Errorf("bad values: got %v", problems)
	}
}

func TestExcludeLibraries(t *testing.T) {
	srv := fakeJellyfin(t)
	jf := testClient(srv.URL, "good-key")

	libs, err := jf.libraryIDs(context.Background(), "a1b2c3", []string{"Adult"})
	if err != nil || strings.Join(libs, ",") != "lib2" {
		t.Fatalf("libraryIDs = %v, %v", libs, err)
	}
	items, err := fetchWatched(context.Background(), jf, "a1b2c3", libs)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range items {
		titles = append(titles, e.Title)
	}
	// Brazil and Show B's only watched episode live in the excluded library.
	if got := strings.Join(titles, ","); got != "Show A,Arrival,Coherence" {
		t.Errorf("titles = %s", got)
	}

	_, err = jf.libraryIDs(context.Background(), "a1b2c3", []string{"Cartoons"})
	if msg, transient := explain(err, config{}); transient || !strings.Contains(msg, "EXCLUDE_LIBRARIES") || !strings.Contains(msg, "Movies, Adult") {
		t.Errorf("unknown library: got %q (transient=%v)", msg, transient)
	}
}
