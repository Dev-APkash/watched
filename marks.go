package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// mark is what the viewer adds to a title: a star rating and liner notes.
// Type, Title and Ext are copied from the title when the mark is saved, so a
// mark can still be matched up and exported after the title leaves the shelf.
type mark struct {
	Rating  int               `json:"rating,omitempty"`
	Note    string            `json:"note,omitempty"`
	Type    string            `json:"type,omitempty"`
	Title   string            `json:"title,omitempty"`
	Ext     map[string]string `json:"ext,omitempty"`
	Updated time.Time         `json:"updated"`
}

func (m mark) empty() bool { return m.Rating == 0 && m.Note == "" }

// markPatch changes a mark. A nil field is left as it is; 0 or "" clears it.
type markPatch struct {
	Rating *int    `json:"rating"`
	Note   *string `json:"note"`
}

const (
	maxNoteLen = 20000 // bytes of liner notes per title
	maxIDLen   = 200
)

func validMarkID(id string) bool {
	return id != "" && len(id) <= maxIDLen && !strings.HasPrefix(id, "sample-") && utf8.ValidString(id)
}

// setMarks applies patches keyed by item ID and saves marks.json.
func (a *app) setMarks(patches map[string]markPatch) error {
	for id, p := range patches {
		if !validMarkID(id) {
			return fmt.Errorf("invalid item id %q", id)
		}
		if p.Rating != nil && (*p.Rating < 0 || *p.Rating > 5) {
			return fmt.Errorf("rating must be 0 to 5, got %d", *p.Rating)
		}
		if p.Note != nil && (len(*p.Note) > maxNoteLen || !utf8.ValidString(*p.Note)) {
			return fmt.Errorf("note for %q is too long or not valid text", id)
		}
	}
	a.mu.RLock()
	byID := make(map[string]entry, len(patches))
	for _, e := range a.items {
		if _, ok := patches[e.ID]; ok {
			byID[e.ID] = e
		}
	}
	a.mu.RUnlock()

	a.marksMu.Lock()
	defer a.marksMu.Unlock()
	next := maps.Clone(a.marks)
	if next == nil {
		next = map[string]mark{}
	}
	now := time.Now().UTC()
	for id, p := range patches {
		m := next[id]
		if p.Rating != nil {
			m.Rating = *p.Rating
		}
		if p.Note != nil {
			m.Note = *p.Note
		}
		if e, ok := byID[id]; ok {
			m.Type, m.Title, m.Ext = e.Type, e.Title, e.Ext
		}
		m.Updated = now
		if m.empty() {
			delete(next, id)
		} else {
			next[id] = m
		}
	}
	if err := saveJSON(a.marksPath(), next); err != nil {
		return err
	}
	a.marks = next
	return nil
}

// marksSnapshot returns the saved marks. The map must not be modified.
func (a *app) marksSnapshot() map[string]mark {
	a.marksMu.Lock()
	defer a.marksMu.Unlock()
	return a.marks
}

// pageMarks is the rating and note of each title, as the page needs them.
func pageMarks(ms map[string]mark) map[string]markPatch {
	out := make(map[string]markPatch, len(ms))
	for id, m := range ms {
		out[id] = markPatch{Rating: &m.Rating, Note: &m.Note}
	}
	return out
}

// backup is the JSON file behind "Download backup".
type backup struct {
	App        string         `json:"app"` // always "watched"
	Version    int            `json:"version"`
	ExportedAt time.Time      `json:"exportedAt"`
	User       string         `json:"user"`
	Items      []backupRecord `json:"items"`
}

type backupRecord struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Year        int               `json:"year,omitempty"`
	LastPlayed  *time.Time        `json:"lastPlayed,omitempty"`
	FirstPlayed *time.Time        `json:"firstPlayed,omitempty"`
	Plays       int               `json:"plays,omitempty"`
	Played      int               `json:"played,omitempty"`
	Total       int               `json:"total,omitempty"`
	Episodes    []string          `json:"episodes,omitempty"`
	Ext         map[string]string `json:"ext,omitempty"`
	Rating      int               `json:"rating,omitempty"`
	Note        string            `json:"note,omitempty"`
	OnShelf     bool              `json:"onShelf"` // false: only a rating or note is left, the title is gone
}

