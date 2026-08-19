package main

import (
	"context"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

const (
	metricTokenUsage = "nanollm.token.usage"

	attrModel         = "nanollm.model"
	attrProvider      = "nanollm.provider"
	attrTokenType     = "nanollm.token.type"
	attrUpstreamModel = "nanollm.upstream.model"

	tokenTypeInput         = "input"
	tokenTypeOutput        = "output"
	tokenTypeCacheRead     = "cache_read"
	tokenTypeCacheCreation = "cache_creation"
	tokenTypeUncached      = "uncached"
)

type Metrics struct {
	tokens metric.Int64Counter
}

func setupMetrics(ctx context.Context) (*Metrics, func(context.Context) error, error) {
	if disabled, _ := strconv.ParseBool(os.Getenv("OTEL_SDK_DISABLED")); disabled {
		return nil, func(context.Context) error { return nil }, nil
	}

	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("nanollm")),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, nil, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	)
	otel.SetMeterProvider(mp)

	m, err := newMetrics(mp.Meter("github.com/yankeguo/nanollm"))
	if err != nil {
		_ = mp.Shutdown(ctx)
		return nil, nil, err
	}
	return m, mp.Shutdown, nil
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	tokens, err := meter.Int64Counter(metricTokenUsage,
		metric.WithDescription("Tokens consumed by forwarded LLM requests, split by model, provider, and token type."),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, err
	}
	return &Metrics{tokens: tokens}, nil
}

func (m *Metrics) record(ctx context.Context, model string, provider Provider, usage tokenUsage) {
	if m == nil || usage.empty() {
		return
	}

	base := []attribute.KeyValue{
		attribute.String(attrModel, model),
		attribute.String(attrProvider, provider.Name),
	}
	if provider.Model != "" {
		base = append(base, attribute.String(attrUpstreamModel, provider.Model))
	}

	add := func(tokenType string, n int64) {
		if n <= 0 {
			return
		}
		attrs := append([]attribute.KeyValue{
			attribute.String(attrTokenType, tokenType),
		}, base...)
		m.tokens.Add(ctx, n, metric.WithAttributes(attrs...))
	}

	add(tokenTypeInput, usage.Input)
	add(tokenTypeOutput, usage.Output)
	add(tokenTypeCacheRead, usage.CacheRead)
	add(tokenTypeCacheCreation, usage.CacheCreation)
	add(tokenTypeUncached, usage.Uncached)
}
