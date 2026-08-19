package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSetupMetricsDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	m, shutdown, err := setupMetrics(context.Background())
	require.NoError(t, err)
	require.Nil(t, m)
	require.NoError(t, shutdown(context.Background()))
}

func TestMetricsRecordTokenUsage(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	m, err := newMetrics(mp.Meter("test"))
	require.NoError(t, err)

	m.record(context.Background(), "fast", Provider{Name: "primary", Model: "gpt-4o-mini"}, tokenUsage{
		Input:     100,
		Output:    20,
		CacheRead: 80,
		Uncached:  20,
	})

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.NotEmpty(t, rm.ScopeMetrics)

	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			require.Equal(t, metricTokenUsage, mm.Name)
			sum := mm.Data.(metricdata.Sum[int64])
			for _, dp := range sum.DataPoints {
				var tokenType, model, provider string
				for _, kv := range dp.Attributes.ToSlice() {
					switch string(kv.Key) {
					case attrTokenType:
						tokenType = kv.Value.AsString()
					case attrModel:
						model = kv.Value.AsString()
					case attrProvider:
						provider = kv.Value.AsString()
					case attrUpstreamModel:
						require.Equal(t, "gpt-4o-mini", kv.Value.AsString())
					}
				}
				require.Equal(t, "fast", model)
				require.Equal(t, "primary", provider)
				got[tokenType] = dp.Value
			}
		}
	}
	require.Equal(t, int64(100), got[tokenTypeInput])
	require.Equal(t, int64(20), got[tokenTypeOutput])
	require.Equal(t, int64(80), got[tokenTypeCacheRead])
	require.Equal(t, int64(20), got[tokenTypeUncached])
	_, hasCreation := got[tokenTypeCacheCreation]
	require.False(t, hasCreation)
}

func TestMetricsSkipEmpty(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := newMetrics(mp.Meter("test"))
	require.NoError(t, err)
	m.record(context.Background(), "fast", Provider{Name: "p"}, tokenUsage{})

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			sum := mm.Data.(metricdata.Sum[int64])
			require.Empty(t, sum.DataPoints)
		}
	}
}
