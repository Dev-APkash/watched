package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// defaultTraktClientID is the public client ID of this project's Trakt API app
// (https://trakt.tv/oauth/applications). It is not a secret; TRAKT_CLIENT_ID overrides it.
const defaultTraktClientID = "nqV3K6wf-adMZOK3qTwj3B0bm4dTNc3gJqC5vRDMe6Y"

// trakt is a minimal client for importing a public Trakt profile's watch history.
type trakt struct {
	clientID string
	baseURL  string
	http     *http.Client
}

func newTrakt(clientID string) *trakt {
	return &trakt{clientID: clientID, baseURL: "https://api.trakt.tv", http: &http.Client{Timeout: 60 * time.Second}}
}

// traktImport is the snapshot kept in DATA_DIR/trakt.json.
type traktImport struct {
	User       string    `json:"user"`
	ImportedAt time.Time `json:"importedAt"`
	Items      []entry   `json:"items"`
}

// traktUser extracts the profile slug from a link like https://trakt.tv/users/alice/history,
// or accepts a bare username.
func traktUser(input string) (string, error) {
	s := strings.TrimSpace(input)
	if _, after, ok := strings.Cut(s, "/users/"); ok {
		s = after
	}
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, "?")
	s, _, _ = strings.Cut(s, "#")
	bad := func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}
	if s == "" || s == "." || s == ".." || strings.ContainsFunc(s, bad) {
		return "", errors.New("paste a Trakt profile link like https://trakt.tv/users/yourname")
	}
	return s, nil
}

type traktError struct {
	Status int
	User   string
}

func (e *traktError) Error() string {
	switch e.Status {
	case http.StatusNotFound:
		return fmt.Sprintf("Trakt user %q not found. Check the link.", e.User)
	case http.StatusUnauthorized:
		return fmt.Sprintf("%q's Trakt profile is private. Turn off \"private\" in your Trakt settings, import, then turn it back on.", e.User)
	case http.StatusForbidden:
		return "Trakt rejected the client ID. Check TRAKT_CLIENT_ID."
	case http.StatusTooManyRequests:
		return "Trakt's rate limit was reached. Try again in a few minutes."
	}
	return fmt.Sprintf("Trakt returned HTTP %d. Try again later.", e.Status)
}

