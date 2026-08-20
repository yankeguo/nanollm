package main

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func adminTestCfg() *Config {
	return &Config{
		Admin:   AdminConfig{Username: "admin", Password: "secret"},
		APIKeys: []APIKey{{Name: "test", Value: testAPIKey}},
		Models:  []ModelConfig{{Name: "fast", Providers: []Provider{{Name: "x", URL: "http://example.invalid", Model: "x"}}}},
	}
}

func TestParseAdminWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		q      string
		rng    string
		bucket string
		dur    time.Duration
	}{
		{"", "7d", "day", 7 * 24 * time.Hour},
		{"range=24h", "24h", "hour", 24 * time.Hour},
		{"range=30d", "30d", "day", 30 * 24 * time.Hour},
		{"range=90d", "90d", "week", 90 * 24 * time.Hour},
		{"range=7d&bucket=month", "7d", "month", 7 * 24 * time.Hour},
		{"range=nope&bucket=hour", "7d", "hour", 7 * 24 * time.Hour},
		{"range=all", "7d", "day", 7 * 24 * time.Hour},
	}
	for _, tc := range tests {
		q, err := url.ParseQuery(tc.q)
		require.NoError(t, err)
		win := parseAdminWindow(q, now)
		require.Equal(t, tc.rng, win.Range, tc.q)
		require.Equal(t, tc.bucket, win.Bucket, tc.q)
		require.Equal(t, now, win.To)
		require.Equal(t, now.Add(-tc.dur), win.From)
	}
	require.Equal(t, "%Y-%m-%d %H:00", bucketSQLFormat("hour"))
	require.Equal(t, "%Y-%m-%d", bucketSQLFormat("day"))
	require.Equal(t, "%x-W%v", bucketSQLFormat("week"))
	require.Equal(t, "%Y-%m", bucketSQLFormat("month"))
}

func TestAdminCookieHMAC(t *testing.T) {
	key := adminCookieKey("admin", "secret")
	exp := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	val := signAdminCookie(key, "admin", exp)
	require.True(t, verifyAdminCookie(key, "admin", val, exp.Add(-time.Minute)))
	require.False(t, verifyAdminCookie(key, "admin", val, exp))
	require.False(t, verifyAdminCookie(key, "admin", val+"x", exp.Add(-time.Minute)))
	require.False(t, verifyAdminCookie(key, "other", val, exp.Add(-time.Minute)))
	require.False(t, verifyAdminCookie(adminCookieKey("admin", "other"), "admin", val, exp.Add(-time.Minute)))
}

func TestAdminLoginAndGuard(t *testing.T) {
	h := NewServer(adminTestCfg(), nil, nil).Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/admin/login", rec.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("username=admin&password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Result().Header.Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "invalid username or password")
	require.Empty(t, rec.Result().Cookies())

	req = httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("username=admin&password=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/admin", rec.Header().Get("Location"))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, "/admin", cookie.Path)
	require.False(t, cookie.Secure)

	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie.Value + "tamper"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/admin/login", rec.Header().Get("Location"))
}

func TestCookieSecureFromForwardedProto(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	require.False(t, cookieSecure(req))
	req.Header.Set("X-Forwarded-Proto", "https")
	require.True(t, cookieSecure(req))
}

func TestPrettyJSON(t *testing.T) {
	require.Empty(t, prettyJSON(nil))
	require.Contains(t, prettyJSON([]byte(`{"a":1}`)), `"a"`)
	require.Equal(t, "hello", prettyJSON([]byte(`"hello"`)))
	require.Equal(t, "not-json", prettyJSON([]byte("not-json")))
}

func TestAdminLoginPage(t *testing.T) {
	h := NewServer(adminTestCfg(), nil, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `name="username"`)
}

func TestAdminLogoutClearsCookieWithoutSession(t *testing.T) {
	h := NewServer(adminTestCfg(), nil, nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/admin/login", rec.Header().Get("Location"))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	require.True(t, cookie.MaxAge < 0)
}

func TestAdminSecurityHeaders(t *testing.T) {
	h := NewServer(adminTestCfg(), nil, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "cdn.jsdelivr.net")
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "connect-src 'self' https://api.iconify.design")
}

