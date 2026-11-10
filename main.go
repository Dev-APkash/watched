// Watched shows a poster grid of everything a Jellyfin user has watched.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML string

// assetsFS holds the page's static artwork (the animated cabin backdrop and its still frame).
//
//go:embed assets
var assetsFS embed.FS

var tmplFuncs = template.FuncMap{
	"lower": strings.ToLower,
	"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"date": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("Jan 2, 2006")
	},
	"json": func(v any) template.JS {
		b, err := json.Marshal(v)
		if err != nil {
			return "[]"
		}
		return template.JS(b)
	},
}

var page = template.Must(template.New("index").Funcs(tmplFuncs).Parse(indexHTML))

type config struct {
	url, apiKey, user string
	refresh           time.Duration
	port              string
	dataDir           string
	excludeLibs       []string
	traktClientID     string
	seerrURL          string
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}

func defaultDataDir() string {
	if v := os.Getenv("DATA_DIR"); v != "" {
		return v
	}
	if os.PathSeparator == '\\' {
		return "data"
	}
	return "/data"
}

// loadConfig reads the environment and reports every problem at once.
func loadConfig() (config, []string) {
	loadDotEnv(".env")
	c := config{
		url:     strings.TrimRight(envOr("JELLYFIN_URL", ""), "/"),
		apiKey:  envOr("JELLYFIN_API_KEY", ""),
		user:    envOr("JELLYFIN_USER", ""),
		port:    envOr("PORT", "8080"),
		dataDir: defaultDataDir(),
		// The built-in client ID can be overridden, or set to "off" to disable Trakt import.
		traktClientID: envOr("TRAKT_CLIENT_ID", defaultTraktClientID),
		seerrURL:      strings.TrimRight(envOr("SEERR_URL", envOr("JELLYSEERR_URL", "")), "/"),
	}
	if c.traktClientID == "off" {
		c.traktClientID = ""
	}
	for _, name := range strings.Split(os.Getenv("EXCLUDE_LIBRARIES"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.excludeLibs = append(c.excludeLibs, name)
		}
	}
	var problems []string
	if c.url == "" {
		problems = append(problems, "JELLYFIN_URL is required (e.g. http://jellyfin:8096)")
	} else if u, err := url.Parse(c.url); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems, fmt.Sprintf("JELLYFIN_URL must be a full URL starting with http:// or https://, got %q", c.url))
	}
	if c.apiKey == "" {
		problems = append(problems, "JELLYFIN_API_KEY is required (create one in Jellyfin: Dashboard → API Keys)")
	}
	if c.user == "" {
		problems = append(problems, "JELLYFIN_USER is required (the Jellyfin username whose history to show)")
	}
	if m, err := strconv.Atoi(envOr("REFRESH_MINUTES", "30")); err != nil || m < 1 {
		problems = append(problems, "REFRESH_MINUTES must be a whole number of minutes, 1 or more")
	} else {
		c.refresh = time.Duration(m) * time.Minute
	}
	if p, err := strconv.Atoi(c.port); err != nil || p < 1 || p > 65535 {
		problems = append(problems, fmt.Sprintf("PORT must be a number between 1 and 65535, got %q", c.port))
	}
	return c, problems
}

// explain turns an error from talking to Jellyfin into advice, and reports
// whether the problem might fix itself (so it is worth retrying later).
func explain(err error, c config) (msg string, transient bool) {
	var ae *apiError
	var nf *notFoundError
	switch {
	case errors.As(err, &nf):
		return nf.Error(), false
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden):
		return fmt.Sprintf("Jellyfin rejected the API key (HTTP %d). Check JELLYFIN_API_KEY.", ae.Status), false
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound,
		errors.Is(err, errBadResponse):
		return fmt.Sprintf("%s does not look like a Jellyfin server (%v). Check JELLYFIN_URL, including any base path such as /jellyfin.", c.url, err), false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // drop the long request URL, keep the cause
	}
	return fmt.Sprintf("cannot reach Jellyfin at %s: %v", c.url, err), true
}

