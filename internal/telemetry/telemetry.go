package telemetry

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func Setup(ctx context.Context, service, version string, enabled bool, sampleRatio float64) (func(context.Context) error, error) {
	if sampleRatio < 0 || sampleRatio > 1 {
		return nil, errors.New("trace sample ratio must be between zero and one")
	}
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}
	resources, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", service), attribute.String("service.version", version)),
		resource.WithFromEnv(), resource.WithProcess(), resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resources), sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return provider.Shutdown, nil
}
