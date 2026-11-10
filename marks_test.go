package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T, items ...entry) *app {
	t.Helper()
	a := &app{cfg: config{dataDir: t.TempDir(), user: "alice"}, tr: newTrakt("")}
	a.items = items
	return a
}

func ptr[T any](v T) *T { return &v }

func TestSetMarks(t *testing.T) {
	arrival := entry{ID: "m1", Type: "movie", Title: "Arrival", Ext: map[string]string{"imdb": "tt2543164"}}
	a := testApp(t, arrival)

	if err := a.setMarks(map[string]markPatch{"m1": {Rating: ptr(4)}}); err != nil {
		t.Fatal(err)
	}
	if err := a.setMarks(map[string]markPatch{"m1": {Note: ptr("the heptapods")}}); err != nil {
		t.Fatal(err)
	}
	m := a.marks["m1"]
	if m.Rating != 4 || m.Note != "the heptapods" || m.Title != "Arrival" || m.Ext["imdb"] != "tt2543164" {
		t.Errorf("a note must not clear the rating, and the title is copied in: %+v", m)
	}

	// Saved to disk and readable on the next start.
	var onDisk map[string]mark
	if err := loadJSON(a.marksPath(), &onDisk); err != nil || onDisk["m1"].Note != "the heptapods" {
		t.Errorf("marks.json = %+v, %v", onDisk, err)
	}

	// Clearing both removes the mark.
	if err := a.setMarks(map[string]markPatch{"m1": {Rating: ptr(0), Note: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.marks["m1"]; ok {
		t.Error("an empty mark should be deleted")
	}

	for name, p := range map[string]map[string]markPatch{
		"rating 6":    {"m1": {Rating: ptr(6)}},
		"negative":    {"m1": {Rating: ptr(-1)}},
		"empty id":    {"": {Rating: ptr(1)}},
		"sample id":   {"sample-3": {Rating: ptr(1)}},
		"long note":   {"m1": {Note: ptr(strings.Repeat("x", maxNoteLen+1))}},
		"invalid utf": {"m1": {Note: ptr("\xff")}},
	} {
		if err := a.setMarks(p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestBackupAndCSV(t *testing.T) {
	played := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	items := []entry{
		{ID: "m1", Type: "movie", Title: "=HYPERLINK(1)", Year: 2016, LastPlayed: played, Plays: 2, Ext: map[string]string{"imdb": "tt1"}},
		{ID: "s1", Type: "show", Title: "Dark", FirstPlayed: played, Plays: 4, Played: 3, Total: 26, Episodes: []string{"S01E01", "S01E02", "S01E03"}},
	}
	ms := map[string]mark{
		"m1":   {Rating: 5, Note: "+great"},
		"gone": {Rating: 2, Type: "movie", Title: "Left the shelf", Ext: map[string]string{"tmdb": "9"}},
	}
	b := buildBackup("alice", items, ms)
	if len(b.Items) != 3 || b.Items[2].ID != "gone" || b.Items[2].OnShelf || !b.Items[0].OnShelf {
		t.Fatalf("backup items = %+v", b.Items)
	}
	if b.Items[0].Rating != 5 || len(b.Items[1].Episodes) != 3 {
		t.Errorf("backup lost data: %+v", b.Items)
	}

	out, err := writeCSV(b)
	if err != nil {
		t.Fatal(err)
	}
	csv := string(out)
	for _, want := range []string{
		"type,title,year,first_watched,last_watched,plays,",
		"movie,'=HYPERLINK(1),2016,,2026-03-01T20:00:00Z,2,,,5,'+great,tt1,,,true",
		"show,Dark,,2026-03-01T20:00:00Z,,4,3,26,,,,,,true",
		"movie,Left the shelf,,,,,,,2,,,9,,false",
	} {
		if !strings.Contains(csv, want) {
			t.Errorf("CSV missing %q:\n%s", want, csv)
		}
	}
}

func TestPlanRestore(t *testing.T) {
	items := []entry{
		{ID: "m1", Type: "movie", Title: "Arrival"},
		// Same film, but Jellyfin gave it a new ID since the backup was made.
		{ID: "new-id", Type: "movie", Title: "Coherence", Ext: map[string]string{"tmdb": "220289"}},
	}
	b := backup{App: "watched", Items: []backupRecord{
		{ID: "m1", Type: "movie", Rating: 4},
		{ID: "old-id", Type: "movie", Ext: map[string]string{"tmdb": "220289"}, Note: "dinner party"},
		{ID: "elsewhere", Type: "movie", Rating: 3},
		{ID: "unrated", Type: "movie"},
	}}
	patches, res, err := planRestore(b, items)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 2 || res.Kept != 1 || len(patches) != 3 {
		t.Errorf("result = %+v, patches = %d", res, len(patches))
	}
	if p, ok := patches["new-id"]; !ok || *p.Note != "dinner party" {
		t.Errorf("matching by external id failed: %+v", patches)
	}
	if _, _, err := planRestore(backup{App: "other"}, items); err == nil {
		t.Error("a file that isn't a Watched backup should be refused")
	}
}

func TestMarkRoutes(t *testing.T) {
	a := testApp(t, entry{ID: "m1", Type: "movie", Title: "Arrival"})
	h := a.routes()
	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do("POST", "/api/marks", `{"m1":{"rating":3,"note":"</script><b>hi"}}`); w.Code != http.StatusNoContent {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/marks", `{"m1":{"rating":9}}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad rating: %d", w.Code)
	}
	if w := do("POST", "/api/marks", `{"m1":{"rating":1}}`, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Errorf("cross-site write should be refused: %d", w.Code)
	}

	// The page carries the marks, with the note escaped so it can't end the script tag.
	page := do("GET", "/", "").Body.String()
	if !strings.Contains(page, `"m1":{"rating":3,"note":"\u003c/script\u003e\u003cb\u003ehi"}`) {
		t.Errorf("page marks not embedded safely")
	}

	w := do("GET", "/export.json", "")
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("export should download: %v", w.Header())
	}
	var b backup
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || len(b.Items) != 1 || b.Items[0].Rating != 3 {
		t.Fatalf("export = %s (%v)", w.Body, err)
	}
	if w := do("GET", "/export.csv", ""); !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("csv content type = %q", w.Header().Get("Content-Type"))
	}

	// Wipe, then restore from the export.
	a.setMarks(map[string]markPatch{"m1": {Rating: ptr(0), Note: ptr("")}})
	backupJSON, _ := json.Marshal(b)
	w = do("POST", "/api/marks/restore", string(backupJSON))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restored":1`) || a.marks["m1"].Rating != 3 {
		t.Errorf("restore: %d %s %+v", w.Code, w.Body, a.marks)
	}
	if w := do("POST", "/api/marks/restore", `{"items":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("non-backup restore: %d", w.Code)
	}
}