type app struct {
	cfg config
	jf  *jellyfin
	tr  *trakt

	syncMu     sync.Mutex // serialises syncs and imports; guards userID and excludeIDs
	userID     string
	excludeIDs []string

	mu        sync.RWMutex // guards everything below
	st        state        // Jellyfin data
	imported  traktImport  // Trakt data
	items     []entry      // both merged, as shown on the page
	syncErr   string
	importErr string

	marksMu sync.Mutex      // serialises rating and note writes
	marks   map[string]mark // ratings and notes by item ID; replaced, never modified in place
}

func (a *app) statePath() string  { return filepath.Join(a.cfg.dataDir, "state.json") }
func (a *app) traktPath() string  { return filepath.Join(a.cfg.dataDir, "trakt.json") }
func (a *app) postersDir() string { return filepath.Join(a.cfg.dataDir, "posters") }
func (a *app) marksPath() string  { return filepath.Join(a.cfg.dataDir, "marks.json") }

// connect resolves the configured user and excluded libraries to Jellyfin IDs.
// The caller must hold syncMu.
func (a *app) connect(ctx context.Context) error {
	id, err := a.jf.userID(ctx, a.cfg.user)
	if err != nil {
		return err
	}
	libs, err := a.jf.libraryIDs(ctx, id, a.cfg.excludeLibs)
	if err != nil {
		return err
	}
	a.userID, a.excludeIDs = id, libs
	return nil
}

func (a *app) poster(ctx context.Context, e entry) (io.ReadCloser, error) {
	if e.PosterURL != "" {
		return a.tr.image(ctx, e.PosterURL)
	}
	return a.jf.poster(ctx, e.ID, e.PosterTag)
}

// refresh syncs with Jellyfin (and Trakt if linked) unless a sync just succeeded. Failures keep the cached data.
func (a *app) refresh(ctx context.Context) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.mu.RLock()
	fresh := time.Since(a.st.LastSync) < 2*time.Second
	a.mu.RUnlock()
	if fresh {
		return // e.g. a double-clicked Refresh button
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	start := time.Now()
	err := a.sync(ctx)
	if err != nil {
		msg, _ := explain(err, a.cfg)
		a.mu.Lock()
		a.syncErr = msg
		a.mu.Unlock()
		slog.Error("sync failed", "err", msg)
		return
	}
	a.mu.Lock()
	a.syncErr = ""
	a.mu.Unlock()
	movies, shows := counts(a.items)
	slog.Info("sync complete", "movies", movies, "shows", shows, "took", time.Since(start).Round(time.Millisecond))
}

// sync fetches from Jellyfin (and refreshes Trakt if a profile was imported) and publishes the result. The caller must hold syncMu.
func (a *app) sync(ctx context.Context) error {
	if a.userID == "" {
		if err := a.connect(ctx); err != nil {
			return err
		}
	}
	items, err := fetchWatched(ctx, a.jf, a.userID, a.excludeIDs)
	if err != nil {
		return err
	}
	st := state{User: a.cfg.user, LastSync: time.Now().UTC(), Items: items}
	imp := a.imported
	if imp.User != "" && a.tr.clientID != "" {
		if trItems, err := a.tr.watched(ctx, imp.User); err != nil {
			slog.Warn("trakt refresh failed during sync", "user", imp.User, "err", err)
		} else {
			imp = traktImport{User: imp.User, ImportedAt: time.Now().UTC(), Items: trItems}
			if err := saveJSON(a.traktPath(), imp); err != nil {
				slog.Error("could not save trakt import", "err", err)
			}
		}
	}
	if err := a.publish(ctx, st, imp); err != nil {
		return err
	}
	if err := saveJSON(a.statePath(), st); err != nil {
		slog.Error("could not save state", "err", err)
	}
	return nil
}

// publish merges Jellyfin and Trakt data, fetches the posters it needs, and
// shows the result. The caller must hold syncMu.
func (a *app) publish(ctx context.Context, st state, imp traktImport) error {
	items := merge(st.Items, imp.Items)
	if err := syncPosters(ctx, a.poster, a.postersDir(), items); err != nil {
		return err
	}
	a.mu.Lock()
	a.st, a.imported, a.items = st, imp, items
	a.mu.Unlock()
	return nil
}

