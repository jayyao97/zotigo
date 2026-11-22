package session

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

// TurnUsageRecord is the provider-reported usage of one finished turn.
// Subagent usage is already part of the parent turn's usage.
type TurnUsageRecord struct {
	SessionID   string
	TurnID      string
	Runtime     string
	Model       string
	Status      string
	Channel     bool
	CompletedAt time.Time
	Usage       protocol.Usage
}

// IsChannelRequest reports whether a turn was triggered from an IM channel.
// Completion triggers also carry a request context but no connection.
func IsChannelRequest(requestContext *protocol.RequestContext) bool {
	return requestContext != nil && requestContext.ConnectionID != ""
}

// RecordTurnUsage stores the usage of one turn. A turn without reported usage
// is skipped rather than recorded as zero. Retries overwrite the same turn.
func (s *FileStore) RecordTurnUsage(ctx context.Context, record TurnUsageRecord) error {
	usage := record.Usage.Normalized()
	if usage == (protocol.Usage{}) {
		return nil
	}
	if record.SessionID == "" || record.TurnID == "" {
		return errors.New("record turn usage: session and turn are required")
	}
	channel := 0
	if record.Channel {
		channel = 1
	}
	completed := formatIndexTime(record.CompletedAt)
	tx, err := s.index.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record turn usage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A retried turn replaces its earlier row, so remove that row's share of
	// the rollup first.
	var previous struct {
		completed                                   int64
		model, runtime                              string
		channel                                     int
		input, cacheRead, cacheWrite, output, total int64
	}
	err = tx.QueryRowContext(ctx, `SELECT completed_at, model, runtime, is_channel, input_tokens, cache_read_tokens,
		cache_write_tokens, output_tokens, total_tokens FROM turn_usage WHERE session_id = ? AND turn_id = ?`, record.SessionID, record.TurnID).
		Scan(&previous.completed, &previous.model, &previous.runtime, &previous.channel, &previous.input, &previous.cacheRead,
			&previous.cacheWrite, &previous.output, &previous.total)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE turn_usage_buckets SET turns = turns - 1, input_tokens = input_tokens - ?,
			cache_read_tokens = cache_read_tokens - ?, cache_write_tokens = cache_write_tokens - ?, output_tokens = output_tokens - ?,
			total_tokens = total_tokens - ? WHERE bucket = ? AND model = ? AND runtime = ? AND is_channel = ?`,
			previous.input, previous.cacheRead, previous.cacheWrite, previous.output, previous.total,
			previous.completed/usageBucketNanos, previous.model, previous.runtime, previous.channel); err != nil {
			return fmt.Errorf("record turn usage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM turn_usage_buckets WHERE turns <= 0 AND bucket = ?`, previous.completed/usageBucketNanos); err != nil {
			return fmt.Errorf("record turn usage: %w", err)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("record turn usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turn_usage (session_id, turn_id, runtime, model, status, is_channel, completed_at,
			input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, total_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, turn_id) DO UPDATE SET
			runtime = excluded.runtime, model = excluded.model, status = excluded.status,
			is_channel = excluded.is_channel, completed_at = excluded.completed_at,
			input_tokens = excluded.input_tokens, cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens, output_tokens = excluded.output_tokens,
			total_tokens = excluded.total_tokens
	`, record.SessionID, record.TurnID, record.Runtime, record.Model, record.Status, channel, completed,
		usage.InputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, usage.OutputTokens, usage.TotalTokens); err != nil {
		return fmt.Errorf("record turn usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turn_usage_buckets (bucket, model, runtime, is_channel, turns,
			input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, total_tokens)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, model, runtime, is_channel) DO UPDATE SET
			turns = turns + 1, input_tokens = input_tokens + excluded.input_tokens,
			cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
			cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
			output_tokens = output_tokens + excluded.output_tokens, total_tokens = total_tokens + excluded.total_tokens
	`, completed/usageBucketNanos, record.Model, record.Runtime, channel,
		usage.InputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, usage.OutputTokens, usage.TotalTokens); err != nil {
		return fmt.Errorf("record turn usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record turn usage: %w", err)
	}
	return nil
}

