package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const adminPageSize = 50
const maxAdminPage = 1_000_000

type usageTotals struct {
	Calls    int64
	Input    int64
	Output   int64
	Cache    int64
	Uncached int64
}

type usageBucket struct {
	Bucket   string
	Calls    int64
	Input    int64
	Output   int64
	Cache    int64
	Uncached int64
}

type usageNamed struct {
	Name     string
	Calls    int64
	Input    int64
	Output   int64
	Cache    int64
	Uncached int64
}

type usageChart struct {
	Labels   []string `json:"labels"`
	Calls    []int64  `json:"calls"`
	Input    []int64  `json:"input"`
	Output   []int64  `json:"output"`
	Cache    []int64  `json:"cache"`
	Uncached []int64  `json:"uncached"`
}

type adminWindow struct {
	Range  string
	Bucket string
	From   time.Time
	To     time.Time
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
	HTTPStatus   int
	Error        string
	HasDetail    bool
}

func parseAdminWindow(q url.Values, now time.Time) adminWindow {
	now = now.UTC()
	rng := q.Get("range")
	switch rng {
	case "24h", "30d", "90d":
	default:
		rng = "7d"
	}
	var dur time.Duration
	defBucket := "day"
	switch rng {
	case "24h":
		dur = 24 * time.Hour
		defBucket = "hour"
	case "7d":
		dur = 7 * 24 * time.Hour
	case "30d":
		dur = 30 * 24 * time.Hour
	case "90d":
		dur = 90 * 24 * time.Hour
		defBucket = "week"
	}
	bucket := q.Get("bucket")
	switch bucket {
	case "hour", "day", "week", "month":
	default:
		bucket = defBucket
	}
	return adminWindow{Range: rng, Bucket: bucket, From: now.Add(-dur), To: now}
}