// importTrakt replaces the Trakt import with the history of the given public profile.
func (a *app) importTrakt(ctx context.Context, link string) error {
	if a.tr.clientID == "" {
		return errors.New("Trakt import is turned off. Set TRAKT_CLIENT_ID to enable it.")
	}
	user, err := traktUser(link)
	if err != nil {
		return err
	}
	items, err := a.tr.watched(ctx, user)
	if err != nil {
		return err
	}
	imp := traktImport{User: user, ImportedAt: time.Now().UTC(), Items: items}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.publish(ctx, a.st, imp); err != nil {
		return err
	}
	if err := saveJSON(a.traktPath(), imp); err != nil {
		return fmt.Errorf("could not save the import: %w", err)
	}
	movies, shows := counts(items)
	slog.Info("trakt import complete", "user", user, "movies", movies, "shows", shows)
	return nil
}

func (a *app) loop(ctx context.Context) {
	t := time.NewTicker(a.cfg.refresh)
	defer t.Stop()
	for {
		a.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func counts(items []entry) (movies, shows int) {
	for _, e := range items {
		if e.Type == "movie" {
			movies++
		} else {
			shows++
		}
	}
	return
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	unit := func(n int, s string) string {
		if n != 1 {
			s += "s"
		}
		return fmt.Sprintf("%d %s ago", n, s)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return unit(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return unit(int(d.Hours()), "hour")
	default:
		return unit(int(d.Hours()/24), "day")
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		st, imp, items, syncErr, importErr := a.st, a.imported, a.items, a.syncErr, a.importErr
		a.mu.RUnlock()
		movies, shows := counts(items)
		var buf bytes.Buffer
		tmpl := page
		if b, err := os.ReadFile("index.html"); err == nil {
			if parsed, err := template.New("index").Funcs(tmplFuncs).Parse(string(b)); err == nil {
				tmpl = parsed
			}
		}
		err := tmpl.Execute(&buf, map[string]any{
			"Items": items, "Movies": movies, "Shows": shows,
			"Synced": ago(st.LastSync), "Never": st.LastSync.IsZero() && len(items) == 0, "SyncErr": syncErr,
			"Trakt": a.tr.clientID != "", "ImportUser": imp.User, "ImportCount": len(imp.Items),
			"ImportAgo": ago(imp.ImportedAt), "ImportErr": importErr,
			"JellyfinURL": a.cfg.url, "SeerrURL": a.cfg.seerrURL, "User": a.cfg.user,
			"Marks": pageMarks(a.marksSnapshot()),
		})
		if err != nil {
			slog.Error("render failed", "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
	})
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		st, imp, items, syncErr := a.st, a.imported, a.items, a.syncErr
		a.mu.RUnlock()
		movies, shows := counts(items)
		type itemDTO struct {
			entry
			Poster    string `json:"poster"`
			Completed bool   `json:"completed"`
		}
		dtos := make([]itemDTO, len(items))
		for i, e := range items {
			dtos[i] = itemDTO{
				entry:     e,
				Poster:    e.Poster(),
				Completed: e.Completed(),
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"user":        a.cfg.user,
			"jellyfinUrl": a.cfg.url,
			"synced":      ago(st.LastSync),
			"syncErr":     syncErr,
			"movies":      movies,
			"shows":       shows,
			"traktUser":   imp.User,
			"items":       dtos,
		})
	})
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		a.refresh(context.WithoutCancel(r.Context()))
		// Relative redirect so Watched also works behind a reverse proxy sub-path.
		w.Header().Set("Location", "./")
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("POST /import-trakt", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // a profile link is tiny
		err := a.importTrakt(ctx, r.FormValue("profile"))
		a.mu.Lock()
		a.importErr = ""
		if err != nil {
			a.importErr = err.Error()
		}
		a.mu.Unlock()
		if err != nil {
			slog.Warn("trakt import failed", "err", err)
		}
		w.Header().Set("Location", "./")
		w.WriteHeader(http.StatusSeeOther)
	})
	a.markRoutes(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /posters/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") ||
			!strings.HasSuffix(name, ".jpg") && !strings.HasSuffix(name, ".webp") {
			http.NotFound(w, r)
			return
		}
		// File names include the image's tag, so a changed poster gets a new URL.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, filepath.Join(a.postersDir(), name))
	})
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(name) {
		case ".woff2":
			w.Header().Set("Content-Type", "font/woff2")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		// Like index.html, prefer a copy on disk so the artwork can be swapped without a rebuild.
		if fi, err := os.Stat(filepath.Join("assets", name)); err == nil && !fi.IsDir() {
			http.ServeFile(w, r, filepath.Join("assets", name))
			return
		}
		http.ServeFileFS(w, r, assetsFS, "assets/"+name)
	})
	// Reject form posts made by other websites (CSRF); same-origin and non-browser requests pass.
	return http.NewCrossOriginProtection().Handler(mux)
}

