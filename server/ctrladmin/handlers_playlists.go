package ctrladmin

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.senan.xyz/gonic/db"
	"go.senan.xyz/gonic/fileutil"
	"go.senan.xyz/gonic/playlist"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specid"
	"go.senan.xyz/gonic/server/ctrlsubsonic/specidpaths"
)

type PlaylistListItem struct {
	Playlist *playlist.Playlist
	RelPath  string
}

type PlaylistItem struct {
	Index   int
	Path    string
	Track   *db.Track
	Missing bool
}

type TrackResult struct {
	Track   *db.Track
	AbsPath string
}

func ownedPath(user *db.User, relPath string) bool {
	first, _, found := strings.Cut(relPath, "/")
	return found && first == strconv.Itoa(user.ID)
}

func (c *Controller) ServeGetPlaylists(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)

	relPaths, err := c.playlistStore.ListByUser(user.ID)
	if err != nil {
		return &Response{code: http.StatusInternalServerError, err: fmt.Sprintf("listing playlists: %v", err)}
	}

	var playlists []PlaylistListItem
	for _, relPath := range relPaths {
		pl, err := c.playlistStore.Read(relPath)
		if err != nil {
			continue
		}
		playlists = append(playlists, PlaylistListItem{Playlist: pl, RelPath: relPath})
	}

	return &Response{
		template: "playlists.tmpl",
		data:     &templateData{Playlists: playlists},
	}
}

func (c *Controller) ServeGetPlaylist(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.URL.Query().Get("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}

	// First pass: resolve IDs (one Lookup query per item, unavoidable due to path→ID mapping)
	ids := make([]*specid.ID, len(pl.Items))
	missing := make([]bool, len(pl.Items))
	for i, path := range pl.Items {
		id, err := specidpaths.Lookup(c.dbc, c.musicPaths, c.podcastsPath, path)
		if err != nil {
			missing[i] = true
		} else {
			ids[i] = id
		}
	}

	// Batch-load tracks in a single query instead of one Locate call per item
	var trackIDs []int
	for _, id := range ids {
		if id != nil && id.Type == specid.Track {
			trackIDs = append(trackIDs, id.Value)
		}
	}
	trackByID := make(map[int]*db.Track, len(trackIDs))
	if len(trackIDs) > 0 {
		var tracks []*db.Track
		c.dbc.Preload("Album").Where("id IN (?)", trackIDs).Find(&tracks)
		for _, t := range tracks {
			trackByID[t.ID] = t
		}
	}

	items := make([]PlaylistItem, len(pl.Items))
	for i, path := range pl.Items {
		items[i] = PlaylistItem{Index: i, Path: path, Missing: missing[i]}
		if id := ids[i]; id != nil && id.Type == specid.Track {
			items[i].Track = trackByID[id.Value]
		}
	}

	return &Response{
		template: "playlist.tmpl",
		data: &templateData{
			Playlist:      pl,
			PlaylistItems: items,
			PlaylistPath:  relPath,
			PlaylistRaw:   strings.Join(pl.Items, "\n"),
		},
	}
}

func (c *Controller) ServeGetDeletePlaylist(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.URL.Query().Get("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}
	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}
	return &Response{
		template: "playlist_delete.tmpl",
		data: &templateData{
			Playlist:     pl,
			PlaylistPath: relPath,
		},
	}
}

func (c *Controller) ServeDeletePlaylist(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.FormValue("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	if err := c.playlistStore.Delete(relPath); err != nil {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{fmt.Sprintf("error deleting playlist: %v", err)},
		}
	}
	return &Response{
		redirect: "/admin/playlists",
		flashN:   []string{"playlist deleted"},
	}
}

func (c *Controller) ServeRawEditPlaylist(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.FormValue("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}

	items, err := parseM3U(strings.NewReader(r.FormValue("raw")))
	if err != nil {
		return &Response{
			redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
			flashW:   []string{fmt.Sprintf("error parsing playlist: %v", err)},
		}
	}
	pl.Items = items
	pl.UpdatedAt = time.Now()
	if err := c.playlistStore.Write(relPath, pl); err != nil {
		return &Response{
			redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
			flashW:   []string{fmt.Sprintf("error saving playlist: %v", err)},
		}
	}
	return &Response{
		redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
		flashN:   []string{"playlist updated"},
	}
}

func (c *Controller) ServeDeleteEntry(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.FormValue("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	indexStr := r.FormValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		return &Response{code: http.StatusBadRequest, err: "invalid index"}
	}

	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}

	if index < 0 || index >= len(pl.Items) {
		return &Response{code: http.StatusBadRequest, err: "index out of range"}
	}

	pl.Items = append(pl.Items[:index], pl.Items[index+1:]...)
	pl.UpdatedAt = time.Now()
	if err := c.playlistStore.Write(relPath, pl); err != nil {
		return &Response{
			redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
			flashW:   []string{fmt.Sprintf("error saving playlist: %v", err)},
		}
	}
	return &Response{redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath)}
}

