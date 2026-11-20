package integration

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrrojek/gocacheport/internal/server"
	"github.com/piotrrojek/gocacheport/internal/store"
)

func TestUploadAbortAdmissionAndDrain(t *testing.T) {
	config := storageConfig(t.TempDir())
	cache, err := store.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	service, err := server.New(cache, server.Config{
		Token: "integration-token", MaxConcurrent: 1, MaxObjectBytes: config.MaxObjectBytes,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, admin := httptest.NewServer(service.Handler()), httptest.NewServer(service.AdminHandler())
	t.Cleanup(data.Close)
	t.Cleanup(admin.Close)
	url := data.URL + "/v1/cache/"
	if status, _, _ := request(t, "PUT", url+actionOne, "abc", abcDigest); status != 204 {
		t.Fatalf("initial upload status = %d", status)
	}
	startUpload := func(action string) (*io.PipeWriter, <-chan int) {
		t.Helper()
		reader, writer := io.Pipe()
		t.Cleanup(func() { writer.CloseWithError(errors.New("test cleanup")) })
		req, _ := http.NewRequest("PUT", url+action, reader)
		req.ContentLength = 3
		req.Header.Set("Authorization", "Bearer integration-token")
		req.Header.Set(server.OutputHeader, abcDigest)
		finished := make(chan int, 1)
		go func() {
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				finished <- 0
				return
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			finished <- response.StatusCode
		}()
		if _, err := writer.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool {
			files, _ := filepath.Glob(filepath.Join(config.Directory, ".tmp-*"))
			return len(files) == 1
		})
		return writer, finished
	}
	writer, finished := startUpload(actionOne)
	if status, headers, _ := request(t, "GET", url+actionOne, "", ""); status != 503 || headers.Get("Retry-After") != "1" || headers.Get("X-Gocacheport-Overloaded") != "true" {
		t.Fatalf("admission did not reject excess request: %d %v", status, headers)
	}
	writer.CloseWithError(errors.New("deliberate upload interruption"))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("aborted upload did not finish")
	}
	await(t, func() bool {
		_, _, body := request(t, "GET", admin.URL+"/metrics", "", "")
		return strings.Contains(body, "gocacheport_http_inflight 0\n")
	})
	if status, _, body := request(t, "GET", url+actionOne, "", ""); status != 200 || body != "abc" {
		t.Fatalf("aborted replacement lost committed artifact: %d %q", status, body)
	}
	if files, _ := filepath.Glob(filepath.Join(config.Directory, ".tmp-*")); len(files) != 0 {
		t.Fatalf("aborted upload left temporary files: %v", files)
	}
	writer, finished = startUpload(actionTwo)
	service.Drain()
	if status, _, _ := request(t, "GET", admin.URL+"/readyz", "", ""); status != 503 {
		t.Fatalf("draining readiness status = %d", status)
	}
	if status, _, _ := request(t, "GET", url+actionOne, "", ""); status != 503 {
		t.Fatalf("draining server admitted new request: %d", status)
	}
	if _, err := writer.Write([]byte("bc")); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case status := <-finished:
		if status != 204 {
			t.Fatalf("drain interrupted admitted upload: %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted upload failed to drain")
	}
	data.Close()
	admin.Close()
	cache.Close()
	abandoned := filepath.Join(config.Directory, ".tmp-abandoned")
	if err := os.WriteFile(abandoned, []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, restarted, _ := startHTTP(t, config)
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart did not clean abandoned upload: %v", err)
	}
	if stats := recovered.Snapshot(); stats.DiskEntries != 2 || stats.DiskBytes != 118 {
		t.Fatalf("abort/drain/restart accounting = %+v", stats)
	}
	for _, action := range []string{actionOne, actionTwo} {
		if status, _, body := request(t, "GET", restarted.URL+"/v1/cache/"+action, "", ""); status != 200 || body != "abc" {
			t.Fatalf("committed upload missing after restart: %d %q", status, body)
		}
	}
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true before deadline")
		case <-ticker.C:
		}
	}
}
