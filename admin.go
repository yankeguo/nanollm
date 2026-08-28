package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const adminPageSize = 50
const maxAdminPage = 1_000_000

// adminLoginFailDelay slows failed password attempts to damp online brute force.
const adminLoginFailDelay = time.Second

type usageTotals struct {
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

type callFileView struct {
	SHA256   string
	MimeType string
	Size     int
	Kind     string
}

func callFileViews(files []LLMFile) []callFileView {
	out := make([]callFileView, len(files))
	for i, f := range files {
		out[i] = callFileView{
			SHA256:   f.SHA256,
			MimeType: f.MimeType,
			Size:     f.Size,
			Kind:     fileKind(f.MimeType),
		}
	}
	return out
}

func (s *Server) requireDB(w http.ResponseWriter) bool {
	if s.DB != nil {
		return true
	}
	http.Error(w, "database unavailable", http.StatusServiceUnavailable)
	return false
}

func clampAdminPage(raw string) int {
	page, _ := strconv.Atoi(raw)
	if page < 1 {
		return 1
	}
	if page > maxAdminPage {
		return maxAdminPage
	}
	return page
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.adminCookieValid(r) {
			http.Redirect(w, r, "/usage", http.StatusFound)
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
			// Write Content-Type before 401 so renderAdmin's later Set is a no-op
			// on already-sent headers.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			s.renderAdmin(w, "login.html", map[string]any{"Error": "invalid username or password"})
			return
		}
		s.setAdminCookie(w, r)
		http.Redirect(w, r, "/usage", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	s.clearAdminCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
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
	data["FilterAction"] = "/usage"
	data["Totals"] = totals
	data["ChartJSON"] = template.JS(raw)
	mergeFilterView(data, f, "usage", opts)
	s.renderAdmin(w, "usage.html", data)
}

func (s *Server) handleAdminCalls(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	q := r.URL.Query()
	f := parseAdminFilter(q, time.Now().UTC(), "calls")
	page := clampAdminPage(q.Get("page"))
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
	data["FilterAction"] = "/calls"
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
	if !s.requireDB(w) {
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
	page := clampAdminPage(r.URL.Query().Get("page"))
	data := adminNavData("calls", f)
	data["Filter"] = f
	data["Kind"] = "calls"
	data["Call"] = call
	data["RequestPretty"] = prettyJSON(call.RequestJSON)
	data["ResponsePretty"] = prettyJSON(call.ResponseJSON)
	data["Files"] = callFileViews(s.listCallFiles(call.ID))
	data["CallsURL"] = pagerURL(f, page)
	s.renderAdmin(w, "detail.html", data)
}

func (s *Server) listCallFiles(callID uint64) []LLMFile {
	var files []LLMFile
	if err := s.DB.Model(&LLMFile{}).
		Select("llm_files.sha256, llm_files.mime_type, llm_files.size").
		Joins("JOIN llm_call_files ON llm_call_files.sha256 = llm_files.sha256").
		Where("llm_call_files.call_id = ?", callID).
		Order("llm_call_files.seq").
		Find(&files).Error; err != nil {
		log.Println("admin call files:", err)
		return nil
	}
	return files
}

func (s *Server) handleAdminFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	sha := r.PathValue("sha")
	if !validFileSHA256(sha) {
		http.NotFound(w, r)
		return
	}
	var f LLMFile
	if err := s.DB.Select("mime_type, data, created_at").Where("sha256 = ?", sha).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Println("admin file lookup:", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	if inlineFileMime(f.MimeType) {
		h.Set("Content-Type", f.MimeType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
	}
	// ServeContent handles Range requests, which Safari requires for media.
	http.ServeContent(w, r, "", f.CreatedAt, bytes.NewReader(f.Data))
}

func adminNavData(nav string, f adminFilter) map[string]any {
	return map[string]any{
		"Nav":      nav,
		"Filter":   f,
		"UsageURL": f.forUsage().path("usage", "/usage"),
		"CallsURL": f.path("calls", "/calls"),
	}
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

func tokenBarPct(part, total int64) int64 {
	if total <= 0 {
		return 0
	}
	pct := part * 100 / total
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// inputBar renders the cached share of an input token total as a small
// proportion bar (amber cached over the blue uncached track, matching the
// usage chart) plus a "N cached · P%" caption. The bar length represents the
// full input, so cache reads as a slice of it.
func inputBar(input, cache int64) template.HTML {
	pct := tokenBarPct(cache, input)
	return template.HTML(`<div class="inbar"><i style="width:` + strconv.FormatInt(pct, 10) +
		`%"></i></div><div class="sub">` + formatInt64(cache) + ` cached · ` + strconv.FormatInt(pct, 10) + `%</div>`)
}

// outputBar renders the output share of total tokens (input + output) as a
// solid green bar matching the usage chart's output color. Same geometry as
// inputBar, with a blank caption line so input/output cells (3 rows each)
// stay vertically aligned as one mirrored unit.
func outputBar(input, output int64) template.HTML {
	pct := tokenBarPct(output, input+output)
	return template.HTML(`<div class="outbar"><i style="width:` + strconv.FormatInt(pct, 10) + `%"></i></div><div class="sub">&nbsp;</div>`)
}

// statusClass picks the badge color for an upstream HTTP status: green for
// 2xx, red for anything else that got a response, muted when the provider was
// never reached (status 0). Returns the badge variant classes from main.css.
func statusClass(status int) string {
	switch {
	case status == 0:
		return "badge-muted"
	case status >= 200 && status < 300:
		return "badge-ok"
	default:
		return "badge-err"
	}
}

// callErrorClass colors the error indicator/text: muted for a client cancel
// after a 2xx, red otherwise. Returns Tailwind text color classes.
func callErrorClass(status int, err string) string {
	if err == "" {
		return ""
	}
	if err == errCanceled && status >= 200 && status < 300 {
		return "text-zinc-500"
	}
	return "text-red-400"
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

func formatMs(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return strconv.FormatFloat(float64(ms)/1000, 'f', 2, 64) + "s"
}

func formatSpeed(v float64) string {
	if v <= 0 {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " tok/s"
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

func (s *Server) renderAdmin(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Println("admin template:", err)
	}
}