// buildBackup lists every title on the shelf, then any marks whose title has left it.
func buildBackup(user string, items []entry, ms map[string]mark) backup {
	b := backup{App: "watched", Version: 1, ExportedAt: time.Now().UTC(), User: user, Items: []backupRecord{}}
	seen := map[string]bool{}
	for _, e := range items {
		seen[e.ID] = true
		m := ms[e.ID]
		r := backupRecord{
			ID: e.ID, Type: e.Type, Title: e.Title, Year: e.Year,
			Plays: e.Plays, Played: e.Played, Total: e.Total, Episodes: e.Episodes, Ext: e.Ext,
			Rating: m.Rating, Note: m.Note, OnShelf: true,
		}
		if !e.LastPlayed.IsZero() {
			t := e.LastPlayed.UTC()
			r.LastPlayed = &t
		}
		if !e.FirstPlayed.IsZero() {
			t := e.FirstPlayed.UTC()
			r.FirstPlayed = &t
		}
		b.Items = append(b.Items, r)
	}
	for _, id := range slices.Sorted(maps.Keys(ms)) {
		if seen[id] {
			continue
		}
		m := ms[id]
		b.Items = append(b.Items, backupRecord{ID: id, Type: m.Type, Title: m.Title, Ext: m.Ext, Rating: m.Rating, Note: m.Note})
	}
	return b
}

// csvSafe stops spreadsheet apps from running a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeCSV(b backup) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"type", "title", "year", "first_watched", "last_watched", "plays", "episodes_watched", "episodes_total", "rating", "note", "imdb", "tmdb", "tvdb", "on_shelf"})
	itoa := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	for _, r := range b.Items {
		stamp := func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format(time.RFC3339)
		}
		eps, total := "", ""
		if r.Type == "show" {
			eps, total = strconv.Itoa(r.Played), strconv.Itoa(r.Total)
		}
		w.Write([]string{
			r.Type, csvSafe(r.Title), itoa(r.Year), stamp(r.FirstPlayed), stamp(r.LastPlayed), itoa(r.Plays), eps, total, itoa(r.Rating), csvSafe(r.Note),
			r.Ext["imdb"], r.Ext["tmdb"], r.Ext["tvdb"], strconv.FormatBool(r.OnShelf),
		})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// restoreResult says how a backup was applied.
type restoreResult struct {
	Restored int `json:"restored"` // marks matched to a title on the shelf
	Kept     int `json:"kept"`     // marks for titles not on the shelf, saved for when they return
}

// planRestore works out which item each mark in a backup belongs to: the same
// ID if it is still on the shelf, otherwise the title with a matching IMDb,
// TMDB or TVDB id, otherwise its old ID.
func planRestore(b backup, items []entry) (map[string]markPatch, restoreResult, error) {
	if b.App != "watched" {
		return nil, restoreResult{}, errors.New("this file is not a Watched backup")
	}
	ids := map[string]bool{}
	byKey := map[string]string{}
	for _, e := range items {
		ids[e.ID] = true
		for _, k := range e.keys() {
			byKey[k] = e.ID
		}
	}
	patches := map[string]markPatch{}
	var res restoreResult
	for _, r := range b.Items {
		if r.Rating == 0 && r.Note == "" {
			continue
		}
		id := r.ID
		if !ids[id] {
			for _, k := range (entry{Type: r.Type, Ext: r.Ext}).keys() {
				if match, ok := byKey[k]; ok {
					id = match
					break
				}
			}
		}
		if !validMarkID(id) {
			continue
		}
		rating, note := r.Rating, r.Note
		patches[id] = markPatch{Rating: &rating, Note: &note}
		if ids[id] {
			res.Restored++
		} else {
			res.Kept++
		}
	}
	return patches, res, nil
}

func (a *app) markRoutes(mux *http.ServeMux) {
	// Save ratings and notes: {"<item id>": {"rating": 4, "note": "..."}, ...}
	mux.HandleFunc("POST /api/marks", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		var patches map[string]markPatch
		if err := json.NewDecoder(r.Body).Decode(&patches); err != nil {
			http.Error(w, "expected a JSON object of ratings and notes", http.StatusBadRequest)
			return
		}
		if err := a.setMarks(patches); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/marks/restore", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		var b backup
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, "this file is not a Watched backup", http.StatusBadRequest)
			return
		}
		a.mu.RLock()
		items := a.items
		a.mu.RUnlock()
		patches, res, err := planRestore(b, items)
		if err == nil {
			err = a.setMarks(patches)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(res)
	})
	export := func(w http.ResponseWriter, r *http.Request, ext string) {
		a.mu.RLock()
		items := a.items
		a.mu.RUnlock()
		b := buildBackup(a.cfg.user, items, a.marksSnapshot())
		var body []byte
		var err error
		ctype := "application/json; charset=utf-8"
		if ext == "csv" {
			ctype = "text/csv; charset=utf-8"
			body, err = writeCSV(b)
		} else {
			body, err = json.MarshalIndent(b, "", "  ")
		}
		if err != nil {
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="watched-%s.%s"`, time.Now().Format("2006-01-02"), ext))
		w.Header().Set("Cache-Control", "no-store")
		w.Write(body)
	}
	mux.HandleFunc("GET /export.json", func(w http.ResponseWriter, r *http.Request) { export(w, r, "json") })
	mux.HandleFunc("GET /export.csv", func(w http.ResponseWriter, r *http.Request) { export(w, r, "csv") })
}
