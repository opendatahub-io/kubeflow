package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var tracings struct {
	SpanExporter  *tracetest.InMemoryExporter
	TraceProvider *trace.TracerProvider
}

// context examples
// https://github.com/go-logr/logr/pull/27

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func setupOTelSDK(ctx context.Context) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	// shutdown() calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up trace provider.
	tracerProvider, err := newTraceProvider()
	if err != nil {
		handleErr(err)
		return
	}
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	return
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newTraceProvider() (*trace.TracerProvider, error) {
	tracings.SpanExporter = tracetest.NewInMemoryExporter()

	tracings.TraceProvider = trace.NewTracerProvider(
		trace.WithBatcher(tracings.SpanExporter),
	)

	return tracings.TraceProvider, nil
}

// Example tests to show how to work with oTel

const name = "opendatahub.io/kubeflow/components/odh-notebook-controller/controllers/opentelemetry_test.go"

var (
	tracer = otel.Tracer(name)
)

func TestHelloOTel(t *testing.T) {
	// Set up OpenTelemetry.
	otelShutdown, err := setupOTelSDK(ctx)
	if err != nil {
		return
	}
	// Handle shutdown properly so nothing leaks.
	defer func() {
		err = errors.Join(err, otelShutdown(context.Background()))
	}()

	func() {
		_, span := tracer.Start(context.Background(), "roll")
		//defer span.End()
		span.AddEvent("do_stuff")
		span.End()
	}()

	assert.Nil(t, tracings.TraceProvider.ForceFlush(context.Background()))
	spans := tracings.SpanExporter.GetSpans()
	assert.Len(t, spans, 1)
}

// TestOpenTelemetryInMemoryExporterReadout demonstrates accessing spans from test code.
// Code is copied from https://github.com/open-telemetry/opentelemetry-go/issues/2080
func TestOpenTelemetryInMemoryExporterReadout(t *testing.T) {
	ctx := context.Background()

	exp := tracetest.NewInMemoryExporter()

	tp := trace.NewTracerProvider(
		trace.WithBatcher(exp),
	)

	tracer := tp.Tracer("tracer")

	_, span := tracer.Start(ctx, "span")
	span.End()

	err := tp.ForceFlush(ctx)
	assert.NoError(t, err)

	// Expect well-defined behavior due to calling ForceFlush
	assert.Len(t, exp.GetSpans(), 1)
}