// UsageChannelFilter selects turns by origin.
type UsageChannelFilter string

const (
	UsageChannelAll     UsageChannelFilter = "all"
	UsageChannelOnly    UsageChannelFilter = "only"
	UsageChannelExclude UsageChannelFilter = "exclude"
)

// UsageFilter selects usage by local calendar days in Location.
// From and To are inclusive calendar dates: only their own year, month and
// day are used, so parse them without a location (time.Parse). Parsing in
// Location can move a date whose midnight does not exist there.
type UsageFilter struct {
	Location *time.Location
	From     time.Time
	To       time.Time
	Model    string
	Channel  UsageChannelFilter
	// Now anchors the current streak; zero means time.Now.
	Now time.Time
}

// UsageTotals is a token breakdown. CacheInput is the full input of models
// that had any cache read in the queried range, the denominator of the cache
// hit rate. Models whose provider never reported cache reads are excluded so
// they do not read as a 0% hit rate.
type UsageTotals struct {
	Total      int64 `json:"total"`
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	CacheInput int64 `json:"cache_input"`
	Turns      int64 `json:"turns"`
}

type UsageDay struct {
	Date string `json:"date"`
	UsageTotals
}

type UsageModel struct {
	Model   string `json:"model"`
	Runtime string `json:"runtime"`
	UsageTotals
}

type UsagePeakDay struct {
	Date  string `json:"date"`
	Total int64  `json:"total"`
}

type UsageActivity struct {
	TrackingSince     *time.Time    `json:"tracking_since"`
	Range             UsageTotals   `json:"range"`
	AllTime           UsageTotals   `json:"all_time"`
	ByModel           []UsageModel  `json:"by_model"`
	Days              []UsageDay    `json:"days"`
	PeakDay           *UsagePeakDay `json:"peak_day"`
	CurrentStreakDays int           `json:"current_streak_days"`
	LongestStreakDays int           `json:"longest_streak_days"`
}

type UsageSession struct {
	SessionID       string    `json:"session_id"`
	Models          []string  `json:"models"`
	ChannelTurns    int64     `json:"channel_turns"`
	LastCompletedAt time.Time `json:"last_completed_at"`
	UsageTotals
}

type UsageSessionPage struct {
	Sessions   []UsageSession `json:"sessions"`
	NextCursor string         `json:"next_cursor"`
}

// ErrInvalidUsageCursor reports a cursor that this store did not issue.
var ErrInvalidUsageCursor = errors.New("invalid usage cursor")

// usageBucket is one aggregate row: a 15-minute UTC rollup for activity, or
// one session and model for rankings. Every current IANA offset and
// transition is a multiple of 15 minutes, so each rollup bucket falls inside
// exactly one local day in any zone.
type usageBucket struct {
	sessionID string
	bucket    int64
	model     string
	runtime   string
	channel   int64
	last      int64
	tokens    UsageTotals
}

const usageBucketNanos = int64(15 * time.Minute)

func (bucket usageBucket) fullInput() int64 {
	return bucket.tokens.Input + bucket.tokens.CacheRead + bucket.tokens.CacheWrite
}

func (totals *UsageTotals) add(other UsageTotals) {
	totals.Total += other.Total
	totals.Input += other.Input
	totals.CacheRead += other.CacheRead
	totals.CacheWrite += other.CacheWrite
	totals.Output += other.Output
	totals.CacheInput += other.CacheInput
	totals.Turns += other.Turns
}

func (filter UsageFilter) bounds() (int64, int64) {
	from := localDayStart(filter.From.Year(), filter.From.Month(), filter.From.Day(), filter.Location)
	to := localDayStart(filter.To.Year(), filter.To.Month(), filter.To.Day()+1, filter.Location)
	return formatIndexTime(from), formatIndexTime(to)
}