// page fetches one page of a paginated Trakt list into out and returns the page count.
// It waits and retries when Trakt's rate limit is hit.
func (t *trakt) page(ctx context.Context, path, user string, page int, out any) (pages int, err error) {
	u := fmt.Sprintf("%s%s&page=%d&limit=250", t.baseURL, path, page)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Watched")
		req.Header.Set("trakt-api-version", "2")
		req.Header.Set("trakt-api-key", t.clientID)
		resp, err := t.http.Do(req)
		if err != nil {
			return 0, fmt.Errorf("cannot reach Trakt: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			resp.Body.Close()
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			select {
			case <-time.After(time.Duration(min(max(wait, 1), 60)) * time.Second):
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, &traktError{Status: resp.StatusCode, User: user}
		}
		pages, _ = strconv.Atoi(resp.Header.Get("X-Pagination-Page-Count"))
		return max(pages, 1), json.NewDecoder(resp.Body).Decode(out)
	}
}

// each calls fn for every item of a paginated Trakt list.
func each[T any](ctx context.Context, t *trakt, path, user string, fn func(T)) error {
	for p, pages := 1, 1; p <= pages; p++ {
		var items []T
		var err error
		if pages, err = t.page(ctx, path, user, p, &items); err != nil {
			return err
		}
		for _, it := range items {
			fn(it)
		}
	}
	return nil
}

type traktMedia struct {
	Title string
	Year  int
	IDs   struct {
		Trakt int
		Imdb  string
		Tmdb  int
		Tvdb  int
	}
	AiredEpisodes int `json:"aired_episodes"` // shows
	Images        struct {
		Poster []string // host and path without scheme, e.g. "media.trakt.tv/images/…/posters/medium/….webp"
	}
}

func (m traktMedia) ext() map[string]string {
	ext := map[string]string{}
	if m.IDs.Imdb != "" {
		ext["imdb"] = m.IDs.Imdb
	}
	if m.IDs.Tmdb != 0 {
		ext["tmdb"] = strconv.Itoa(m.IDs.Tmdb)
	}
	if m.IDs.Tvdb != 0 {
		ext["tvdb"] = strconv.Itoa(m.IDs.Tvdb)
	}
	return ext
}

// entry builds a card for this title; the poster is cached like Jellyfin's, as Trakt requires.
func (m traktMedia) entry(kind string, last time.Time) entry {
	e := entry{
		ID: fmt.Sprintf("trakt-%s-%d", kind, m.IDs.Trakt), Type: kind, Title: m.Title, Year: m.Year,
		LastPlayed: last, Ext: m.ext(),
	}
	if len(m.Images.Poster) > 0 {
		// "thumb" (300x450, ~20 KB) instead of the default "medium" (600x900, ~70 KB).
		e.PosterURL = "https://" + strings.Replace(strings.TrimPrefix(m.Images.Poster[0], "https://"), "/posters/medium/", "/posters/thumb/", 1)
		e.PosterTag = safeName(path.Base(e.PosterURL))
	}
	return e
}

// watched fetches everything a public Trakt profile has watched.
func (t *trakt) watched(ctx context.Context, user string) ([]entry, error) {
	var out []entry
	u := "/users/" + url.PathEscape(user)
	err := each(ctx, t, u+"/watched/movies?extended=images", user, func(m struct {
		LastWatchedAt time.Time `json:"last_watched_at"`
		Movie         traktMedia
	}) {
		out = append(out, m.Movie.entry("movie", m.LastWatchedAt))
	})
	if err != nil {
		return nil, err
	}

	// The watched-shows list has the totals but no longer includes which episodes
	// were watched or posters, so those come from the episode history.
	type show struct {
		LastWatchedAt time.Time  `json:"last_watched_at"`
		ResetAt       *time.Time `json:"reset_at"` // set when the user started re-watching
		Show          traktMedia
	}
	shows := map[int]*show{}
	var order []int
	err = each(ctx, t, u+"/watched/shows?extended=images", user, func(s show) {
		shows[s.Show.IDs.Trakt] = &s
		order = append(order, s.Show.IDs.Trakt)
	})
	if err != nil {
		return nil, err
	}

	episodes := map[int]map[string]bool{}
	posters := map[int][]string{}
	err = each(ctx, t, u+"/history/episodes?extended=images", user, func(h struct {
		WatchedAt time.Time `json:"watched_at"`
		Episode   struct{ Season, Number int }
		Show      traktMedia
	}) {
		s := shows[h.Show.IDs.Trakt]
		if s == nil || h.Episode.Season == 0 || // specials aren't part of aired_episodes
			s.ResetAt != nil && h.WatchedAt.Before(*s.ResetAt) {
			return
		}
		id := h.Show.IDs.Trakt
		if episodes[id] == nil {
			episodes[id] = map[string]bool{}
			posters[id] = h.Show.Images.Poster
		}
		episodes[id][epKey(h.Episode.Season, h.Episode.Number)] = true
	})
	if err != nil {
		return nil, err
	}

	for _, id := range order {
		s, eps := shows[id], episodes[id]
		if len(eps) == 0 {
			continue
		}
		s.Show.Images.Poster = posters[id]
		e := s.Show.entry("show", s.LastWatchedAt)
		for k := range eps {
			e.Episodes = append(e.Episodes, k)
		}
		slices.Sort(e.Episodes)
		e.Played, e.Total = len(eps), max(s.Show.AiredEpisodes, len(eps))
		out = append(out, e)
	}
	sortRecent(out)
	return out, nil
}

// image downloads a poster from Trakt's image CDN.
func (t *trakt) image(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".trakt.tv") {
		return nil, fmt.Errorf("refusing non-Trakt image URL %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Watched")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("trakt image: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
