//go:build darwin || linux

package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrrojek/gocacheport/internal/store"
)

func TestDiskByteLimitsAndPinnedReader(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.DiskBytes, config.MaxObjectBytes, config.MemoryBytes = 118, 62, 0
	cache, data, _ := startHTTP(t, config)
	url := data.URL + "/v1/cache/"
	for _, action := range []string{actionOne, actionTwo} {
		if status, _, _ := request(t, "PUT", url+action, "abc", abcDigest); status != 204 {
			t.Fatalf("byte budget rejected fitting artifact: %d", status)
		}
	}
	artifact, err := cache.Get(actionOne)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Body.Close()
	first := make([]byte, 1)
	if _, err := io.ReadFull(artifact.Body, first); err != nil || string(first) != "a" {
		t.Fatalf("pinned first byte = %q, %v", first, err)
	}
	emptyDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if status, _, _ := request(t, "PUT", url+actionOne, "", emptyDigest); status != 204 {
		t.Fatalf("shrinking replacement failed: %d", status)
	}
	if stats := cache.Snapshot(); stats.DiskEntries != 2 || stats.DiskBytes != 115 {
		t.Fatalf("shrinking replacement accounting = %+v", stats)
	}
	if status, headers, body := request(t, "GET", url+actionOne, "", ""); status != 200 || body != "" || headers.Get("X-Gocacheport-Output-ID") != emptyDigest {
		t.Fatalf("replacement body/metadata torn: %d %q %v", status, body, headers)
	}
	request(t, "PUT", url+actionOne, "abc", abcDigest)
	request(t, "PUT", url+actionThree, "abc", abcDigest)
	if stats := cache.Snapshot(); stats.DiskEntries != 2 || stats.DiskBytes != 118 || stats.Evictions != 1 {
		t.Fatalf("disk budget was not enforced exactly: %+v", stats)
	}
	if status, _, _ := request(t, "GET", url+actionTwo, "", ""); status != 404 {
		t.Fatalf("disk-byte LRU eviction did not remove oldest artifact: %d", status)
	}
	remaining, err := io.ReadAll(artifact.Body)
	if err != nil || string(remaining) != "bc" {
		t.Fatalf("open reader changed across replacement: %q, %v", remaining, err)
	}
}

func TestSpecialFilesNeverBlockOrEscape(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.MemoryBytes = 0
	cache, data, admin := startHTTP(t, config)
	url := data.URL + "/v1/cache/" + actionOne
	request(t, "PUT", url, "abc", abcDigest)
	path := filepath.Join(config.Directory, "01", actionOne)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer integration-token")
	response, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatalf("special file blocked the server: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("FIFO served as an artifact: %d", response.StatusCode)
	}
	data.Close()
	admin.Close()
	cache.Close()
	outside := filepath.Join(t.TempDir(), "must-survive")
	if err := os.WriteFile(outside, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	_, restarted, _ := startHTTP(t, config)
	if status, _, _ := request(t, "GET", restarted.URL+"/v1/cache/"+actionOne, "", ""); status != 404 {
		t.Fatalf("symlink accepted after startup: %d", status)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "outside sentinel" {
		t.Fatalf("outside file modified or removed: %q, %v", body, err)
	}
}

func TestRestartWithReducedInventoryLimits(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.MemoryBytes = 0
	cache, data, admin := startHTTP(t, config)
	for index := range 40 {
		action := fmt.Sprintf("%064x", index+100)
		if status, _, _ := request(t, "PUT", data.URL+"/v1/cache/"+action, "abc", abcDigest); status != 204 {
			t.Fatalf("inventory fixture upload failed: %d", status)
		}
	}
	data.Close()
	admin.Close()
	cache.Close()
	config.MaxEntries = 2
	recovered, _, _ := startHTTP(t, config)
	if stats := recovered.Snapshot(); stats.DiskEntries != 2 || stats.DiskBytes != 118 || stats.Evictions != 38 {
		t.Fatalf("reduced startup inventory was not bounded: %+v", stats)
	}
	files := 0
	if err := filepath.WalkDir(config.Directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && store.ValidID(filepath.Base(path)) {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Fatalf("reduced startup left %d record files, want 2", files)
	}
}

func TestSameLengthPayloadCorruptionWithMemoryDisabled(t *testing.T) {
	config := storageConfig(t.TempDir())
	config.MemoryBytes = 0
	cache, data, _ := startHTTP(t, config)
	url := data.URL + "/v1/cache/" + actionOne
	request(t, "PUT", url, "abc", abcDigest)
	file, err := os.OpenFile(filepath.Join(config.Directory, "01", actionOne), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("abd"), 56); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if status, _, body := request(t, "GET", url, "", ""); status != 404 || strings.Contains(body, "abd") {
		t.Fatalf("same-length corruption served to client: %d %q", status, body)
	}
	if stats := cache.Snapshot(); stats.Corruptions != 1 || stats.DiskEntries != 0 {
		t.Fatalf("corrupt generation was not quarantined: %+v", stats)
	}
}
