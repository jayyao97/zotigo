package zotigod

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

type catalogSessionCursor struct {
	Position int64  `json:"position"`
	At       int64  `json:"at"`
	ID       string `json:"id"`
}

func (h *handler) handleCatalogSessionPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	invalid := func() { writeAPIError(w, http.StatusBadRequest, "invalid session query") }
	limit := 50
	if q.Has("limit") {
		value, err := strconv.Atoi(q.Get("limit"))
		if err != nil || value < 1 || value > 200 {
			invalid()
			return
		}
		limit = value
	}
	filter := zotigoworkspace.SessionQuery{ProjectID: q.Get("project_id"), WorkspaceID: q.Get("workspace_id"), Search: q.Get("q"), ChannelMode: q.Get("channel"), Limit: limit + 1}
	if filter.ChannelMode == "" {
		filter.ChannelMode = "all"
	}
	if filter.ChannelMode != "all" && filter.ChannelMode != "none" && filter.ChannelMode != "pinned" {
		invalid()
		return
	}
	if len(filter.Search) > 500 {
		invalid()
		return
	}
	for name, target := range map[string]*bool{"ungrouped": &filter.Ungrouped, "pinned": &filter.Pinned, "include_archived": &filter.IncludeArchived} {
		if q.Has(name) {
			value, err := strconv.ParseBool(q.Get(name))
			if err != nil {
				invalid()
				return
			}
			*target = value
		}
	}
	if q.Has("unread") {
		value, err := strconv.ParseBool(q.Get("unread"))
		if err != nil {
			invalid()
			return
		}
		filter.Unread = &value
	}
	if q.Has("since") {
		value, err := time.Parse(time.RFC3339Nano, q.Get("since"))
		if err != nil {
			invalid()
			return
		}
		filter.Since = value.UnixMilli()
	}
	if q.Has("cursor") {
		raw, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err != nil || len(raw) > 512 {
			invalid()
			return
		}
		var cursor catalogSessionCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.ID == "" || cursor.At < 0 {
			invalid()
			return
		}
		filter.Before, filter.BeforeID, filter.BeforePosition = cursor.At, cursor.ID, cursor.Position
	}
	if q.Has("ids") {
		filter.IDs = strings.Split(q.Get("ids"), ",")
		if len(filter.IDs) > 200 {
			invalid()
			return
		}
	}
	if filter.ChannelMode != "all" && h.channels != nil {
		ids, err := h.channels.BoundSessionIDs(r.Context())
		if err != nil {
			h.writeChannelError(w, err)
			return
		}
		filter.ChannelIDs = ids
	}
	diagnostics := h.codexSyncDiagnostics(r.Context(), q.Get("sync_codex") == "true")
	rows, err := h.catalog.QuerySessions(r.Context(), filter)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		raw, _ := json.Marshal(catalogSessionCursor{At: last.UpdatedAt, ID: last.ID, Position: last.Position})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	projections := make([]sessionProjection, 0, len(rows))
	// The page path reads only metadata; store.Get would deserialize snapshots.
	if h.store != nil && len(ids) > 0 {
		metadata, err := h.store.List(r.Context(), zotigosession.ListFilter{IDs: ids})
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		byID := make(map[string]zotigosession.Metadata, len(metadata))
		for _, meta := range metadata {
			byID[meta.ID] = meta
		}
		for _, row := range rows {
			meta, ok := byID[row.ID]
			if !ok {
				continue
			}
			runtime := sessionFromMetadata(meta, SessionStateOffline, false)
			if live, ok := h.registry.Get(row.ID); ok {
				runtime = live
				runtime.Live = true
				runtime = withSessionMetadata(runtime, meta)
			}
			if store, ok := h.store.(sessionReadStore); ok {
				state, err := store.SessionReadState(r.Context(), row.ID)
				if err != nil {
					h.writeCatalogError(w, err)
					return
				}
				runtime.ReadState = &state
			}
			organization, err := h.catalog.GetSessionOrganization(r.Context(), row.ID)
			var org *zotigoworkspace.SessionOrganization
			if err == nil {
				org = &organization
			} else if !errors.Is(err, zotigoworkspace.ErrNotFound) {
				h.writeCatalogError(w, err)
				return
			}
			availability, err := h.sessionAvailability(r.Context(), &runtime, org)
			if err != nil {
				h.writeCatalogError(w, err)
				return
			}
			projections = append(projections, sessionProjection{Runtime: &runtime, Organization: org, Availability: availability})
		}
	}
	writeAPIJSON(w, http.StatusOK, struct {
		Sessions     []sessionProjection     `json:"sessions"`
		NextCursor   string                  `json:"next_cursor,omitempty"`
		QueryVersion int                     `json:"query_version"`
		Diagnostics  []sessionSyncDiagnostic `json:"diagnostics,omitempty"`
	}{projections, next, 1, diagnostics})
}