// localDayStart returns the first instant of a local calendar day. Where a
// daylight-saving change skips midnight (America/Santiago in September),
// time.Date may return an instant on the previous day or after the skipped
// hour; the day then starts at the zone transition.
func localDayStart(year int, month time.Month, day int, location *time.Location) time.Time {
	start := time.Date(year, month, day, 0, 0, 0, 0, location)
	want := time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	switch local := start.In(location); {
	case local.Format(time.DateOnly) < want:
		_, end := start.ZoneBounds()
		return end
	case local.Hour() != 0 || local.Minute() != 0:
		zoneStart, _ := start.ZoneBounds()
		return zoneStart
	}
	return start
}

func usageFilterSQL(filter UsageFilter, query *strings.Builder, args []any) []any {
	if filter.Model != "" {
		query.WriteString(` AND model = ?`)
		args = append(args, filter.Model)
	}
	switch filter.Channel {
	case UsageChannelOnly:
		query.WriteString(` AND is_channel = 1`)
	case UsageChannelExclude:
		query.WriteString(` AND is_channel = 0`)
	}
	return args
}

// Reads use the read-only pool so long scans never hold the writer.
func (s *FileStore) scanUsage(ctx context.Context, query string, args []any, scan func(*sql.Rows) (usageBucket, error), visit func(usageBucket)) error {
	rows, err := s.index.searchDB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query turn usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		bucket, err := scan(rows)
		if err != nil {
			return fmt.Errorf("scan turn usage: %w", err)
		}
		visit(bucket)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read turn usage: %w", err)
	}
	return nil
}

// activityBuckets reads the rollup over all history; callers split the range.
func (s *FileStore) activityBuckets(ctx context.Context, filter UsageFilter, visit func(usageBucket)) error {
	var query strings.Builder
	query.WriteString(`SELECT bucket, model, runtime, turns, input_tokens, cache_read_tokens, cache_write_tokens,
		output_tokens, total_tokens FROM turn_usage_buckets WHERE 1=1`)
	args := usageFilterSQL(filter, &query, nil)
	return s.scanUsage(ctx, query.String(), args, func(rows *sql.Rows) (usageBucket, error) {
		var bucket usageBucket
		err := rows.Scan(&bucket.bucket, &bucket.model, &bucket.runtime, &bucket.tokens.Turns, &bucket.tokens.Input,
			&bucket.tokens.CacheRead, &bucket.tokens.CacheWrite, &bucket.tokens.Output, &bucket.tokens.Total)
		return bucket, err
	}, visit)
}

// sessionBuckets groups the range's turns by session and model.
func (s *FileStore) sessionBuckets(ctx context.Context, filter UsageFilter, visit func(usageBucket)) error {
	from, to := filter.bounds()
	var query strings.Builder
	query.WriteString(`SELECT session_id, model, SUM(is_channel), MAX(completed_at), COUNT(*), SUM(input_tokens),
		SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(output_tokens), SUM(total_tokens)
		FROM turn_usage WHERE completed_at >= ? AND completed_at < ?`)
	args := usageFilterSQL(filter, &query, []any{from, to})
	query.WriteString(` GROUP BY session_id, model`)
	return s.scanUsage(ctx, query.String(), args, func(rows *sql.Rows) (usageBucket, error) {
		var bucket usageBucket
		err := rows.Scan(&bucket.sessionID, &bucket.model, &bucket.channel, &bucket.last, &bucket.tokens.Turns, &bucket.tokens.Input,
			&bucket.tokens.CacheRead, &bucket.tokens.CacheWrite, &bucket.tokens.Output, &bucket.tokens.Total)
		return bucket, err
	}, visit)
}

func (s *FileStore) usageTrackingSince(ctx context.Context) (*time.Time, error) {
	var earliest sql.NullInt64
	if err := s.index.searchDB.QueryRowContext(ctx, `SELECT MIN(completed_at) FROM turn_usage`).Scan(&earliest); err != nil {
		return nil, fmt.Errorf("query turn usage start: %w", err)
	}
	if !earliest.Valid {
		return nil, nil
	}
	since := time.Unix(0, earliest.Int64).UTC()
	return &since, nil
}

