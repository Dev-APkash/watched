package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// entry is one card on the page: a watched movie or a series with at least one watched episode.
type entry struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"` // "movie" or "show"
	Title      string            `json:"title"`
	Year       int               `json:"year,omitempty"`
	LastPlayed time.Time         `json:"lastPlayed"`
	Played     int               `json:"played,omitempty"` // shows: episodes watched
	Total      int               `json:"total,omitempty"`  // shows: episodes available
	PosterTag  string            `json:"posterTag,omitempty"`
	PosterURL  string            `json:"posterUrl,omitempty"` // set for posters that don't come from Jellyfin
	Ext        map[string]string `json:"ext,omitempty"`       // external ids ("imdb", "tmdb", "tvdb") for matching
	Episodes   []string          `json:"episodes,omitempty"`  // shows: watched episodes as "S01E02"
}

func (e entry) Completed() bool { return e.Type == "show" && e.Played >= e.Total }

// Poster is the cached poster's file name, or "" if the item has none.
func (e entry) Poster() string {
	if e.PosterTag == "" {
		return ""
	}
	ext := ".jpg"
	if e.PosterURL != "" {
		ext = ".webp" // Trakt serves WebP
	}
	return safeName(e.ID) + "-" + safeName(e.PosterTag) + ext
}

