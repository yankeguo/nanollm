package main

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestNormalizeMySQLDSN(t *testing.T) {
	out, err := normalizeMySQLDSN("user:pass@tcp(127.0.0.1:3306)/nanollm?charset=latin1&parseTime=false&loc=Local&time_zone=%27Asia%2FShanghai%27")
	require.NoError(t, err)
	cfg, err := mysql.ParseDSN(out)
	require.NoError(t, err)
	require.True(t, cfg.ParseTime)
	require.Equal(t, time.UTC, cfg.Loc)
	require.Contains(t, out, "charset=utf8mb4")
	require.NotContains(t, out, "latin1")
	require.Equal(t, "'UTC'", cfg.Params["time_zone"])
}

func TestNormalizeMySQLDSNBare(t *testing.T) {
	out, err := normalizeMySQLDSN("nanollm:REPLACE_ME@tcp(127.0.0.1:3306)/nanollm")
	require.NoError(t, err)
	cfg, err := mysql.ParseDSN(out)
	require.NoError(t, err)
	require.True(t, cfg.ParseTime)
	require.Equal(t, time.UTC, cfg.Loc)
	require.Contains(t, out, "charset=utf8mb4")
	require.Equal(t, "'UTC'", cfg.Params["time_zone"])
}

func TestClipBlobAndError(t *testing.T) {
	require.Nil(t, clipBlob(nil))
	require.Equal(t, []byte("ok"), clipBlob([]byte("ok")))
	require.Nil(t, clipBlob(make([]byte, maxMediumBlob+1)))
	require.Equal(t, "short", clipError("short"))
	long := make([]byte, maxErrorLen+10)
	for i := range long {
		long[i] = 'x'
	}
	require.Len(t, clipError(string(long)), maxErrorLen)
}

func TestClipErrorUTF8(t *testing.T) {
	prefix := strings.Repeat("x", maxErrorLen-1)
	got := clipError(prefix + "世")
	require.True(t, utf8.ValidString(got))
	require.LessOrEqual(t, len(got), maxErrorLen)
	require.Equal(t, prefix, got)
}

func TestDetailRetainDefault(t *testing.T) {
	require.Equal(t, 168*time.Hour, Config{}.detailRetain())
	require.Equal(t, time.Duration(0), Config{DetailRetain: retainDuration{set: true}}.detailRetain())
}

func TestPruneAsyncThrottle(t *testing.T) {
	var n int
	l := &gormCallLogger{pruneFn: func() error {
		n++
		return nil
	}}
	for i := 0; i < pruneEveryN-1; i++ {
		l.pruneAsync()
		require.Equal(t, i+1, l.sincePrune)
		require.Equal(t, 0, n)
	}
	l.pruneAsync()
	require.Equal(t, 0, l.sincePrune)
	require.Equal(t, 1, n)

	l.pruneAsync()
	require.Equal(t, 1, l.sincePrune)
	require.Equal(t, 1, n)
}

// TestUsageSeriesMetricAveragesMySQL verifies the /usage chart filters against
// a real MySQL instance. It is opt-in because unit tests must not start or
// require a database: set NANOLLM_TEST_MYSQL_DSN to a *disposable* MySQL 8.x
// database (for example a local `docker run mysql:8.4` container). The test
// owns and truncates llm_calls, so never point it at production.
func TestUsageSeriesMetricAveragesMySQL(t *testing.T) {
	rawDSN := os.Getenv("NANOLLM_TEST_MYSQL_DSN")
	if rawDSN == "" {
		t.Skip("set NANOLLM_TEST_MYSQL_DSN to a disposable MySQL 8.x database to run this SQL integration test")
	}
	dsn, err := normalizeMySQLDSN(rawDSN)
	require.NoError(t, err)
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{NowFunc: func() time.Time { return time.Now().UTC() }})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&LLMCall{}))
	require.NoError(t, db.Exec("DELETE FROM llm_calls").Error)

	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	rows := []LLMCall{
		// Measurable SSE sample.
		{CreatedAt: base, Model: "m", Provider: "p", ResponseMode: responseModeSSE, FirstTokenMs: 100, OutputTokens: 10, OutputSpeed: 20, HTTPStatus: 200},
		// SSE with no output tokens: counted for speed, excluded from TTFT.
		{CreatedAt: base.Add(1 * time.Minute), Model: "m", Provider: "p", ResponseMode: responseModeSSE, FirstTokenMs: 300, OutputTokens: 0, OutputSpeed: 40, HTTPStatus: 200},
		// JSON rows are excluded from both averages.
		{CreatedAt: base.Add(2 * time.Minute), Model: "m", Provider: "p", ResponseMode: responseModeJSON, FirstTokenMs: 900, OutputTokens: 10, OutputSpeed: 60, HTTPStatus: 200},
		// Legacy rows (pre-migration or old instances) are excluded.
		{CreatedAt: base.Add(3 * time.Minute), Model: "m", Provider: "p", ResponseMode: "", FirstTokenMs: 5000, OutputTokens: 10, OutputSpeed: 1000, HTTPStatus: 200},
		// Measurable NDJSON sample.
		{CreatedAt: base.Add(4 * time.Minute), Model: "m", Provider: "p", ResponseMode: responseModeNDJSON, FirstTokenMs: 600, OutputTokens: 5, OutputSpeed: 100, HTTPStatus: 200},
	}
	require.NoError(t, db.Create(&rows).Error)

	f := adminFilter{Bounded: true, Bucket: "hour", From: base.Add(-time.Hour), To: base.Add(time.Hour)}
	buckets, err := queryUsageSeries(db, f)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	got := buckets[0]
	require.Equal(t, int64(5), got.Calls)
	// TTFT: only the two sse/ndjson rows with output_tokens > 0: (100+600)/2.
	require.InDelta(t, 350, got.FirstTokenMs, 0.001)
	// Speed: every non-zero sse/ndjson row: (20+40+100)/3.
	require.InDelta(t, 160.0/3, got.OutputSpeed, 0.001)
}