func TestAdminTrailingSlashRedirect(t *testing.T) {
	h := NewServer(adminTestCfg(), nil, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/admin", rec.Header().Get("Location"))
}

func TestCallErrorClass(t *testing.T) {
	require.Equal(t, "", callErrorClass(200, ""))
	require.Equal(t, "muted", callErrorClass(200, errCanceled))
	require.Equal(t, "err", callErrorClass(0, errCanceled))
	require.Equal(t, "err", callErrorClass(200, "upstream status 502"))
}

func TestFormatNum(t *testing.T) {
	require.Equal(t, "0", formatNum(0))
	require.Equal(t, "999", formatNum(999))
	require.Equal(t, "1,000", formatNum(1000))
	require.Equal(t, "1,234,567", formatNum(int64(1234567)))
	require.Equal(t, "-1,234", formatNum(-1234))
	require.Equal(t, "1,000", formatNum(uint64(1000)))
}

func TestOutputBar(t *testing.T) {
	cases := []struct {
		name   string
		input  int64
		output int64
		want   string
	}{
		{"zero total", 0, 0, `<div class="outbar"><i style="width:0%"></i></div><div class="sub">&nbsp;</div>`},
		{"quarter of total", 3000, 1000, `<div class="outbar"><i style="width:25%"></i></div><div class="sub">&nbsp;</div>`},
		{"all output", 0, 500, `<div class="outbar"><i style="width:100%"></i></div><div class="sub">&nbsp;</div>`},
		{"negative total clamps", -100, 50, `<div class="outbar"><i style="width:0%"></i></div><div class="sub">&nbsp;</div>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, template.HTML(c.want), outputBar(c.input, c.output))
		})
	}
}

func TestInputBar(t *testing.T) {
	cases := []struct {
		name  string
		input int64
		cache int64
		want  string
	}{
		{"zero input", 0, 0, `<div class="inbar"><i style="width:0%"></i></div><div class="sub">0 cached · 0%</div>`},
		{"half cached", 2000, 1000, `<div class="inbar"><i style="width:50%"></i></div><div class="sub">1,000 cached · 50%</div>`},
		{"all cached", 1234, 1234, `<div class="inbar"><i style="width:100%"></i></div><div class="sub">1,234 cached · 100%</div>`},
		{"cache over input clamps", 100, 250, `<div class="inbar"><i style="width:100%"></i></div><div class="sub">250 cached · 100%</div>`},
		{"negative input", -10, 5, `<div class="inbar"><i style="width:0%"></i></div><div class="sub">5 cached · 0%</div>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, template.HTML(c.want), inputBar(c.input, c.cache))
		})
	}
}

func TestParseAdminFilterCustom(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	q := url.Values{
		"range":  []string{"custom"},
		"from":   []string{from.Format(time.RFC3339)},
		"to":     []string{to.Format(time.RFC3339)},
		"bucket": []string{"hour"},
		"model":  []string{"fast"},
	}
	f := parseAdminFilter(q, now, "usage")
	require.Equal(t, "custom", f.Range)
	require.Equal(t, "hour", f.Bucket)
	require.True(t, f.Bounded)
	require.True(t, from.Equal(f.From))
	require.True(t, to.Equal(f.To))
	require.Equal(t, "fast", f.Model)

	q.Set("from", to.Format(time.RFC3339))
	q.Set("to", from.Format(time.RFC3339))
	f = parseAdminFilter(q, now, "usage")
	require.Equal(t, "7d", f.Range)
	require.True(t, f.Bounded)
	require.Equal(t, now.Add(-7*24*time.Hour), f.From)
	require.Equal(t, now, f.To)

	q.Set("from", now.Add(-400*24*time.Hour).Format(time.RFC3339))
	q.Set("to", now.Format(time.RFC3339))
	f = parseAdminFilter(q, now, "usage")
	require.Equal(t, "7d", f.Range)

	q.Set("from", "not-a-time")
	q.Set("to", to.Format(time.RFC3339))
	f = parseAdminFilter(q, now, "calls")
	require.Equal(t, "all", f.Range)
	require.False(t, f.Bounded)

	q = url.Values{"range": []string{"custom"}, "from": []string{"2026-08-01T00:00"}, "to": []string{"2026-08-02T00:00"}}
	f = parseAdminFilter(q, now, "usage")
	require.Equal(t, "custom", f.Range)
	require.Equal(t, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), f.From)
}

