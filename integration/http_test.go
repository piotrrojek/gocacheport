package integration

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotrrojek/gocacheport/internal/server"
	"github.com/piotrrojek/gocacheport/internal/store"
)

const (
	abcDigest   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	actionOne   = "0101010101010101010101010101010101010101010101010101010101010101"
	actionTwo   = "0202020202020202020202020202020202020202020202020202020202020202"
	actionThree = "0303030303030303030303030303030303030303030303030303030303030303"
)

func storageConfig(directory string) store.Config {
	return store.Config{
		Directory: directory, DiskBytes: 1 << 20, MaxEntries: 100,
		MaxObjectBytes: 1 << 16, MemoryBytes: 1024, MemoryObjectBytes: 512,
	}
}

func startHTTP(t *testing.T, config store.Config) (*store.Store, *httptest.Server, *httptest.Server) {
	t.Helper()
	cache, err := store.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	service, err := server.New(cache, server.Config{
		Token: "integration-token", MaxConcurrent: 8, MaxObjectBytes: config.MaxObjectBytes,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, admin := httptest.NewServer(service.Handler()), httptest.NewServer(service.AdminHandler())
	t.Cleanup(data.Close)
	t.Cleanup(admin.Close)
	return cache, data, admin
}

func request(t *testing.T, method, url, body, digest string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer integration-token")
	if digest != "" {
		req.Header.Set(server.OutputHeader, digest)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, string(contents)
}

func TestHTTPIntegrityPersistenceAndIsolation(t *testing.T) {
	directory := t.TempDir()
	config := storageConfig(directory)
	cache, data, admin := startHTTP(t, config)
	url := data.URL + "/v1/cache/" + actionOne
	if status, _, _ := request(t, "GET", url, "", ""); status != 404 {
		t.Fatalf("fresh action status = %d, want 404", status)
	}
	if status, _, body := request(t, "PUT", url, "abc", abcDigest); status != 204 {
		t.Fatalf("known SHA-256 vector rejected: %d %s", status, body)
	}
	for range 2 {
		status, headers, body := request(t, "GET", url, "", "")
		if status != 200 || body != "abc" || headers.Get(server.OutputHeader) != abcDigest || headers.Get("Content-Length") != "3" {
			t.Fatalf("stored known vector = %d %q %v", status, body, headers)
		}
	}
	if status, _, _ := request(t, "PUT", url, "bad", abcDigest); status != 400 {
		t.Fatalf("forged upload status = %d, want 400", status)
	}
	if _, _, body := request(t, "GET", url, "", ""); body != "abc" {
		t.Fatalf("failed upload replaced committed artifact: %q", body)
	}
	if stats := cache.Snapshot(); stats.MemoryHits < 1 || stats.DiskHits != 1 || stats.Writes != 1 {
		t.Fatalf("cache tiers not exercised: %+v", stats)
	}
	for _, route := range []string{"/metrics", "/readyz", "/debug/pprof/"} {
		if status, _, _ := request(t, "GET", data.URL+route, "", ""); status != 404 {
			t.Fatalf("administrative route %s exposed on artifact listener: %d", route, status)
		}
	}
	if status, _, metrics := request(t, "GET", admin.URL+"/metrics", "", ""); status != 200 || !strings.Contains(metrics, "gocacheport_writes_total 1\n") {
		t.Fatalf("missing observed write metric: %d %s", status, metrics)
	}
	if _, err := store.Open(config); err == nil {
		t.Fatal("second writer acquired the same directory")
	}
	data.Close()
	admin.Close()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	config.MemoryBytes = 0
	_, restarted, _ := startHTTP(t, config)
	if status, headers, body := request(t, "GET", restarted.URL+"/v1/cache/"+actionOne, "", ""); status != 200 || body != "abc" || headers.Get(server.OutputHeader) != abcDigest {
		t.Fatalf("restart lost verified artifact: %d %q %v", status, body, headers)
	}
}

func TestHTTPLimitsEvictionAndAuthentication(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.MaxEntries = 2
	_, data, _ := startHTTP(t, config)
	for _, action := range []string{actionOne, actionTwo} {
		if status, _, _ := request(t, "PUT", data.URL+"/v1/cache/"+action, "abc", abcDigest); status != 204 {
			t.Fatalf("upload status: %d", status)
		}
	}
	request(t, "GET", data.URL+"/v1/cache/"+actionOne, "", "")
	request(t, "PUT", data.URL+"/v1/cache/"+actionThree, "abc", abcDigest)
	if status, _, _ := request(t, "GET", data.URL+"/v1/cache/"+actionTwo, "", ""); status != 404 {
		t.Fatalf("least recently used action survived eviction: %d", status)
	}
	if status, _, _ := request(t, "GET", data.URL+"/v1/cache/"+actionOne, "", ""); status != 200 {
		t.Fatalf("recently used action evicted: %d", status)
	}
	if status, _, _ := request(t, "PUT", data.URL+"/v1/cache/"+actionOne, strings.Repeat("x", 1<<17), abcDigest); status != 413 {
		t.Fatalf("oversize upload status = %d", status)
	}
	if status, _, _ := request(t, "GET", data.URL+"/v1/cache/not-an-id", "", ""); status != 400 {
		t.Fatalf("invalid ID status = %d", status)
	}
	response, err := http.Get(data.URL + "/v1/cache/" + actionOne)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 || response.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unauthenticated request accepted: %d", response.StatusCode)
	}
}

func TestHTTPExpiryAndCorruptRecord(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.MemoryBytes = 0
	cache, data, _ := startHTTP(t, config)
	url := data.URL + "/v1/cache/" + actionOne
	request(t, "PUT", url, "abc", abcDigest)
	if err := os.WriteFile(filepath.Join(config.Directory, "01", actionOne), []byte("not a record"), 0o640); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := request(t, "GET", url, "", ""); status != 404 {
		t.Fatalf("corrupt record served: %d", status)
	}
	if stats := cache.Snapshot(); stats.Corruptions != 1 {
		t.Fatalf("corruption was not detected: %+v", stats)
	}
	cache.Close()
	data.Close()
	config.TTL = 50 * time.Millisecond
	cache, data, _ = startHTTP(t, config)
	url = data.URL + "/v1/cache/" + actionOne
	request(t, "PUT", url, "abc", abcDigest)
	time.Sleep(75 * time.Millisecond)
	if status, _, _ := request(t, "GET", url, "", ""); status != 404 {
		t.Fatalf("expired entry served: %d", status)
	}
	if stats := cache.Snapshot(); stats.DiskEntries != 0 || stats.DiskBytes != 0 {
		t.Fatalf("corruption/expiry accounting: %+v", stats)
	}
}

func TestHTTPConcurrentReadersAndWriters(t *testing.T) {
	_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
	var workers sync.WaitGroup
	for index := range 16 {
		workers.Go(func() {
			action := fmt.Sprintf("%064x", index+100)
			url := data.URL + "/v1/cache/" + action
			for range 3 {
				req, _ := http.NewRequest("PUT", url, bytes.NewBufferString("abc"))
				req.Header.Set("Authorization", "Bearer integration-token")
				req.Header.Set(server.OutputHeader, abcDigest)
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != 204 && response.StatusCode != 503 {
					t.Errorf("concurrent write status = %d", response.StatusCode)
					return
				}
				if response.StatusCode == 503 {
					continue
				}
				status, _, body := request(t, "GET", url, "", "")
				if status != 503 && (status != 200 || body != "abc") {
					t.Errorf("concurrent read returned torn data: %d %q", status, body)
				}
			}
		})
	}
	workers.Wait()
}