func bucketSQLFormat(bucket string) string {
	switch bucket {
	case "hour":
		return "%Y-%m-%d %H:00"
	case "week":
		return "%x-W%v"
	case "month":
		return "%Y-%m"
	default:
		return "%Y-%m-%d"
	}
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.adminCookieValid(r) {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.renderAdmin(w, "login.html", map[string]any{"Error": ""})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !s.checkAdminLogin(r.Form.Get("username"), r.Form.Get("password")) {
			// renderAdmin sets Content-Type after WriteHeader, so write it first.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			s.renderAdmin(w, "login.html", map[string]any{"Error": "invalid username or password"})
			return
		}
		s.setAdminCookie(w, r)
		http.Redirect(w, r, "/admin", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	s.clearAdminCookie(w, r)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	win := parseAdminWindow(r.URL.Query(), time.Now().UTC())
	series, err := queryUsageSeries(s.DB, win)
	if err != nil {
		log.Println("admin usage series:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	byModel, err := queryUsageBreakdown(s.DB, win, "model")
	if err != nil {
		log.Println("admin usage model:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	byProvider, err := queryUsageBreakdown(s.DB, win, "provider")
	if err != nil {
		log.Println("admin usage provider:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	byKey, err := queryUsageBreakdown(s.DB, win, "api_key_name")
	if err != nil {
		log.Println("admin usage api_key:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	totals := usageTotals{}
	chart := usageChart{
		Labels:   make([]string, 0, len(series)),
		Calls:    make([]int64, 0, len(series)),
		Input:    make([]int64, 0, len(series)),
		Output:   make([]int64, 0, len(series)),
		Cache:    make([]int64, 0, len(series)),
		Uncached: make([]int64, 0, len(series)),
	}
	for _, b := range series {
		chart.Labels = append(chart.Labels, b.Bucket)
		chart.Calls = append(chart.Calls, b.Calls)
		chart.Input = append(chart.Input, b.Input)
		chart.Output = append(chart.Output, b.Output)
		chart.Cache = append(chart.Cache, b.Cache)
		chart.Uncached = append(chart.Uncached, b.Uncached)
		totals.Calls += b.Calls
		totals.Input += b.Input
		totals.Output += b.Output
		totals.Cache += b.Cache
		totals.Uncached += b.Uncached
	}
	raw, err := json.Marshal(chart)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	s.renderAdmin(w, "usage.html", map[string]any{
		"Nav":       "usage",
		"Window":    win,
		"Ranges":    []string{"24h", "7d", "30d", "90d"},
		"Buckets":   []string{"hour", "day", "week", "month"},
		"Totals":    totals,
		"Series":    series,
		"ChartJSON": template.JS(raw),
		"Breakdowns": []map[string]any{
			{"Title": "model", "Rows": byModel},
			{"Title": "provider", "Rows": byProvider},
			{"Title": "api key", "Rows": byKey},
		},
	})
}

func (s *Server) handleAdminCalls(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	filter := struct {
		Model    string
		Provider string
		APIKey   string
	}{
		Model:    strings.TrimSpace(q.Get("model")),
		Provider: strings.TrimSpace(q.Get("provider")),
		APIKey:   strings.TrimSpace(q.Get("api_key")),
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > maxAdminPage {
		page = maxAdminPage
	}
	offset := (page - 1) * adminPageSize
	rows, total, err := listCalls(s.DB, filter.Model, filter.Provider, filter.APIKey, offset, adminPageSize)
	if err != nil {
		log.Println("admin calls:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	vals := url.Values{}
	if filter.Model != "" {
		vals.Set("model", filter.Model)
	}
	if filter.Provider != "" {
		vals.Set("provider", filter.Provider)
	}
	if filter.APIKey != "" {
		vals.Set("api_key", filter.APIKey)
	}
	var prev, next string
	if page > 1 {
		v := cloneValues(vals)
		if page-1 > 1 {
			v.Set("page", strconv.Itoa(page-1))
		}
		prev = "/admin/calls"
		if enc := v.Encode(); enc != "" {
			prev += "?" + enc
		}
	}
	if offset+len(rows) < int(total) {
		v := cloneValues(vals)
		v.Set("page", strconv.Itoa(page+1))
		next = "/admin/calls?" + v.Encode()
	}
	s.renderAdmin(w, "calls.html", map[string]any{
		"Nav":    "calls",
		"Filter": filter,
		"Rows":   rows,
		"Total":  total,
		"Prev":   prev,
		"Next":   next,
	})
}

func (s *Server) handleAdminCall(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		http.NotFound(w, r)
		return
	}
	var call LLMCall
	if err := s.DB.First(&call, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Println("admin call lookup:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	s.renderAdmin(w, "detail.html", map[string]any{
		"Nav":            "calls",
		"Call":           call,
		"RequestPretty":  prettyJSON(call.RequestJSON),
		"ResponsePretty": prettyJSON(call.ResponseJSON),
	})
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	if s, ok := v.(string); ok {
		return s
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return string(raw)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// inputBar renders the cached share of an input token total as a small
// proportion bar (amber cached over the blue uncached track, matching the
// usage chart) plus a "N cached · P%" caption. The bar length represents the
// full input, so cache reads as a slice of it.
func inputBar(input, cache int64) template.HTML {
	var pct int64
	if input > 0 {
		pct = cache * 100 / input
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
	}
	return template.HTML(`<div class="inbar"><i style="width:` + strconv.FormatInt(pct, 10) +
		`%"></i></div><div class="sub">` + formatInt64(cache) + ` cached · ` + strconv.FormatInt(pct, 10) + `%</div>`)
}

func callErrorClass(status int, err string) string {
	if err == "" {
		return ""
	}
	if err == errCanceled && status >= 200 && status < 300 {
		return "muted"
	}
	return "err"
}

func formatNum(n any) string {
	switch t := n.(type) {
	case int:
		return formatInt64(int64(t))
	case int64:
		return formatInt64(t)
	case uint64:
		return commaDigits(strconv.FormatUint(t, 10))
	default:
		return ""
	}
}

func formatInt64(v int64) string {
	s := strconv.FormatInt(v, 10)
	if s[0] == '-' {
		return "-" + commaDigits(s[1:])
	}
	return commaDigits(s)
}

func commaDigits(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	b.Grow(n + n/3)
	rem := n % 3
	if rem == 0 {
		rem = 3
	}
	b.WriteString(s[:rem])
	for i := rem; i < n; i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func queryUsageSeries(db *gorm.DB, win adminWindow) ([]usageBucket, error) {
	var rows []usageBucket
	err := db.Raw(`
SELECT DATE_FORMAT(created_at, ?) AS bucket,
       COUNT(*) AS calls,
       COALESCE(SUM(input_tokens), 0) AS input,
       COALESCE(SUM(output_tokens), 0) AS output,
       COALESCE(SUM(cache_tokens), 0) AS cache,
       COALESCE(SUM(uncached_tokens), 0) AS uncached
FROM llm_calls
WHERE created_at >= ? AND created_at < ?
GROUP BY bucket
ORDER BY bucket`, bucketSQLFormat(win.Bucket), win.From, win.To).Scan(&rows).Error
	return rows, err
}

func queryUsageBreakdown(db *gorm.DB, win adminWindow, col string) ([]usageNamed, error) {
	switch col {
	case "model", "provider", "api_key_name":
	default:
		return nil, nil
	}
	var rows []usageNamed
	err := db.Raw(`
SELECT COALESCE(`+col+`, '') AS name,
       COUNT(*) AS calls,
       COALESCE(SUM(input_tokens), 0) AS input,
       COALESCE(SUM(output_tokens), 0) AS output,
       COALESCE(SUM(cache_tokens), 0) AS cache,
       COALESCE(SUM(uncached_tokens), 0) AS uncached
FROM llm_calls
WHERE created_at >= ? AND created_at < ?
GROUP BY name
ORDER BY input DESC, calls DESC
LIMIT 50`, win.From, win.To).Scan(&rows).Error
	return rows, err
}

func listCalls(db *gorm.DB, model, provider, apiKey string, offset, limit int) ([]callListRow, int64, error) {
	q := db.Model(&LLMCall{})
	if model != "" {
		q = q.Where("model = ?", model)
	}
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	if apiKey != "" {
		q = q.Where("api_key_name = ?", apiKey)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []callListRow
	err := q.Select("id, created_at, model, provider, api_key_name, input_tokens, output_tokens, cache_tokens, http_status, error, (request_json IS NOT NULL OR response_json IS NOT NULL) AS has_detail").
		Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
}

func (s *Server) renderAdmin(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Println("admin template:", err)
	}
}