// UsageActivity aggregates the range by local day and model. All-time totals
// and streaks ignore the range but keep the model and channel filters.
func (s *FileStore) UsageActivity(ctx context.Context, filter UsageFilter) (UsageActivity, error) {
	type modelKey struct{ model, runtime string }
	type dayModel struct{ day, model string }
	// Range membership compares local dates, the same labels used for days,
	// so an ambiguous local midnight cannot place an hour on the wrong side.
	from, to := filter.From.Format(time.DateOnly), filter.To.Format(time.DateOnly)
	rangeByDayModel := map[dayModel]UsageTotals{}
	rangeInput := map[dayModel]int64{}
	rangeByModel := map[modelKey]UsageTotals{}
	rangeCacheRead := map[string]int64{}
	allInput := map[string]int64{}
	allCacheRead := map[string]int64{}
	activeDays := map[string]bool{}
	if filter.Location == nil {
		return UsageActivity{}, errors.New("usage query requires a location")
	}
	var allTime UsageTotals
	err := s.activityBuckets(ctx, filter, func(bucket usageBucket) {
		start := bucket.bucket * usageBucketNanos
		day := time.Unix(0, start).In(filter.Location).Format(time.DateOnly)
		activeDays[day] = true
		allTime.add(bucket.tokens)
		allInput[bucket.model] += bucket.fullInput()
		allCacheRead[bucket.model] += bucket.tokens.CacheRead
		if day < from || day > to {
			return
		}
		key := dayModel{day, bucket.model}
		totals := rangeByDayModel[key]
		totals.add(bucket.tokens)
		rangeByDayModel[key] = totals
		rangeInput[key] += bucket.fullInput()
		modelTotals := rangeByModel[modelKey{bucket.model, bucket.runtime}]
		modelTotals.add(bucket.tokens)
		rangeByModel[modelKey{bucket.model, bucket.runtime}] = modelTotals
		rangeCacheRead[bucket.model] += bucket.tokens.CacheRead
	})
	if err != nil {
		return UsageActivity{}, err
	}
	since, err := s.usageTrackingSince(ctx)
	if err != nil {
		return UsageActivity{}, err
	}
	activity := UsageActivity{TrackingSince: since, AllTime: allTime, ByModel: []UsageModel{}, Days: []UsageDay{}}
	for model, input := range allInput {
		if allCacheRead[model] > 0 {
			activity.AllTime.CacheInput += input
		}
	}
	days := map[string]UsageTotals{}
	for key, totals := range rangeByDayModel {
		if rangeCacheRead[key.model] > 0 {
			totals.CacheInput = rangeInput[key]
		}
		day := days[key.day]
		day.add(totals)
		days[key.day] = day
		activity.Range.add(totals)
	}
	for day, totals := range days {
		activity.Days = append(activity.Days, UsageDay{Date: day, UsageTotals: totals})
	}
	sort.Slice(activity.Days, func(i, j int) bool { return activity.Days[i].Date < activity.Days[j].Date })
	for _, day := range activity.Days {
		if activity.PeakDay == nil || day.Total > activity.PeakDay.Total {
			activity.PeakDay = &UsagePeakDay{Date: day.Date, Total: day.Total}
		}
	}
	for key, totals := range rangeByModel {
		// Same rule as the range: a model counts when any of its runtimes read the cache.
		if rangeCacheRead[key.model] > 0 {
			totals.CacheInput = totals.Input + totals.CacheRead + totals.CacheWrite
		}
		activity.ByModel = append(activity.ByModel, UsageModel{Model: key.model, Runtime: key.runtime, UsageTotals: totals})
	}
	sort.Slice(activity.ByModel, func(i, j int) bool {
		left, right := activity.ByModel[i], activity.ByModel[j]
		if left.Total != right.Total {
			return left.Total > right.Total
		}
		return left.Model+"\x00"+left.Runtime < right.Model+"\x00"+right.Runtime
	})
	now := filter.Now
	if now.IsZero() {
		now = time.Now()
	}
	activity.CurrentStreakDays, activity.LongestStreakDays = usageStreaks(activeDays, now.In(filter.Location))
	return activity, nil
}