func TestParseAdminFilterCallsAll(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	f := parseAdminFilter(url.Values{}, now, "calls")
	require.Equal(t, "all", f.Range)
	require.False(t, f.Bounded)
	require.True(t, f.From.IsZero())

	f = parseAdminFilter(url.Values{"range": []string{"7d"}, "api_key": []string{"alice"}}, now, "calls")
	require.Equal(t, "7d", f.Range)
	require.True(t, f.Bounded)
	require.Equal(t, "alice", f.APIKey)
	require.Equal(t, now.Add(-7*24*time.Hour), f.From)
}

func TestAdminFilterRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC)
	to := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	orig := adminFilter{
		Range:    "custom",
		Bucket:   "week",
		From:     from,
		To:       to,
		Bounded:  true,
		Model:    "fast",
		Provider: "openai",
		APIKey:   "alice",
		Outcome:  "error",
	}
	q := orig.values("usage")
	got := parseAdminFilter(q, now, "usage")
	require.Equal(t, orig.Range, got.Range)
	require.Equal(t, orig.Bucket, got.Bucket)
	require.Equal(t, orig.Model, got.Model)
	require.Equal(t, orig.Provider, got.Provider)
	require.Equal(t, orig.APIKey, got.APIKey)
	require.Equal(t, orig.Outcome, got.Outcome)
	require.True(t, orig.From.Equal(got.From))
	require.True(t, orig.To.Equal(got.To))

	empty := parseAdminFilter(url.Values{}, now, "usage")
	require.Empty(t, empty.values("usage").Encode())
}

func TestAdminFilterCrossLinks(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	f := parseAdminFilter(url.Values{"range": []string{"30d"}, "model": []string{"fast"}, "page": []string{"3"}}, now, "usage")
	usage := f.path("usage", "/admin")
	calls := f.path("calls", "/admin/calls")
	require.Contains(t, usage, "range=30d")
	require.Contains(t, usage, "model=fast")
	require.NotContains(t, usage, "page=")
	require.Contains(t, calls, "range=30d")
	require.Contains(t, calls, "model=fast")
	require.NotContains(t, calls, "page=")
	require.NotContains(t, calls, "bucket=")

	fromCalls := parseAdminFilter(url.Values{"model": []string{"fast"}}, now, "calls")
	require.Equal(t, "all", fromCalls.Range)
	usageFromCalls := fromCalls.forUsage().path("usage", "/admin")
	require.Equal(t, "/admin?model=fast", usageFromCalls)
	require.NotContains(t, usageFromCalls, "range=")

	require.Equal(t, "/admin/calls?model=fast", pagerURL(fromCalls, 1))
	require.Equal(t, "/admin/calls?model=fast&page=2", pagerURL(fromCalls, 2))

	g := setFilter(f, "provider", "openai")
	require.Equal(t, "openai", g.Provider)
	require.Equal(t, "fast", g.Model)
	cleared := setFilter(g, "model", "")
	require.Empty(t, cleared.Model)
	require.Equal(t, "openai", cleared.Provider)
}

func TestOutcomeSQL(t *testing.T) {
	clause, args := outcomeSQL("ok")
	require.Contains(t, clause, "http_status >= ?")
	require.Equal(t, []any{200, 300}, args)
	clause, args = outcomeSQL("canceled")
	require.Equal(t, "error = ?", clause)
	require.Equal(t, []any{errCanceled}, args)
	clause, args = outcomeSQL("no_response")
	require.Equal(t, "http_status = ?", clause)
	require.Equal(t, []any{0}, args)
	clause, args = outcomeSQL("error")
	require.Contains(t, clause, "error != ?")
	require.Equal(t, []any{errCanceled}, args)
	clause, args = outcomeSQL("")
	require.Empty(t, clause)
	require.Nil(t, args)
	require.Equal(t, "", parseOutcome("nope"))
	require.Equal(t, "ok", parseOutcome("ok"))
}

