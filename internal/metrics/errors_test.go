package metrics

import (
	"errors"
	"fmt"
	"testing"
)

// samplingRejectionBody is the shape of Google's Telemetry API refusal when a
// final flush lands too soon after the periodic export.
const samplingRejectionBody = `failed to upload metrics: 400 Bad Request {"code":400,"message":"points were written more frequently than the maximum sampling period","status":"FAILED_PRECONDITION"}`

// otlpSamplingRejection matches the text otlpmetrichttp v1.42 builds for a
// non retryable response, including the endpoint URL.
const otlpSamplingRejection = `failed to upload metrics: failed to send metrics to https://telemetry.googleapis.com/v1/metrics: 400 Bad Request (body: {"error":{"code":400,"message":"One or more points were written more frequently than the maximum sampling period configured for the metric.","status":"FAILED_PRECONDITION"}})`

// opaqueError hides the error it wraps from its own text, so matching it
// requires walking the wrapped chain.
type opaqueError struct{ err error }

func (e opaqueError) Error() string { return "metrics shutdown" }
func (e opaqueError) Unwrap() error { return e.err }

func TestSamplingPeriodRejection(t *testing.T) {
	rejection := errors.New(samplingRejectionBody)
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "telemetry API 400 body", err: rejection, want: true},
		{name: "otlp exporter text", err: errors.New(otlpSamplingRejection), want: true},
		{name: "fmt wrapped", err: fmt.Errorf("shutdown meter provider: %w", rejection), want: true},
		{name: "joined", err: errors.Join(errors.New("collect pending metrics"), rejection), want: true},
		{name: "wrapped without its text", err: opaqueError{err: rejection}, want: true},
		{name: "case insensitive", err: errors.New("POINTS WERE WRITTEN MORE FREQUENTLY THAN THE MAXIMUM SAMPLING PERIOD"), want: true},
		{name: "failed precondition with sampling", err: errors.New(`{"code":400,"message":"write exceeds the sampling rate","status":"FAILED_PRECONDITION"}`), want: true},
		{name: "nil", err: nil, want: false},
		{name: "plain 503", err: errors.New(`failed to upload metrics: retry-able request failure: body: {"code":503,"message":"The service is currently unavailable.","status":"UNAVAILABLE"}`), want: false},
		{name: "sampling without rejection wording", err: errors.New("stdout exporter: sampling disabled"), want: false},
		{name: "failed precondition without sampling", err: errors.New(`{"code":400,"message":"Project has no metrics scope.","status":"FAILED_PRECONDITION"}`), want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SamplingPeriodRejection(tt.err); got != tt.want {
				t.Fatalf("SamplingPeriodRejection(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestExporterStatus(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		want   int
		wantOK bool
	}{
		{name: "telemetry API 400 body", err: errors.New(samplingRejectionBody), want: 400, wantOK: true},
		{name: "otlp exporter status line", err: errors.New(otlpSamplingRejection), want: 400, wantOK: true},
		{name: "status code phrase", err: errors.New("export metrics: unexpected status code: 503"), want: 503, wantOK: true},
		{name: "status line", err: errors.New("failed to send metrics: 503 Service Unavailable"), want: 503, wantOK: true},
		{name: "retryable body code", err: errors.New(`failed to upload metrics: retry-able request failure: body: {"error": {"code": 429, "message": "Quota exceeded.", "status": "RESOURCE_EXHAUSTED"}}`), want: 429, wantOK: true},
		{name: "fmt wrapped", err: fmt.Errorf("shutdown meter provider: %w", errors.New("429 Too Many Requests")), want: 429, wantOK: true},
		{name: "wrapped without its text", err: opaqueError{err: errors.New("status code: 502")}, want: 502, wantOK: true},
		{name: "absent", err: errors.New("private-metrics-exporter-error"), wantOK: false},
		{name: "nil", err: nil, wantOK: false},
		{name: "grpc status code", err: errors.New(`body: {"code":9,"message":"failed precondition"}`), wantOK: false},
		{name: "success status", err: errors.New("unexpected 200 OK"), wantOK: false},
		{name: "number that is not a status", err: errors.New("gave up after 500 attempts against collector.example:4318"), wantOK: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExporterStatus(tt.err)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ExporterStatus(%v) = %d, %v, want %d, %v", tt.err, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
