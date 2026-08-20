// Package otel 初始化 OpenTelemetry traces / metrics。
//
// endpoint 為空時不輸出 OTLP（僅 Prometheus /metrics 與 propagation 仍可用）。
package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Options 為 Setup 的選項。
type Options struct {
	// Insecure 為 true 時以明文 gRPC 連 collector（本機 compose）。
	Insecure bool
	// Version / Env 進 resource attributes。
	Version string
	Env     string
	// EnablePrometheus 為 true 時把 metrics 同時註冊到 Prometheus 預設 registry（供 /metrics）。
	EnablePrometheus bool
}

// Setup 初始化全域 TracerProvider / MeterProvider / Propagator，回傳 shutdown 函式。
// endpoint 為空時使用 noop 的 OTLP 輸出（但仍設定 propagator 與 Prometheus reader）。
func Setup(ctx context.Context, serviceName, endpoint string, opts Options) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(opts.Version),
		attribute.String("deployment.environment.name", opts.Env),
	))
	if err != nil {
		return nil, fmt.Errorf("otel: resource: %w", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	var shutdowns []func(context.Context) error

	// ---- traces ----
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if endpoint != "" {
		topts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
		if opts.Insecure {
			topts = append(topts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, topts...)
		if err != nil {
			return nil, fmt.Errorf("otel: trace exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	shutdowns = append(shutdowns, tp.Shutdown)

	// ---- metrics ----
	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if endpoint != "" {
		mopts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
		if opts.Insecure {
			mopts = append(mopts, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, mopts...)
		if err != nil {
			return nil, fmt.Errorf("otel: metric exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(15*time.Second))))
	}
	if opts.EnablePrometheus {
		reader, err := otelprom.New()
		if err != nil {
			return nil, fmt.Errorf("otel: prometheus exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(reader))
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)
	shutdowns = append(shutdowns, mp.Shutdown)

	return func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			if err := shutdowns[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}