// keys identifies the title across services, e.g. "movie:imdb:tt0111161".
func (e entry) keys() []string {
	var ks []string
	for k, v := range e.Ext {
		ks = append(ks, e.Type+":"+k+":"+v)
	}
	return ks
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func epKey(season, episode int) string { return fmt.Sprintf("S%02dE%02d", season, episode) }

// state is persisted to DATA_DIR/state.json so the page survives restarts and Jellyfin outages.
type state struct {
	User     string    `json:"user"`
	LastSync time.Time `json:"lastSync"`
	Items    []entry   `json:"items"` // from Jellyfin only; Trakt imports are kept separately
}

// loadJSON reads path into v; a missing file leaves v untouched.
func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fetchWatched asks Jellyfin for everything the user has watched outside the
// excluded libraries, most recent first.
func fetchWatched(ctx context.Context, jf *jellyfin, userID string, excludeLibs []string) ([]entry, error) {
	base := func() url.Values {
		return url.Values{
			"userId":         {userID},
			"recursive":      {"true"},
			"enableUserData": {"true"},
			"isPlayed":       {"true"},
		}
	}
	withPoster := func(q url.Values) url.Values {
		q.Set("enableImageTypes", "Primary")
		q.Set("imageTypeLimit", "1")
		q.Set("fields", "ProviderIds")
		return q
	}

	// /Items can't exclude a library, so look up what was played there and skip it.
	hidden := map[string]bool{}
	for _, lib := range excludeLibs {
		q := base()
		q.Set("parentId", lib)
		q.Set("includeItemTypes", "Movie,Episode")
		q.Set("enableUserData", "false")
		q.Set("enableImages", "false")
		items, err := jf.items(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			hidden[it.ID] = true
		}
	}

	q := withPoster(base())
	q.Set("includeItemTypes", "Movie")
	movies, err := jf.items(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, m := range movies {
		if hidden[m.ID] {
			continue
		}
		out = append(out, entry{
			ID: m.ID, Type: "movie", Title: m.Name, Year: m.ProductionYear,
			LastPlayed: m.UserData.LastPlayedDate.Time, PosterTag: m.ImageTags["Primary"],
			Ext: externalIDs(m.ProviderIds),
		})
	}

	q = base()
	q.Set("includeItemTypes", "Episode")
	q.Set("enableImages", "false")
	episodes, err := jf.items(ctx, q)
	if err != nil {
		return nil, err
	}
	type progress struct {
		played   int
		last     time.Time
		episodes []string
	}
	bySeries := map[string]*progress{}
	for _, ep := range episodes {
		if ep.SeriesID == "" || hidden[ep.ID] {
			continue
		}
		p := bySeries[ep.SeriesID]
		if p == nil {
			p = &progress{}
			bySeries[ep.SeriesID] = p
		}
		p.played++
		if t := ep.UserData.LastPlayedDate.Time; t.After(p.last) {
			p.last = t
		}
		if ep.ParentIndexNumber != nil && ep.IndexNumber != nil {
			p.episodes = append(p.episodes, epKey(*ep.ParentIndexNumber, *ep.IndexNumber))
		}
	}

	ids := make([]string, 0, len(bySeries))
	for id := range bySeries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for batch := range slices.Chunk(ids, 100) {
		q = withPoster(base())
		q.Del("isPlayed") // partly watched series are not "played"
		q.Set("ids", strings.Join(batch, ","))
		series, err := jf.items(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			p := bySeries[s.ID]
			if p == nil {
				continue
			}
			unplayed := 0
			if s.UserData.UnplayedItemCount != nil {
				unplayed = *s.UserData.UnplayedItemCount
			}
			slices.Sort(p.episodes)
			out = append(out, entry{
				ID: s.ID, Type: "show", Title: s.Name, Year: s.ProductionYear,
				LastPlayed: p.last, Played: p.played, Total: p.played + unplayed,
				PosterTag: s.ImageTags["Primary"], Ext: externalIDs(s.ProviderIds), Episodes: p.episodes,
			})
		}
	}

	sortRecent(out)
	return out, nil
}

func externalIDs(providerIDs map[string]string) map[string]string {
	ext := map[string]string{}
	for _, k := range []string{"Imdb", "Tmdb", "Tvdb"} {
		if v := providerIDs[k]; v != "" {
			ext[strings.ToLower(k)] = v
		}
	}
	if len(ext) == 0 {
		return nil
	}
	return ext
}

func sortRecent(items []entry) {
	slices.SortStableFunc(items, func(a, b entry) int { return b.LastPlayed.Compare(a.LastPlayed) })
}

// merge adds imported entries (from Trakt) to the Jellyfin ones. A title found in
// both, matched by IMDb/TMDB/TVDB id, becomes one card with the combined history.
func merge(jf, imported []entry) []entry {
	out := slices.Clone(jf)
	index := map[string]int{}
	for i, e := range out {
		for _, k := range e.keys() {
			index[k] = i
		}
	}
	for _, t := range imported {
		i, found := 0, false
		for _, k := range t.keys() {
			if i, found = index[k]; found {
				break
			}
		}
		if !found {
			out = append(out, t)
			continue
		}
		e := &out[i]
		if t.LastPlayed.After(e.LastPlayed) {
			e.LastPlayed = t.LastPlayed
		}
		if e.Type == "show" {
			seen := map[string]bool{}
			for _, k := range e.Episodes {
				seen[k] = true
			}
			eps := slices.Clone(e.Episodes)
			for _, k := range t.Episodes {
				if !seen[k] {
					seen[k] = true
					eps = append(eps, k)
					e.Played++
				}
			}
			e.Episodes = eps
			e.Total = max(e.Total, t.Total, e.Played)
		}
	}
	sortRecent(out)
	return out
}

// posterSource fetches the poster image for an entry.
type posterSource func(ctx context.Context, e entry) (io.ReadCloser, error)

// syncPosters downloads missing posters and deletes cached files no longer referenced.
// Entries whose poster could not be downloaded get their tag cleared so they show a placeholder.
func syncPosters(ctx context.Context, fetch posterSource, dir string, items []entry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var missing []int
	for i, e := range items {
		if name := e.Poster(); name != "" {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				missing = append(missing, i)
			}
		}
	}

	work := make(chan int)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				e := &items[i]
				if err := downloadPoster(ctx, fetch, dir, *e); err != nil {
					slog.Warn("poster download failed", "title", e.Title, "err", err)
					e.PosterTag = "" // each worker owns distinct indices
				}
			}
		}()
	}
	for _, i := range missing {
		work <- i
	}
	close(work)
	wg.Wait()

	keep := map[string]bool{}
	for _, e := range items {
		keep[e.Poster()] = true
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !keep[f.Name()] {
			os.Remove(filepath.Join(dir, f.Name()))
		}
	}
	return nil
}

func downloadPoster(ctx context.Context, fetch posterSource, dir string, e entry) error {
	body, err := fetch(ctx, e)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, e.Poster()))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
