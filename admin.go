package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
const maxAdminUsageRange = 366 * 24 * time.Hour
const adminDistinctLimit = 200

// adminLoginFailDelay slows failed password attempts to damp online brute force.
const adminLoginFailDelay = time.Second

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

type adminFilter struct {
	Range    string
	Bucket   string
	From     time.Time
	To       time.Time
	Bounded  bool
	TZ       string
	Model    string
	Provider string
	APIKey   string
	Outcome  string
}

type selectOption struct {
	Value string
	Label string
	On    bool
}

type filterOptions struct {
	Models    []string
	Providers []string
	Keys      []string
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
	f := parseAdminFilter(q, now, "usage")
	return adminWindow{Range: f.Range, Bucket: f.Bucket, From: f.From, To: f.To}
}

func parseAdminFilter(q url.Values, now time.Time, kind string) adminFilter {
	now = now.UTC()
	f := adminFilter{
		Model:    strings.TrimSpace(q.Get("model")),
		Provider: strings.TrimSpace(q.Get("provider")),
		APIKey:   strings.TrimSpace(q.Get("api_key")),
		Outcome:  parseOutcome(q.Get("outcome")),
	}
	if tz := strings.TrimSpace(q.Get("tz")); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			f.TZ = loc.String()
		}
	}
	loc := f.tzLoc()
	rng := q.Get("range")
	if kind == "calls" {
		switch rng {
		case "24h", "7d", "30d", "90d", "custom", "all":
		default:
			rng = "all"
		}
	} else {
		switch rng {
		case "24h", "7d", "30d", "90d", "custom":
		default:
			rng = "7d"
		}
	}
	f.Range = rng

	switch rng {
	case "all":
		// unbounded
	case "custom":
		from, okFrom := parseAdminTime(q.Get("from"), loc)
		to, okTo := parseAdminTime(q.Get("to"), loc)
		if !okFrom || !okTo || !to.After(from) || (kind == "usage" && to.Sub(from) > maxAdminUsageRange) {
			if kind == "calls" {
				f.Range = "all"
			} else {
				f.Range = "7d"
				f.Bounded = true
				f.From, f.To = presetWindow("7d", now)
			}
			break
		}
		f.Bounded = true
		f.From = from
		f.To = to
	default:
		f.Bounded = true
		f.From, f.To = presetWindow(rng, now)
	}
	if f.Bounded {
		f.Bucket = deriveAdminBucket(f.From, f.To)
	}
	return f
}

// tzLoc resolves the filter's display timezone, defaulting to UTC.
func (f adminFilter) tzLoc() *time.Location {
	if f.TZ != "" {
		if loc, err := time.LoadLocation(f.TZ); err == nil {
			return loc
		}
	}
	return time.UTC
}

// deriveAdminBucket picks the bucket granularity from the window span; users
// no longer choose it. Short windows get hours, medium days, long weeks.
func deriveAdminBucket(from, to time.Time) string {
	switch span := to.Sub(from); {
	case span <= 48*time.Hour:
		return "hour"
	case span <= 62*24*time.Hour:
		return "day"
	default:
		return "week"
	}
}

func parseOutcome(s string) string {
	switch strings.TrimSpace(s) {
	case "ok", "error", "canceled", "no_response":
		return s
	default:
		return ""
	}
}

func presetWindow(rng string, now time.Time) (time.Time, time.Time) {
	var dur time.Duration
	switch rng {
	case "24h":
		dur = 24 * time.Hour
	case "30d":
		dur = 30 * 24 * time.Hour
	case "90d":
		dur = 90 * 24 * time.Hour
	default:
		dur = 7 * 24 * time.Hour
	}
	return now.Add(-dur), now
}

