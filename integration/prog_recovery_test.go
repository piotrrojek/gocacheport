package integration

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piotrrojek/gocacheport/internal/server"
)

func TestProgramHungRemoteAndSingleRecoveryProbe(t *testing.T) {
	var requests atomic.Int64
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			<-request.Context().Done()
			return
		}
		writer.Header().Set(server.OutputHeader, abcDigest)
		writer.Header().Set(server.TimeHeader, "2026-11-05T00:00:00Z")
		writer.Header().Set("Content-Length", "3")
		writer.(http.Flusher).Flush()
		select {
		case probeStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseProbe:
			io.WriteString(writer, "abc")
		case <-request.Context().Done():
		}
	}))
	defer remote.Close()
	input, decoder, command, _ := startProgram(t, remote.URL, false, "--timeout=10s", "--header-timeout=500ms", "--workers=32")
	action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	started := time.Now()
	fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"get\",\"ActionID\":%q}\n", action)
	if response := readWire(t, decoder, 1); !response.Miss {
		t.Fatalf("hung remote response = %+v", response)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("hung server stalled the first lookup for %v", elapsed)
	}
	time.Sleep(1100 * time.Millisecond)
	fmt.Fprintf(input, "{\"ID\":2,\"Command\":\"get\",\"ActionID\":%q}\n", action)
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery probe never reached remote")
	}
	started = time.Now()
	for id := 3; id <= 22; id++ {
		fmt.Fprintf(input, "{\"ID\":%d,\"Command\":\"get\",\"ActionID\":%q}\n", id, action)
	}
	seen := make(map[int64]bool)
	for range 20 {
		var response wireResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.ID < 3 || response.ID > 22 || seen[response.ID] || !response.Miss || response.Err != "" {
			t.Fatalf("parallel request blocked on or bypassed probe: %+v", response)
		}
		seen[response.ID] = true
	}
	if elapsed := time.Since(started); elapsed > time.Second || requests.Load() != 2 {
		t.Fatalf("half-open circuit did not isolate one probe: elapsed=%v remote_requests=%d", elapsed, requests.Load())
	}
	close(releaseProbe)
	probe := readWire(t, decoder, 2)
	if body, err := os.ReadFile(probe.DiskPath); err != nil || string(body) != "abc" || probe.Miss {
		t.Fatalf("recovered artifact = %q, %v, %+v", body, err, probe)
	}
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	fmt.Fprintf(input, "{\"ID\":23,\"Command\":\"get\",\"ActionID\":%q}\n", other)
	if response := readWire(t, decoder, 23); response.Miss || requests.Load() != 3 {
		t.Fatalf("circuit failed to restore healthy requests: %+v requests=%d", response, requests.Load())
	}
	fmt.Fprintln(input, `{"ID":99,"Command":"close"}`)
	readWire(t, decoder, 99)
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestProgramRequestFailuresDoNotDisableHealthyObjects(t *testing.T) {
	for _, failure := range []string{"429", "503", "413", "integrity", "metadata"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int64
			remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept-Encoding") != "" {
					t.Error("artifact transport negotiated transparent compression")
				}
				first := requests.Add(1) == 1
				if first {
					switch failure {
					case "429":
						writer.WriteHeader(429)
						return
					case "503":
						writer.Header().Set("Retry-After", "1")
						writer.Header().Set("X-Gocacheport-Overloaded", "true")
						writer.WriteHeader(503)
						return
					case "413":
						writer.WriteHeader(413)
						return
					}
				}
				writer.Header().Set(server.OutputHeader, abcDigest)
				writer.Header().Set(server.TimeHeader, "2026-11-05T00:00:00Z")
				writer.Header().Set("Content-Length", "3")
				if first && failure == "integrity" {
					io.WriteString(writer, "abd")
				} else if first && failure == "metadata" {
					writer.Header().Set("Content-Length", "5")
					io.WriteString(writer, "abcde")
				} else {
					io.WriteString(writer, "abc")
				}
			}))
			defer remote.Close()
			input, decoder, command, _ := startProgram(t, remote.URL, false, "--max-object-bytes=4")
			action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
			fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"get\",\"ActionID\":%q}\n", action)
			if response := readWire(t, decoder, 1); !response.Miss {
				t.Fatalf("request failure was served as a hit: %+v", response)
			}
			fmt.Fprintf(input, "{\"ID\":2,\"Command\":\"get\",\"ActionID\":%q}\n", action)
			response := readWire(t, decoder, 2)
			body, err := os.ReadFile(response.DiskPath)
			if err != nil || string(body) != "abc" || response.Miss || requests.Load() != 2 {
				t.Fatalf("object-local failure disabled healthy cache: %+v body=%q error=%v requests=%d", response, body, err, requests.Load())
			}
			fmt.Fprintln(input, `{"ID":3,"Command":"close"}`)
			readWire(t, decoder, 3)
			if err := command.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramUsesPrivateDefaultArtifactsAndRejectsUnsafeParent(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	t.Setenv("HOME", parent)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(parent, "gocacheport")); err != nil {
		t.Fatal(err)
	}
	input, decoder, command, _ := startProgram(t, "http://127.0.0.1:1", false, "--local-dir=")
	action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	output := "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0="
	fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, output)
	response := readWire(t, decoder, 1)
	session := filepath.Dir(response.DiskPath)
	info, err := os.Lstat(session)
	if err != nil || info.Mode().Perm() != 0o700 || filepath.Dir(session) != filepath.Join(userCache, "gocacheport", "artifacts") {
		t.Fatalf("session is not private and under the user artifact cache: %s, %v, %v", session, info, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("legacy shared directory was used: %v, %v", entries, err)
	}
	fmt.Fprintln(input, `{"ID":2,"Command":"close"}`)
	readWire(t, decoder, 2)
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, filepath.Join(parent, "gocacheport"), filepath.Join(parent, "gocacheport") + "/"} {
		permissions := os.FileMode(0o700)
		if path == outside {
			permissions = 0o777
		}
		if err := os.Chmod(outside, permissions); err != nil {
			t.Fatal(err)
		}
		check := exec.Command(programBinary, "prog", "--local-dir", path)
		check.Env = cleanEnvironment()
		if output, err := check.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("private, owned directory")) {
			t.Fatalf("unsafe staging parent accepted: %q, %v", output, err)
		}
	}
}

