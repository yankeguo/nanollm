package main

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const adminDistinctLimit = 200

type usageBucket struct {
	Bucket   string
	Calls    int64
	Input    int64
	Output   int64
	Cache    int64
	Uncached int64
}

type callListRow struct {
	ID           uint64
	CreatedAt    time.Time
	Model        string
	Provider     string
	APIKeyName   string
	InputTokens  int64
	OutputTokens int64
	CacheTokens  int64
	FirstTokenMs int64
	OutputSpeed  float64
	HTTPStatus   int
	Error        string
	HasDetail    bool
}

// outcomeSQL maps an outcome filter to a WHERE clause. The four outcomes
// partition llm_calls: ok (2xx, no error), canceled (client disconnected,
// before or after the response started), no_response (no HTTP status ever
// arrived, e.g. transport failure), error (anything else with an HTTP
// status: 4xx/5xx, rewrite failures, mid-stream copy errors).
func outcomeSQL(outcome string) (string, []any) {
	switch outcome {
	case "ok":
		return "http_status >= ? AND http_status < ? AND (error = '' OR error IS NULL)", []any{200, 300}
	case "canceled":
		return "error = ?", []any{errCanceled}
	case "no_response":
		return "http_status = ? AND (error IS NULL OR error != ?)", []any{0, errCanceled}
	case "error":
		return "http_status != ? AND ((http_status < ? OR http_status >= ?) OR (error != '' AND error IS NOT NULL AND error != ?))", []any{0, 200, 300, errCanceled}
	default:
		return "", nil
	}
}

func adminFilterSQL(f adminFilter, skip string) (string, []any) {
	var b strings.Builder
	var args []any
	if f.Bounded {
		b.WriteString(" AND created_at >= ? AND created_at < ?")
		args = append(args, f.From, f.To)
	}
	if f.Model != "" && skip != "model" {
		b.WriteString(" AND model = ?")
		args = append(args, f.Model)
	}
	if f.Provider != "" && skip != "provider" {
		b.WriteString(" AND provider = ?")
		args = append(args, f.Provider)
	}
	if f.APIKey != "" && skip != "api_key_name" {
		b.WriteString(" AND api_key_name = ?")
		args = append(args, f.APIKey)
	}
	if clause, cargs := outcomeSQL(f.Outcome); clause != "" {
		b.WriteString(" AND (")
		b.WriteString(clause)
		b.WriteString(")")
		args = append(args, cargs...)
	}
	return b.String(), args
}

func applyAdminFilter(q *gorm.DB, f adminFilter, skip string) *gorm.DB {
	sql, args := adminFilterSQL(f, skip)
	if sql == "" {
		return q
	}
	return q.Where(strings.TrimPrefix(sql, " AND "), args...)
}

func queryUsageSeries(db *gorm.DB, f adminFilter) ([]usageBucket, error) {
	extra, extraArgs := adminFilterSQL(f, "")
	args := append([]any{bucketSQLFormat(f.Bucket)}, extraArgs...)
	var rows []usageBucket
	err := db.Raw(`
SELECT DATE_FORMAT(created_at, ?) AS bucket,
       COUNT(*) AS calls,
       COALESCE(SUM(input_tokens), 0) AS input,
       COALESCE(SUM(output_tokens), 0) AS output,
       COALESCE(SUM(cache_tokens), 0) AS cache,
       COALESCE(SUM(uncached_tokens), 0) AS uncached
FROM llm_calls
WHERE 1=1`+extra+`
GROUP BY bucket
ORDER BY bucket`, args...).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func queryFilterOptions(db *gorm.DB, f adminFilter) (filterOptions, error) {
	var opts filterOptions
	var err error
	opts.Models, err = queryDistinct(db, f, "model")
	if err != nil {
		return opts, err
	}
	opts.Providers, err = queryDistinct(db, f, "provider")
	if err != nil {
		return opts, err
	}
	opts.Keys, err = queryDistinct(db, f, "api_key_name")
	return opts, err
}

func queryDistinct(db *gorm.DB, f adminFilter, col string) ([]string, error) {
	switch col {
	case "model", "provider", "api_key_name":
	default:
		return nil, nil
	}
	var names []string
	err := applyAdminFilter(db.Model(&LLMCall{}), f, col).
		Distinct().
		Order(col+" ASC").
		Limit(adminDistinctLimit).
		Pluck(col, &names).Error
	return names, err
}

func listCalls(db *gorm.DB, f adminFilter, offset, limit int) ([]callListRow, int64, error) {
	q := applyAdminFilter(db.Model(&LLMCall{}), f, "")
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []callListRow
	err := q.Select("id, created_at, model, provider, api_key_name, input_tokens, output_tokens, cache_tokens, first_token_ms, output_speed, http_status, error, (request_json IS NOT NULL OR response_json IS NOT NULL) AS has_detail").
		Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
}

func bucketSQLFormat(bucket string) string {
	switch bucket {
	case "hour":
		return "%Y-%m-%d %H:00"
	case "week":
		return "%x-W%v"
	default:
		return "%Y-%m-%d"
	}
}

func truncateToBucket(t time.Time, bucket string) time.Time {
	t = t.UTC()
	switch bucket {
	case "hour":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
	case "week":
		wd := t.Weekday()
		off := int(wd - time.Monday)
		if wd == time.Sunday {
			off = 6
		}
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return day.AddDate(0, 0, -off)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

func nextBucket(t time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		return t.Add(time.Hour)
	case "week":
		return t.AddDate(0, 0, 7)
	default:
		return t.AddDate(0, 0, 1)
	}
}

func bucketLabel(t time.Time, bucket string) string {
	t = t.UTC()
	switch bucket {
	case "hour":
		return t.Format("2006-01-02 15:00")
	case "week":
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default:
		return t.Format("2006-01-02")
	}
}

func fillUsageBuckets(win adminWindow, rows []usageBucket) []usageBucket {
	byLabel := make(map[string]usageBucket, len(rows))
	for _, r := range rows {
		byLabel[r.Bucket] = r
	}
	if win.From.IsZero() || !win.To.After(win.From) {
		return rows
	}
	var out []usageBucket
	for t := truncateToBucket(win.From, win.Bucket); t.Before(win.To); t = nextBucket(t, win.Bucket) {
		label := bucketLabel(t, win.Bucket)
		if b, ok := byLabel[label]; ok {
			out = append(out, b)
		} else {
			out = append(out, usageBucket{Bucket: label})
		}
	}
	return out
}