func (c *Controller) ServeReplaceEntry(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.FormValue("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	indexStr := r.FormValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		return &Response{code: http.StatusBadRequest, err: "invalid index"}
	}

	trackPath := r.FormValue("track_path")
	if trackPath == "" {
		return &Response{code: http.StatusBadRequest, err: "missing track_path"}
	}

	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}

	if index < 0 || index >= len(pl.Items) {
		return &Response{code: http.StatusBadRequest, err: "index out of range"}
	}

	pl.Items[index] = trackPath
	pl.UpdatedAt = time.Now()
	if err := c.playlistStore.Write(relPath, pl); err != nil {
		return &Response{
			redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
			flashW:   []string{fmt.Sprintf("error saving playlist: %v", err)},
		}
	}
	return &Response{
		redirect: fmt.Sprintf("/admin/playlist?path=%s", relPath),
		flashN:   []string{"entry replaced"},
	}
}

func (c *Controller) ServePlaylistSearch(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.URL.Query().Get("path")
	if !ownedPath(user, relPath) {
		return &Response{code: http.StatusForbidden, err: "forbidden"}
	}

	pl, err := c.playlistStore.Read(relPath)
	if err != nil {
		return &Response{code: http.StatusNotFound, err: fmt.Sprintf("reading playlist: %v", err)}
	}

	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 || index >= len(pl.Items) {
		return &Response{code: http.StatusBadRequest, err: "invalid index"}
	}

	query := r.URL.Query().Get("q")
	data := &templateData{
		Playlist:     pl,
		PlaylistPath: relPath,
		EntryIndex:   index,
		SearchQuery:  query,
	}

	if query != "" {
		q := strings.TrimSpace(query)
		q = strings.ReplaceAll(q, `\`, `\\`)
		q = strings.ReplaceAll(q, "%", `\%`)
		q = strings.ReplaceAll(q, "_", `\_`)
		q = strings.ReplaceAll(q, "*", "%")
		if !strings.Contains(q, "%") {
			q = "%" + q + "%"
		}
		var tracks []*db.Track
		c.dbc.
			Preload("Album").
			Where("tag_title LIKE ? ESCAPE '\\' OR filename LIKE ? ESCAPE '\\'", q, q).
			Limit(50).
			Find(&tracks)
		results := make([]TrackResult, 0, len(tracks))
		for _, t := range tracks {
			results = append(results, TrackResult{Track: t, AbsPath: t.AbsPath()})
		}
		data.TrackResults = results
	}

	return &Response{template: "playlist_search.tmpl", data: data}
}

func (c *Controller) ServePlaylistDownload(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(CtxUser).(*db.User)
	relPath := r.URL.Query().Get("path")
	if !ownedPath(user, relPath) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	absPath, err := fileutil.SafeJoin(c.playlistStore.BasePath(), relPath)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := filepath.Base(relPath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Type", "audio/mpegurl")
	http.ServeFile(w, r, absPath)
}

func (c *Controller) ServePlaylistUpload(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{"could not parse upload"},
		}
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{"please enter a playlist name"},
		}
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{"please select a file to upload"},
		}
	}
	defer file.Close()

	pl := &playlist.Playlist{
		Name:      name,
		UserID:    user.ID,
		UpdatedAt: time.Now(),
	}
	items, err := parseM3U(file)
	if err != nil {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{fmt.Sprintf("error parsing uploaded file: %v", err)},
		}
	}
	pl.Items = items

	relPath := playlist.NewPath(user.ID, name)
	if err := c.playlistStore.Write(relPath, pl); err != nil {
		return &Response{
			redirect: "/admin/playlists",
			flashW:   []string{fmt.Sprintf("error saving playlist: %v", err)},
		}
	}
	return &Response{
		redirect: "/admin/playlists",
		flashN:   []string{fmt.Sprintf("playlist %q uploaded", name)},
	}
}

func parseM3U(r io.Reader) ([]string, error) {
	var items []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		items = append(items, line)
	}
	return items, sc.Err()
}
