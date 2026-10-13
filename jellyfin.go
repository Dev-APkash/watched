package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// jellyfin is a minimal client for the few Jellyfin REST endpoints Watched needs.
type jellyfin struct {
	baseURL  string // without trailing slash, may include a base path
	apiKey   string
	http     *http.Client
	pageSize int
}

func newJellyfin(baseURL, apiKey string) *jellyfin {
	return &jellyfin{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		http:     &http.Client{Timeout: 30 * time.Second},
		pageSize: 500,
	}
}

// apiError is a non-200 response from Jellyfin.
type apiError struct {
	Status int
	Path   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("jellyfin: GET %s returned HTTP %d", e.Path, e.Status)
}

// errBadResponse means the server answered, but not with the JSON Jellyfin would send.
var errBadResponse = errors.New("unexpected (non-Jellyfin) response")

// item is the subset of Jellyfin's BaseItemDto that Watched reads.
type item struct {
	ID             string `json:"Id"`
	Name           string
	Type           string
	ProductionYear int
	SeriesID       string `json:"SeriesId"`
	ImageTags      map[string]string
	ProviderIds    map[string]string // e.g. {"Imdb": "tt0111161", "Tmdb": "278"}; needs fields=ProviderIds
	// Episode numbering
	ParentIndexNumber *int // season
	IndexNumber       *int // episode
	UserData          struct {
		Played            bool
		LastPlayedDate    jfTime
		UnplayedItemCount *int
	}
}

// jfTime parses Jellyfin timestamps, which may or may not carry a zone suffix.
type jfTime struct{ time.Time }

func (t *jfTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return fmt.Errorf("jellyfin: unrecognised date %q", s)
}

// do performs an authenticated GET and returns the response only if it is a 200.
func (j *jellyfin) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := j.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+j.apiKey+`"`)
	resp, err := j.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, &apiError{Status: resp.StatusCode, Path: path}
	}
	return resp, nil
}

func (j *jellyfin) get(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := j.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w from GET %s: %v", errBadResponse, path, err)
	}
	return nil
}

type named struct {
	Name string
	ID   string `json:"Id"`
}

// userID resolves a username (case-insensitive) to its Jellyfin user ID.
func (j *jellyfin) userID(ctx context.Context, name string) (string, error) {
	var users []named
	if err := j.get(ctx, "/Users", nil, &users); err != nil {
		return "", err
	}
	return find(users, name, "user", "JELLYFIN_USER")
}

// libraryIDs resolves library names (case-insensitive) as the user sees them to their IDs.
func (j *jellyfin) libraryIDs(ctx context.Context, userID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var views struct{ Items []named }
	if err := j.get(ctx, "/UserViews", url.Values{"userId": {userID}}, &views); err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range names {
		id, err := find(views.Items, n, "library", "EXCLUDE_LIBRARIES")
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func find(list []named, name, kind, env string) (string, error) {
	var names []string
	for _, x := range list {
		if strings.EqualFold(x.Name, name) {
			return x.ID, nil
		}
		names = append(names, x.Name)
	}
	return "", &notFoundError{Kind: kind, Name: name, Env: env, Available: names}
}

// notFoundError is a configured user or library that Jellyfin doesn't know.
type notFoundError struct {
	Kind, Name, Env string
	Available       []string
}

func (e *notFoundError) Error() string {
	return fmt.Sprintf("%s %q not found on the Jellyfin server (available: %s). Check %s.",
		e.Kind, e.Name, strings.Join(e.Available, ", "), e.Env)
}

// items fetches every page of GET /Items for the given query, dropping duplicates.
func (j *jellyfin) items(ctx context.Context, q url.Values) ([]item, error) {
	q.Set("sortBy", "SortName")
	q.Set("enableTotalRecordCount", "true")
	q.Set("limit", strconv.Itoa(j.pageSize))
	var all []item
	seen := map[string]bool{}
	for start := 0; ; {
		q.Set("startIndex", strconv.Itoa(start))
		var page struct {
			Items            []item
			TotalRecordCount int
		}
		if err := j.get(ctx, "/Items", q, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if !seen[it.ID] {
				seen[it.ID] = true
				all = append(all, it)
			}
		}
		start += len(page.Items)
		if len(page.Items) == 0 || start >= page.TotalRecordCount {
			return all, nil
		}
	}
}

// poster streams an item's primary image, scaled down and recompressed by Jellyfin.
func (j *jellyfin) poster(ctx context.Context, id, tag string) (io.ReadCloser, error) {
	q := url.Values{"maxWidth": {"200"}, "quality": {"60"}, "format": {"Jpg"}, "tag": {tag}}
	resp, err := j.do(ctx, "/Items/"+url.PathEscape(id)+"/Images/Primary", q)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
