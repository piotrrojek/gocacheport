package integration

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/piotrrojek/gocacheport/internal/telemetry"
)

func TestOTLPExportAndIncomingTraceContext(t *testing.T) {
	exports := make(chan *collector.ExportTraceServiceRequest, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/traces" || request.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("incorrect OTLP/HTTP request: %s, %s", request.URL.Path, request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		var exported collector.ExportTraceServiceRequest
		if err != nil || proto.Unmarshal(body, &exported) != nil {
			t.Errorf("invalid OTLP protobuf payload: %v", err)
			writer.WriteHeader(400)
			return
		}
		exports <- &exported
		writer.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer receiver.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_SERVICE_NAME", "integration-cache")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.namespace.name=integration,k8s.pod.name=cache-fixture")
	shutdown, err := telemetry.Setup(t.Context(), "gocacheport", "integration-version", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
	request, _ := http.NewRequest("GET", data.URL+"/v1/cache/"+actionOne, nil)
	request.Header.Set("Authorization", "Bearer integration-token")
	request.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case exported := <-exports:
		foundSpan, foundPod, foundService := false, false, false
		for _, resource := range exported.ResourceSpans {
			for _, attribute := range resource.Resource.Attributes {
				foundPod = foundPod || attribute.Key == "k8s.pod.name" && attribute.Value.GetStringValue() == "cache-fixture"
				foundService = foundService || attribute.Key == "service.name" && attribute.Value.GetStringValue() == "integration-cache"
			}
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					if span.Name == "GET /v1/cache/{action}" && hex.EncodeToString(span.TraceId) == "4bf92f3577b34da6a3ce929d0e0e4736" && hex.EncodeToString(span.ParentSpanId) == "00f067aa0ba902b7" {
						foundSpan = true
					}
				}
			}
		}
		if !foundSpan || !foundPod || !foundService {
			t.Fatalf("missing independently specified OTLP trace/resource fields: span=%t pod=%t service=%t", foundSpan, foundPod, foundService)
		}
	case <-ctx.Done():
		t.Fatal("trace SDK did not export an OTLP request")
	}
}
