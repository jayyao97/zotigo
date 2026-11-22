package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	// Clients choose any IANA zone; do not depend on the host's zoneinfo.
	_ "time/tzdata"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	usageQueryVersion   = 1
	usageMaxRangeDays   = 400
	usageDefaultLimit   = 50
	usageMaxLimit       = 200
	usageRequestTimeout = 10 * time.Second
)

type usageStore interface {
	UsageActivity(context.Context, zotigosession.UsageFilter) (zotigosession.UsageActivity, error)
	UsageSessions(context.Context, zotigosession.UsageFilter, string, int) (zotigosession.UsageSessionPage, error)
}

type usageActivityResponse struct {
	QueryVersion int `json:"usage_query_version"`
	zotigosession.UsageActivity
}

type usageSessionsResponse struct {
	QueryVersion int `json:"usage_query_version"`
	zotigosession.UsageSessionPage
}

func (h *handler) handleUsageActivity(w http.ResponseWriter, r *http.Request) {
	store, filter, ok := h.usageRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), usageRequestTimeout)
	defer cancel()
	activity, err := store.UsageActivity(ctx, filter)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, usageActivityResponse{QueryVersion: usageQueryVersion, UsageActivity: activity})
}

func (h *handler) handleUsageSessions(w http.ResponseWriter, r *http.Request) {
	store, filter, ok := h.usageRequest(w, r)
	if !ok {
		return
	}
	limit := usageDefaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > usageMaxLimit {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", usageMaxLimit))
			return
		}
		limit = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), usageRequestTimeout)
	defer cancel()
	page, err := store.UsageSessions(ctx, filter, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, usageSessionsResponse{QueryVersion: usageQueryVersion, UsageSessionPage: page})
}

func (h *handler) usageRequest(w http.ResponseWriter, r *http.Request) (usageStore, zotigosession.UsageFilter, bool) {
	store, ok := h.store.(usageStore)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable, "session storage unavailable")
		return nil, zotigosession.UsageFilter{}, false
	}
	filter, err := parseUsageFilter(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return nil, zotigosession.UsageFilter{}, false
	}
	return store, filter, true
}

func parseUsageFilter(query url.Values) (zotigosession.UsageFilter, error) {
	zone := strings.TrimSpace(query.Get("tz"))
	if zone == "" {
		return zotigosession.UsageFilter{}, errors.New("tz is required")
	}
	// "Local" would silently use the daemon host's zone instead of the client's.
	if zone == "Local" {
		return zotigosession.UsageFilter{}, errors.New("tz must be an IANA time zone name")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return zotigosession.UsageFilter{}, fmt.Errorf("tz %q is not a known IANA time zone", zone)
	}
	// Dates stay calendar dates; the store finds where each day begins in the zone.
	from, err := time.Parse(time.DateOnly, query.Get("from"))
	if err != nil {
		return zotigosession.UsageFilter{}, errors.New("from must be a YYYY-MM-DD date")
	}
	to, err := time.Parse(time.DateOnly, query.Get("to"))
	if err != nil {
		return zotigosession.UsageFilter{}, errors.New("to must be a YYYY-MM-DD date")
	}
	// Count civil days so a daylight-saving transition does not change the span.
	span := to.Sub(from)
	days := int(span.Hours()/24) + 1
	if days < 1 || days > usageMaxRangeDays {
		return zotigosession.UsageFilter{}, fmt.Errorf("from and to must span 1 to %d days", usageMaxRangeDays)
	}
	channel := zotigosession.UsageChannelFilter(query.Get("channel"))
	switch channel {
	case "":
		channel = zotigosession.UsageChannelAll
	case zotigosession.UsageChannelAll, zotigosession.UsageChannelOnly, zotigosession.UsageChannelExclude:
	default:
		return zotigosession.UsageFilter{}, errors.New("channel must be all, only or exclude")
	}
	return zotigosession.UsageFilter{Location: location, From: from, To: to, Model: query.Get("model"), Channel: channel}, nil
}

func writeUsageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, zotigosession.ErrInvalidUsageCursor):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, err.Error())
	}
}