func TestProgramUnavailable503OpensCircuit(t *testing.T) {
	for _, operation := range []string{"get", "put"} {
		t.Run(operation, func(t *testing.T) {
			var requests atomic.Int64
			remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				io.Copy(io.Discard, request.Body)
				writer.Header().Set("Retry-After", "1")
				writer.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer remote.Close()
			input, decoder, command, _ := startProgram(t, remote.URL, false)
			action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
			output := "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0="
			if operation == "put" {
				fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, output)
				response := readWire(t, decoder, 1)
				if body, err := os.ReadFile(response.DiskPath); err != nil || string(body) != "abc" {
					t.Fatalf("unavailable remote lost local put: %q, %v", body, err)
				}
			} else {
				fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"get\",\"ActionID\":%q}\n", action)
				if response := readWire(t, decoder, 1); !response.Miss {
					t.Fatalf("unavailable remote produced hit: %+v", response)
				}
			}
			other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
			fmt.Fprintf(input, "{\"ID\":2,\"Command\":\"get\",\"ActionID\":%q}\n", other)
			if response := readWire(t, decoder, 2); !response.Miss {
				t.Fatalf("open circuit produced hit: %+v", response)
			}
			fmt.Fprintf(input, "{\"ID\":3,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", other, output)
			readWire(t, decoder, 3)
			if requests.Load() != 1 {
				t.Fatalf("unavailable 503 did not suppress remote get/put: requests=%d", requests.Load())
			}
			fmt.Fprintln(input, `{"ID":4,"Command":"close"}`)
			readWire(t, decoder, 4)
			if err := command.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramStagingFailureKeepsProtocolSynchronized(t *testing.T) {
	_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
	input, decoder, command, _ := startProgram(t, data.URL, false)
	action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	output := "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0="
	fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, output)
	stored := readWire(t, decoder, 1)
	if err := os.RemoveAll(filepath.Dir(stored.DiskPath)); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(input, "{\"ID\":2,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, output)
	var response wireResponse
	if err := decoder.Decode(&response); err != nil || response.ID != 2 || !strings.Contains(response.Err, "local artifact staging failed") {
		t.Fatalf("local failure did not get a request-scoped error: %+v, %v", response, err)
	}
	missing := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	fmt.Fprintf(input, "{\"ID\":3,\"Command\":\"get\",\"ActionID\":%q}\n", missing)
	if response := readWire(t, decoder, 3); !response.Miss {
		t.Fatalf("request after failed staging lost framing: %+v", response)
	}
	fmt.Fprintln(input, `{"ID":4,"Command":"close"}`)
	readWire(t, decoder, 4)
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}
