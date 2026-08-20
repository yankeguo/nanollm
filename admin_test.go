package main

import (
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
		{"zero total", 0, 0, `<div class="outbar"><i style="width:0%"></i></div>`},
		{"quarter of total", 3000, 1000, `<div class="outbar"><i style="width:25%"></i></div>`},
		{"all output", 0, 500, `<div class="outbar"><i style="width:100%"></i></div>`},
		{"negative total clamps", -100, 50, `<div class="outbar"><i style="width:0%"></i></div>`},
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