// parseAdminTime parses a custom range endpoint. Offset-bearing RFC3339 is
// honored as given; naive layouts are interpreted in loc (the selected tz).
func parseAdminTime(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
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

func (f adminFilter) window() adminWindow {
	return adminWindow{Range: f.Range, Bucket: f.Bucket, From: f.From, To: f.To}
}

func (f adminFilter) values(kind string) url.Values {
	v := url.Values{}
	switch {
	case f.Range == "":
	case kind == "calls" && f.Range == "all":
	case kind == "usage" && f.Range == "7d":
	default:
		v.Set("range", f.Range)
	}
	if f.Range == "custom" && f.Bounded {
		if !f.From.IsZero() {
			v.Set("from", f.From.UTC().Format(time.RFC3339))
		}
		if !f.To.IsZero() {
			v.Set("to", f.To.UTC().Format(time.RFC3339))
		}
	}
	if f.TZ != "" && f.TZ != "UTC" {
		v.Set("tz", f.TZ)
	}
	if f.Model != "" {
		v.Set("model", f.Model)
	}
	if f.Provider != "" {
		v.Set("provider", f.Provider)
	}
	if f.APIKey != "" {
		v.Set("api_key", f.APIKey)
	}
	if f.Outcome != "" {
		v.Set("outcome", f.Outcome)
	}
	return v
}

func (f adminFilter) path(kind, base string) string {
	enc := f.values(kind).Encode()
	if enc == "" {
		return base
	}
	return base + "?" + enc
}

func adminKindPath(kind string) string {
	if kind == "calls" {
		return "/admin/calls"
	}
	return "/admin"
}

func (f adminFilter) forUsage() adminFilter {
	g := f
	if g.Range == "all" || g.Range == "" {
		g.Range = "7d"
		g.Bounded = true
		g.From = time.Time{}
		g.To = time.Time{}
	}
	return g
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
			time.Sleep(adminLoginFailDelay)
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
	f := parseAdminFilter(r.URL.Query(), time.Now().UTC(), "usage")
	series, err := queryUsageSeries(s.DB, f)
	if err != nil {
		log.Println("admin usage series:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	opts, err := queryFilterOptions(s.DB, f)
	if err != nil {
		log.Println("admin usage options:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	totals := usageTotals{}
	chartSeries := series
	if f.Bounded {
		chartSeries = fillUsageBuckets(f.window(), series)
	}
	chart := usageChart{
		Labels:   make([]string, 0, len(chartSeries)),
		Calls:    make([]int64, 0, len(chartSeries)),
		Input:    make([]int64, 0, len(chartSeries)),
		Output:   make([]int64, 0, len(chartSeries)),
		Cache:    make([]int64, 0, len(chartSeries)),
		Uncached: make([]int64, 0, len(chartSeries)),
	}
	for _, b := range chartSeries {
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
	data := adminNavData("usage", f)
	data["Filter"] = f
	data["Kind"] = "usage"
	data["FilterAction"] = "/admin"
	data["Totals"] = totals
	data["ChartJSON"] = template.JS(raw)
	mergeFilterView(data, f, "usage", opts)
	s.renderAdmin(w, "usage.html", data)
}

func (s *Server) handleAdminCalls(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	f := parseAdminFilter(q, time.Now().UTC(), "calls")
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > maxAdminPage {
		page = maxAdminPage
	}
	offset := (page - 1) * adminPageSize
	rows, total, err := listCalls(s.DB, f, offset, adminPageSize)
	if err != nil {
		log.Println("admin calls:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	opts, err := queryFilterOptions(s.DB, f)
	if err != nil {
		log.Println("admin calls options:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var prev, next string
	if page > 1 {
		prev = pagerURL(f, page-1)
	}
	if offset+len(rows) < int(total) {
		next = pagerURL(f, page+1)
	}
	totalPages := int((total + adminPageSize - 1) / adminPageSize)
	if totalPages < 1 {
		totalPages = 1
	}
	data := adminNavData("calls", f)
	data["Filter"] = f
	data["Kind"] = "calls"
	data["FilterAction"] = "/admin/calls"
	data["Rows"] = rows
	data["Total"] = total
	data["Page"] = page
	data["TotalPages"] = totalPages
	data["Pages"] = pagerItems(f, page, totalPages)
	data["Prev"] = prev
	data["Next"] = next
	lv := f.values("calls")
	if page > 1 {
		lv.Set("page", strconv.Itoa(page))
	}
	// template.URL: embedded in an href, a plain string gets percent-escaped
	// by html/template, which would break multi-param links.
	data["ListQuery"] = template.URL(lv.Encode())
	mergeFilterView(data, f, "calls", opts)
	s.renderAdmin(w, "calls.html", data)
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
	f := parseAdminFilter(r.URL.Query(), time.Now().UTC(), "calls")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > maxAdminPage {
		page = maxAdminPage
	}
	data := adminNavData("calls", f)
	data["Filter"] = f
	data["Kind"] = "calls"
	data["Call"] = call
	data["RequestPretty"] = prettyJSON(call.RequestJSON)
	data["ResponsePretty"] = prettyJSON(call.ResponseJSON)
	data["CallsURL"] = pagerURL(f, page)
	s.renderAdmin(w, "detail.html", data)
}

func adminNavData(nav string, f adminFilter) map[string]any {
	return map[string]any{
		"Nav":      nav,
		"Filter":   f,
		"UsageURL": f.forUsage().path("usage", "/admin"),
		"CallsURL": f.path("calls", "/admin/calls"),
	}
}

func mergeFilterView(data map[string]any, f adminFilter, kind string, opts filterOptions) {
	ranges := []string{"24h", "7d", "30d", "90d"}
	if kind == "calls" {
		ranges = append([]string{"all"}, ranges...)
	}
	ranges = append(ranges, "custom")
	rangeOpts := make([]selectOption, 0, len(ranges))
	for _, rng := range ranges {
		rangeOpts = append(rangeOpts, selectOption{Value: rng, Label: rng, On: f.Range == rng})
	}
	data["RangeOpts"] = rangeOpts
	data["ModelOpts"] = mergeOption(opts.Models, f.Model)
	data["ProviderOpts"] = mergeOption(opts.Providers, f.Provider)
	data["KeyOpts"] = mergeOption(opts.Keys, f.APIKey)
	tz := f.TZ
	if tz == "" {
		tz = "UTC"
	}
	data["TZone"] = tz
	data["TZExplicit"] = f.TZ != ""
	// RangeParam mirrors values(): omit the range hidden input when it is the
	// kind's default so the dim-filter form does not pin it explicitly.
	rangeParam := f.Range
	if rangeParam == "" || (kind == "usage" && rangeParam == "7d") || (kind == "calls" && rangeParam == "all") {
		rangeParam = ""
	}
	data["RangeParam"] = rangeParam
	if f.Bounded {
		loc := f.tzLoc()
		data["FromLocal"] = f.From.In(loc).Format("2006-01-02T15:04")
		data["ToLocal"] = f.To.In(loc).Format("2006-01-02T15:04")
		data["FromRFC"] = f.From.UTC().Format(time.RFC3339)
		data["ToRFC"] = f.To.UTC().Format(time.RFC3339)
	} else {
		data["FromLocal"] = ""
		data["ToLocal"] = ""
		data["FromRFC"] = ""
		data["ToRFC"] = ""
	}
}

func mergeOption(opts []string, extra string) []string {
	if extra == "" {
		return opts
	}
	for _, o := range opts {
		if o == extra {
			return opts
		}
	}
	return append([]string{extra}, opts...)
}

func pagerURL(f adminFilter, page int) string {
	v := f.values("calls")
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	enc := v.Encode()
	if enc == "" {
		return "/admin/calls"
	}
	return "/admin/calls?" + enc
}

// pagerItem is one slot of the calls pager: either a numbered page link or an
// ellipsis gap.
type pagerItem struct {
	Num    int
	URL    template.URL
	Active bool
	Gap    bool
}

// pagerItems builds a compact numbered window around page: the first and last
// page plus current ±2, with ellipsis gaps between runs.
func pagerItems(f adminFilter, page, totalPages int) []pagerItem {
	show := map[int]bool{1: true, totalPages: true}
	for p := page - 2; p <= page+2; p++ {
		if p >= 1 && p <= totalPages {
			show[p] = true
		}
	}
	var items []pagerItem
	prev := 0
	for p := 1; p <= totalPages; p++ {
		if !show[p] {
			continue
		}
		if prev > 0 && p-prev > 1 {
			items = append(items, pagerItem{Gap: true})
		}
		// template.URL: built by pagerURL from our own params, same as ListQuery.
		items = append(items, pagerItem{Num: p, URL: template.URL(pagerURL(f, p)), Active: p == page})
		prev = p
	}
	return items
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

// outputBar renders the output share of total tokens (input + output) as a
// solid green bar matching the usage chart's output color. Same geometry as
// inputBar, with a blank caption line so input/output cells (3 rows each)
// stay vertically aligned as one mirrored unit.
func outputBar(input, output int64) template.HTML {
	var pct int64
	if total := input + output; total > 0 {
		pct = output * 100 / total
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
	}
	return template.HTML(`<div class="outbar"><i style="width:` + strconv.FormatInt(pct, 10) + `%"></i></div><div class="sub">&nbsp;</div>`)
}

// statusClass picks the badge color for an upstream HTTP status: green for
// 2xx, red for anything else that got a response, muted when the provider was
// never reached (status 0). Returns Bootstrap text-bg classes.
func statusClass(status int) string {
	switch {
	case status == 0:
		return "text-bg-secondary"
	case status >= 200 && status < 300:
		return "text-bg-success"
	default:
		return "text-bg-danger"
	}
}

// callErrorClass colors the error indicator/text: muted for a client cancel
// after a 2xx, red otherwise. Returns Bootstrap text color classes.
func callErrorClass(status int, err string) string {
	if err == "" {
		return ""
	}
	if err == errCanceled && status >= 200 && status < 300 {
		return "text-secondary"
	}
	return "text-danger"
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
		return "http_status = ? AND (error = '' OR error IS NULL OR error != ?)", []any{0, errCanceled}
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
	err := q.Select("id, created_at, model, provider, api_key_name, input_tokens, output_tokens, cache_tokens, http_status, error, (request_json IS NOT NULL OR response_json IS NOT NULL) AS has_detail").
		Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
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
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
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
	case "month":
		return t.AddDate(0, 1, 0)
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
	case "month":
		return t.Format("2006-01")
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

func (s *Server) renderAdmin(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Println("admin template:", err)
	}
}