func TestAdminFilterSQL(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	f := adminFilter{
		Bounded:  true,
		From:     from,
		To:       to,
		Model:    "fast",
		Provider: "openai",
		APIKey:   "alice",
		Outcome:  "ok",
	}
	sql, args := adminFilterSQL(f, "")
	require.Contains(t, sql, "created_at >= ?")
	require.Contains(t, sql, "model = ?")
	require.Contains(t, sql, "provider = ?")
	require.Contains(t, sql, "api_key_name = ?")
	require.Contains(t, sql, "http_status >= ?")
	require.Equal(t, []any{from, to, "fast", "openai", "alice", 200, 300}, args)

	sql, args = adminFilterSQL(f, "model")
	require.NotContains(t, sql, "model =")
	require.Contains(t, sql, "provider = ?")
	require.Equal(t, []any{from, to, "openai", "alice", 200, 300}, args)

	sql, args = adminFilterSQL(adminFilter{Range: "all"}, "")
	require.Empty(t, sql)
	require.Nil(t, args)
}

func TestFillUsageBuckets(t *testing.T) {
	win := adminWindow{
		Bucket: "hour",
		From:   time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC),
	}
	got := fillUsageBuckets(win, []usageBucket{{Bucket: "2026-08-20 11:00", Calls: 5, Input: 10}})
	require.Equal(t, []usageBucket{
		{Bucket: "2026-08-20 10:00"},
		{Bucket: "2026-08-20 11:00", Calls: 5, Input: 10},
		{Bucket: "2026-08-20 12:00"},
		{Bucket: "2026-08-20 13:00"},
	}, got)

	dayWin := adminWindow{
		Bucket: "day",
		From:   time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}
	got = fillUsageBuckets(dayWin, nil)
	require.Equal(t, []string{"2026-08-18", "2026-08-19", "2026-08-20", "2026-08-21"}, bucketNames(got))

	weekWin := adminWindow{
		Bucket: "week",
		From:   time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC),
	}
	got = fillUsageBuckets(weekWin, nil)
	require.Equal(t, []string{"2026-W34", "2026-W35"}, bucketNames(got))

	monthWin := adminWindow{
		Bucket: "month",
		From:   time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	got = fillUsageBuckets(monthWin, nil)
	require.Equal(t, []string{"2026-07", "2026-08"}, bucketNames(got))
}

func bucketNames(rows []usageBucket) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Bucket
	}
	return out
}

func TestAdminTemplatesRender(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	f := parseAdminFilter(url.Values{"model": []string{"fast"}}, now, "usage")
	data := adminNavData("usage", f)
	data["Window"] = f.window()
	data["Kind"] = "usage"
	data["FilterAction"] = "/admin"
	data["Totals"] = usageTotals{Calls: 1, Input: 2, Output: 3}
	data["Series"] = []usageBucket{{Bucket: "2026-08-20", Calls: 1, Input: 2, Output: 3}}
	data["ChartJSON"] = template.JS(`{"labels":["2026-08-20"],"calls":[1],"input":[2],"output":[3],"cache":[0],"uncached":[2]}`)
	data["Breakdowns"] = []map[string]any{
		{"Title": "model", "Param": "model", "Rows": []usageNamed{{Name: "fast", Calls: 1, Input: 2, Output: 3}}},
	}
	mergeFilterView(data, f, "usage", filterOptions{Models: []string{"fast"}})
	var buf bytes.Buffer
	require.NoError(t, adminTmpl.ExecuteTemplate(&buf, "usage.html", data))
	body := buf.String()
	require.Contains(t, body, `href="/admin?model=fast"`)
	require.Contains(t, body, `/admin/calls?`)
	require.Contains(t, body, "model=fast")
	require.Contains(t, body, "By model")

	cf := parseAdminFilter(url.Values{"model": []string{"fast"}}, now, "calls")
	cdata := adminNavData("calls", cf)
	cdata["Kind"] = "calls"
	cdata["FilterAction"] = "/admin/calls"
	cdata["Rows"] = []callListRow{}
	cdata["Total"] = int64(0)
	cdata["ListQuery"] = filterQuery("calls", cf)
	mergeFilterView(cdata, cf, "calls", filterOptions{Models: []string{"fast"}})
	buf.Reset()
	require.NoError(t, adminTmpl.ExecuteTemplate(&buf, "calls.html", cdata))
	require.Contains(t, buf.String(), `name="model"`)
	require.Contains(t, buf.String(), "all")
}
