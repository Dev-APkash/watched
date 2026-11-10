package main

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// logbook is the stats page: totals, titles per year and top genres, worked out from the
// synced shelf only. Nothing is fetched or estimated for it.
type logbook struct {
	Titles, Movies, Shows int
	Episodes              int // distinct episodes watched
	Plays                 int // every play, rewatches included
	Minutes               int // from known runtimes only
	Untimed               int // titles with no runtime, left out of Minutes
	Years                 []logYear
	MaxYear               int // the busiest year's title count, to scale the bars
	Undated               int // titles with no watch date
	Genres                []logGenre
	MaxGenre              int
	Rewatched             []entry // films seen more than once, most plays first
	Decades               []logDecade
	MaxDecade             int
}

// logYear counts titles by the year they were last watched.
type logYear struct {
	Year, Movies, Shows, Minutes int
}

func (y logYear) Titles() int { return y.Movies + y.Shows }

type logGenre struct {
	Name  string
	Count int
}

type logDecade struct {
	Decade, Count int
}

const (
	topGenres    = 10
	topRewatched = 5
)

func buildLogbook(items []entry) logbook {
	var lb logbook
	years := map[int]*logYear{}
	genres := map[string]*logGenre{} // keyed by lower-case name so "Drama" and "drama" merge
	decades := map[int]int{}
	for _, e := range items {
		lb.Titles++
		if e.Type == "show" {
			lb.Shows++
			lb.Episodes += e.Played
		} else {
			lb.Movies++
		}
		lb.Plays += max(e.Plays, 1, e.Played)
		if e.Minutes > 0 {
			lb.Minutes += e.Minutes
		} else {
			lb.Untimed++
		}
		if e.LastPlayed.Year() > 1970 {
			y := e.LastPlayed.Local().Year()
			if years[y] == nil {
				years[y] = &logYear{Year: y}
			}
			if e.Type == "show" {
				years[y].Shows++
			} else {
				years[y].Movies++
			}
			years[y].Minutes += e.Minutes
		} else {
			lb.Undated++
		}
		seen := map[string]bool{}
		for _, g := range e.Genres {
			g = strings.TrimSpace(g)
			k := strings.ToLower(g)
			if g == "" || seen[k] {
				continue
			}
			seen[k] = true
			if genres[k] == nil {
				genres[k] = &logGenre{Name: g}
			}
			genres[k].Count++
		}
		if e.Year > 1800 {
			decades[e.Year/10*10]++
		}
		if e.Type == "movie" && e.Plays > 1 {
			lb.Rewatched = append(lb.Rewatched, e)
		}
	}

	for _, y := range years {
		lb.Years = append(lb.Years, *y)
		lb.MaxYear = max(lb.MaxYear, y.Titles())
	}
	slices.SortFunc(lb.Years, func(a, b logYear) int { return b.Year - a.Year }) // newest first

	for _, g := range genres {
		lb.Genres = append(lb.Genres, *g)
	}
	slices.SortFunc(lb.Genres, func(a, b logGenre) int {
		return cmp.Or(b.Count-a.Count, strings.Compare(a.Name, b.Name))
	})
	lb.Genres = lb.Genres[:min(len(lb.Genres), topGenres)]
	if len(lb.Genres) > 0 {
		lb.MaxGenre = lb.Genres[0].Count
	}

	for d, n := range decades {
		lb.Decades = append(lb.Decades, logDecade{d, n})
		lb.MaxDecade = max(lb.MaxDecade, n)
	}
	slices.SortFunc(lb.Decades, func(a, b logDecade) int { return a.Decade - b.Decade })

	slices.SortStableFunc(lb.Rewatched, func(a, b entry) int {
		return cmp.Or(b.Plays-a.Plays, b.LastPlayed.Compare(a.LastPlayed))
	})
	lb.Rewatched = lb.Rewatched[:min(len(lb.Rewatched), topRewatched)]
	return lb
}

// Hours rounds the watched time to whole hours.
func (lb logbook) Hours() int { return (lb.Minutes + 30) / 60 }

// Days is the watched time as days and hours, e.g. "3 days, 4 hours".
func (lb logbook) Days() string { return spanText(lb.Minutes) }

func spanText(minutes int) string {
	d := time.Duration(minutes) * time.Minute
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	plural := func(n int, s string) string {
		if n == 1 {
			return "1 " + s
		}
		return strconv.Itoa(n) + " " + s + "s"
	}
	switch {
	case days > 0 && hours > 0:
		return plural(days, "day") + ", " + plural(hours, "hour")
	case days > 0:
		return plural(days, "day")
	case hours > 0:
		return plural(hours, "hour")
	}
	return plural(minutes, "minute")
}