// runCheck is the -check flag: verify the connection and print what was found.
func runCheck(a *app) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fmt.Printf("Connecting to %s as %q...\n", a.cfg.url, a.cfg.user)
	err := a.connect(ctx)
	if err == nil {
		var items []entry
		if items, err = fetchWatched(ctx, a.jf, a.userID, a.excludeIDs); err == nil {
			movies, shows := counts(items)
			completed := 0
			for _, e := range items {
				if e.Completed() {
					completed++
				}
			}
			fmt.Printf("OK\nWatched movies: %d\nWatched shows:  %d (%d completed)\n", movies, shows, completed)
			return 0
		}
	}
	msg, _ := explain(err, a.cfg)
	fmt.Fprintln(os.Stderr, "FAILED:", msg)
	return 1
}

// runHealthcheck is the -healthcheck flag, used by Docker since distroless has no curl.
func runHealthcheck() int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + envOr("PORT", "8080") + "/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	check := flag.Bool("check", false, "test the Jellyfin connection, print how many watched movies and shows were found, and exit")
	health := flag.Bool("healthcheck", false, "exit 0 if the local server's /health endpoint responds (for Docker HEALTHCHECK)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if *health {
		os.Exit(runHealthcheck())
	}
	cfg, problems := loadConfig()
	if len(problems) > 0 {
		fmt.Fprintln(os.Stderr, "watched: configuration problem:\n  - "+strings.Join(problems, "\n  - "))
		os.Exit(2)
	}
	a := &app{cfg: cfg, jf: newJellyfin(cfg.url, cfg.apiKey), tr: newTrakt(cfg.traktClientID)}
	if *check {
		os.Exit(runCheck(a))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(a.postersDir(), 0o700); err != nil {
		slog.Error("data directory is not writable; mount a writable volume at "+cfg.dataDir, "err", err)
		os.Exit(1)
	}
	var st state
	if err := loadJSON(a.statePath(), &st); err != nil {
		slog.Warn("ignoring unreadable state file", "err", err)
	} else if st.User == cfg.user {
		a.st = st
	}
	if err := loadJSON(a.traktPath(), &a.imported); err != nil {
		slog.Warn("ignoring unreadable Trakt import", "err", err)
	}
	a.items = merge(a.st.Items, a.imported.Items)
	if err := loadJSON(a.marksPath(), &a.marks); err != nil {
		// Don't start with an empty set: the next save would overwrite the user's notes.
		slog.Error("marks.json is unreadable; fix or move it, then restart", "err", err)
		os.Exit(1)
	}

	// Serve (cached data) straight away, even if Jellyfin is slow to answer.
	ln, err := net.Listen("tcp", ":"+cfg.port)
	if err != nil {
		slog.Error("cannot listen", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("watched started", "addr", ln.Addr().String(), "jellyfin", cfg.url, "user", cfg.user, "refresh", cfg.refresh)

	// Resolve the user and libraries up front so a bad key or name fails fast.
	// If Jellyfin is merely unreachable, keep serving cached data and retry each refresh.
	a.syncMu.Lock()
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = a.connect(startCtx)
	cancel()
	a.syncMu.Unlock()
	if err != nil {
		msg, transient := explain(err, cfg)
		if !transient {
			slog.Error(msg)
			os.Exit(1)
		}
		slog.Warn(msg + "; serving cached data and retrying")
	}

	a.loop(ctx) // returns on SIGINT/SIGTERM
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