// The current streak may start yesterday so a day without usage yet does not
// reset it. Dates are compared as civil dates, unaffected by DST.
func usageStreaks(active map[string]bool, now time.Time) (int, int) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !active[day.Format(time.DateOnly)] {
		day = day.AddDate(0, 0, -1)
	}
	current := 0
	for active[day.Format(time.DateOnly)] {
		current++
		day = day.AddDate(0, 0, -1)
	}
	dates := make([]time.Time, 0, len(active))
	for value := range active {
		parsed, err := time.Parse(time.DateOnly, value)
		if err == nil {
			dates = append(dates, parsed)
		}
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	longest, run := 0, 0
	for index, date := range dates {
		if index > 0 && dates[index-1].AddDate(0, 0, 1).Equal(date) {
			run++
		} else {
			run = 1
		}
		longest = max(longest, run)
	}
	return current, longest
}

// UsageSessions ranks sessions by total tokens in the range. The cursor holds
// the last returned total and session ID.
func (s *FileStore) UsageSessions(ctx context.Context, filter UsageFilter, cursor string, limit int) (UsageSessionPage, error) {
	afterTotal, afterID, hasCursor, err := decodeUsageCursor(cursor)
	if err != nil {
		return UsageSessionPage{}, err
	}
	type sessionModel struct{ session, model string }
	sessions := map[string]*UsageSession{}
	sessionModelInput := map[sessionModel]int64{}
	cacheRead := map[string]int64{}
	if filter.Location == nil {
		return UsageSessionPage{}, errors.New("usage query requires a location")
	}
	err = s.sessionBuckets(ctx, filter, func(bucket usageBucket) {
		entry := sessions[bucket.sessionID]
		if entry == nil {
			entry = &UsageSession{SessionID: bucket.sessionID, Models: []string{}}
			sessions[bucket.sessionID] = entry
		}
		entry.add(bucket.tokens)
		entry.ChannelTurns += bucket.channel
		if last := time.Unix(0, bucket.last).UTC(); last.After(entry.LastCompletedAt) {
			entry.LastCompletedAt = last
		}
		entry.Models = append(entry.Models, bucket.model)
		sessionModelInput[sessionModel{bucket.sessionID, bucket.model}] += bucket.fullInput()
		cacheRead[bucket.model] += bucket.tokens.CacheRead
	})
	if err != nil {
		return UsageSessionPage{}, err
	}
	for key, input := range sessionModelInput {
		if cacheRead[key.model] > 0 {
			sessions[key.session].CacheInput += input
		}
	}
	ranked := make([]UsageSession, 0, len(sessions))
	for _, entry := range sessions {
		sort.Strings(entry.Models)
		ranked = append(ranked, *entry)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Total != ranked[j].Total {
			return ranked[i].Total > ranked[j].Total
		}
		return ranked[i].SessionID < ranked[j].SessionID
	})
	start := 0
	if hasCursor {
		start = sort.Search(len(ranked), func(index int) bool {
			entry := ranked[index]
			return entry.Total < afterTotal || (entry.Total == afterTotal && entry.SessionID > afterID)
		})
	}
	end := min(start+limit, len(ranked))
	page := UsageSessionPage{Sessions: ranked[start:end]}
	if end < len(ranked) {
		last := ranked[end-1]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last.Total, 10) + ":" + last.SessionID))
	}
	return page, nil
}

func decodeUsageCursor(cursor string) (int64, string, bool, error) {
	if cursor == "" {
		return 0, "", false, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", false, ErrInvalidUsageCursor
	}
	total, id, found := strings.Cut(string(decoded), ":")
	value, err := strconv.ParseInt(total, 10, 64)
	if !found || err != nil || id == "" {
		return 0, "", false, ErrInvalidUsageCursor
	}
	return value, id, true, nil
}
