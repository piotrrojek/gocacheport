package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/piotrrojek/gocacheport/internal/server"
)

var programBinary string

func TestMain(tests *testing.M) {
	directory, err := os.MkdirTemp("", "gocacheport-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	programBinary = filepath.Join(directory, "gocacheport")
	command := exec.Command("go", "build", "-race", "-o", programBinary, "../cmd/gocacheport")
	command.Env = cleanEnvironment()
	if output, err := command.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build integration binary: %v\n%s", err, output)
		os.RemoveAll(directory)
		os.Exit(1)
	}
	code := tests.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

func cleanEnvironment() []string {
	return slices.DeleteFunc(os.Environ(), func(value string) bool {
		return strings.HasPrefix(value, "GOCACHEPORT_") || strings.HasPrefix(value, "GOCACHEPROG=") ||
			strings.HasPrefix(value, "GOCACHE=") || strings.HasPrefix(value, "OTEL_")
	})
}

type wireResponse struct {
	ID            int64
	Err           string
	KnownCommands []string
	Miss          bool
	DiskPath      string
	OutputID      []byte
	Size          int64
}

func startProgram(t *testing.T, url string, strict bool, overrides ...string) (io.WriteCloser, *json.Decoder, *exec.Cmd, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	args := []string{"prog", "--url", url, "--local-dir", t.TempDir(), "--timeout", "1s"}
	if strict {
		args = append(args, "--strict")
	}
	args = append(args, overrides...)
	command := exec.CommandContext(ctx, programBinary, args...)
	command.Env = append(cleanEnvironment(), "GOCACHEPORT_TOKEN=integration-token")
	var logs bytes.Buffer
	command.Stderr = &logs
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); command.Process.Kill() })
	decoder := json.NewDecoder(output)
	var hello wireResponse
	if err := decoder.Decode(&hello); err != nil || hello.ID != 0 || !slices.Equal(hello.KnownCommands, []string{"get", "put", "close"}) {
		t.Fatalf("invalid capability handshake: %+v, %v", hello, err)
	}
	return input, decoder, command, &logs
}

func readWire(t *testing.T, decoder *json.Decoder, id int64) wireResponse {
	t.Helper()
	var response wireResponse
	if err := decoder.Decode(&response); err != nil || response.ID != id || response.Err != "" {
		t.Fatalf("protocol response for %d = %+v, %v", id, response, err)
	}
	return response
}

