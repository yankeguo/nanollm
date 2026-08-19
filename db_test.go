package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
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

func TestMySQLDetailRetainDefault(t *testing.T) {
	require.Equal(t, 1000, MySQLConfig{}.detailRetain())
	zero := 0
	require.Equal(t, 0, MySQLConfig{DetailRetain: &zero}.detailRetain())
}