func TestProgramIndependentWireFixtures(t *testing.T) {
	_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
	input, decoder, command, logs := startProgram(t, data.URL, true)
	action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	output, _ := hex.DecodeString(abcDigest)
	fmt.Fprintf(input, "{\"ID\":7,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, base64.StdEncoding.EncodeToString(output))
	put := readWire(t, decoder, 7)
	contents, err := os.ReadFile(put.DiskPath)
	if err != nil || string(contents) != "abc" || !filepath.IsAbs(put.DiskPath) || filepath.Ext(put.DiskPath) != "" {
		t.Fatalf("Go-readable local body = %q, %v, %q", contents, err, put.DiskPath)
	}
	fmt.Fprintf(input, "{\"ID\":8,\"Command\":\"get\",\"ActionID\":%q}\n", action)
	get := readWire(t, decoder, 8)
	if get.Miss || get.Size != 3 || !bytes.Equal(get.OutputID, output) || get.DiskPath != put.DiskPath {
		t.Fatalf("local action lookup returned incorrect metadata: %+v", get)
	}
	emptyOutput, _ := hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	emptyAction := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	fmt.Fprintf(input, "{\"ID\":9,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q}\n\n", emptyAction, base64.StdEncoding.EncodeToString(emptyOutput))
	empty := readWire(t, decoder, 9)
	if contents, err := os.ReadFile(empty.DiskPath); err != nil || len(contents) != 0 {
		t.Fatalf("zero-byte output was not materialized: %q, %v", contents, err)
	}
	fmt.Fprintln(input, `{"ID":10,"Command":"close"}`)
	readWire(t, decoder, 10)
	if err := command.Wait(); err != nil {
		t.Fatalf("helper exit: %v %s", err, logs.String())
	}
	if _, err := os.Stat(put.DiskPath); !os.IsNotExist(err) {
		t.Fatalf("session was not removed after close: %v", err)
	}
	if status, _, body := request(t, "GET", data.URL+"/v1/cache/"+actionTwo, "", ""); status != 200 || body != "" {
		t.Fatalf("zero-byte object did not reach server: %d %q", status, body)
	}
}

func TestProgramRejectsCorruptRemoteArtifact(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set(server.OutputHeader, abcDigest)
		writer.Header().Set(server.TimeHeader, "2026-11-05T00:00:00Z")
		writer.Header().Set("Content-Length", "3")
		io.WriteString(writer, "abd")
	}))
	defer remote.Close()
	input, decoder, command, _ := startProgram(t, remote.URL, false)
	action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	fmt.Fprintf(input, "{\"ID\":1,\"Command\":\"get\",\"ActionID\":%q}\n", action)
	response := readWire(t, decoder, 1)
	if !response.Miss || response.DiskPath != "" {
		t.Fatalf("corrupt remote artifact reached Go: %+v", response)
	}
	fmt.Fprintln(input, `{"ID":2,"Command":"close"}`)
	readWire(t, decoder, 2)
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRealGoBuildAndTestRemoteReuse(t *testing.T) {
	settings := storageConfig(t.TempDir())
	settings.DiskBytes, settings.MaxObjectBytes = 512<<20, 64<<20
	settings.MaxEntries, settings.MemoryBytes = 10000, 16<<20
	cache, data, _ := startHTTP(t, settings)
	fixture := t.TempDir()
	files := map[string]string{
		"go.mod":             "module example.com/cache-fixture\n\ngo 1.27.3\n",
		"main.go":            "package main\nimport \"example.com/cache-fixture/calc\"\nfunc main() { println(calc.Value()) }\n",
		"calc/value.go":      "package calc\nfunc Value() int { return 42 }\n",
		"calc/value_test.go": "package calc\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 42 { t.Fatal(\"expected the answer 42\") } }\n",
	}
	for name, content := range files {
		path := filepath.Join(fixture, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	goCommand := func(url string, strict bool, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = fixture
		helper := fmt.Sprintf("%q prog --url %q --local-dir %q --workers 4 --timeout 5s", programBinary, url, t.TempDir())
		if strict {
			helper += " --strict"
		}
		command.Env = append(cleanEnvironment(), "GOCACHE="+t.TempDir(), "GOCACHEPROG="+helper, "GOCACHEPORT_TOKEN=integration-token", "GOTOOLCHAIN=local")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v failed: %v\n%s", args, err, output)
		}
		return string(output)
	}
	binary := filepath.Join(fixture, "fixture")
	goCommand(data.URL, true, "build", "-p", "4", "-x", "-o", binary, ".")
	if output, err := exec.Command(binary).CombinedOutput(); err != nil || string(output) != "42\n" {
		t.Fatalf("real compiled program output = %q, %v", output, err)
	}
	before := cache.Snapshot()
	if before.Writes == 0 {
		t.Fatal("cold Go build did not upload artifacts")
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	warmBuild := goCommand(data.URL, true, "build", "-p", "4", "-x", "-o", binary, ".")
	after := cache.Snapshot()
	if after.DiskHits+after.MemoryHits <= before.DiskHits+before.MemoryHits || strings.Contains(warmBuild, "/compile ") {
		t.Fatalf("fresh-client build did not reuse remote compiler output:\n%s", warmBuild)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil || string(output) != "42\n" {
		t.Fatalf("remotely reused executable = %q, %v", output, err)
	}
	goCommand(data.URL, true, "test", "-p", "4", "./calc")
	if output := goCommand(data.URL, true, "test", "-p", "4", "./calc"); !strings.Contains(output, "(cached)") {
		t.Fatalf("real Go test result was not remotely reusable:\n%s", output)
	}
	goCommand("http://127.0.0.1:1", false, "build", "-p", "4", "-o", binary, ".")
	if output, err := exec.Command(binary).CombinedOutput(); err != nil || string(output) != "42\n" {
		t.Fatalf("outage fallback did not produce a working executable: %q, %v", output, err)
	}
}
