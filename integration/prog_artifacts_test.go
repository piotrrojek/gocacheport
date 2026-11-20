package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProgramRepeatedOutputPreservesOpenArtifact(t *testing.T) {
	_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
	input, decoder, command, _ := startProgram(t, data.URL, false, "--local-max-bytes=6")
	output := "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0="
	var original os.FileInfo
	var readers []*os.File
	for requestID := int64(1); requestID <= 4; requestID++ {
		action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(requestID)}, 32))
		fmt.Fprintf(input, "{\"ID\":%d,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", requestID, action, output)
		response := readWire(t, decoder, requestID)
		reader, err := os.Open(response.DiskPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reader.Close() })
		readers = append(readers, reader)
		info, err := reader.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if original == nil {
			original = info
		} else if !os.SameFile(original, info) {
			t.Fatal("repeated publication replaced an inode still held by an archive consumer")
		}
	}
	fmt.Fprintln(input, `{"ID":5,"Command":"close"}`)
	readWire(t, decoder, 5)
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		contents, err := io.ReadAll(reader)
		if err != nil || string(contents) != "abc" {
			t.Fatalf("retained reader = %q, %v", contents, err)
		}
	}
}

func TestProgramConcurrentArtifactRetentionAndLimits(t *testing.T) {
	for _, budget := range []struct {
		name    string
		bytes   int
		entries int
	}{
		{name: "bytes", bytes: 6, entries: 100},
		{name: "entries", bytes: 100, entries: 4},
	} {
		t.Run(budget.name, func(t *testing.T) {
			_, data, _ := startHTTP(t, storageConfig(t.TempDir()))
			parent := privateTempDir(t)
			limits := []string{
				"--local-dir=" + parent, fmt.Sprintf("--local-max-bytes=%d", budget.bytes),
				fmt.Sprintf("--local-max-entries=%d", budget.entries),
			}
			firstInput, firstDecoder, firstCommand, _ := startProgram(t, data.URL, false, limits...)
			secondInput, secondDecoder, secondCommand, _ := startProgram(t, data.URL, false, limits...)
			action := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
			output := "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0="
			request := fmt.Sprintf("{\"ID\":1,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"YWJj\"\n", action, output)
			fmt.Fprint(firstInput, request)
			fmt.Fprint(secondInput, request)
			first := readWire(t, firstDecoder, 1)
			second := readWire(t, secondDecoder, 1)
			if first.DiskPath == second.DiskPath {
				t.Fatal("concurrent helpers did not isolate their artifact sessions")
			}
			expire := func(path string) {
				t.Helper()
				old := time.Now().Add(-48 * time.Hour)
				if err := os.Chtimes(filepath.Join(filepath.Dir(path), ".lease"), old, old); err != nil {
					t.Fatal(err)
				}
			}
			expire(first.DiskPath)
			expire(second.DiskPath)
			defDigest := sha256.Sum256([]byte("def"))
			otherAction := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
			other := fmt.Sprintf("{\"ID\":2,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":3}\n\n\"ZGVm\"\n", otherAction, base64.StdEncoding.EncodeToString(defDigest[:]))
			full := func() {
				t.Helper()
				fmt.Fprint(secondInput, other)
				var response wireResponse
				if err := secondDecoder.Decode(&response); err != nil || response.ID != 2 || !strings.Contains(response.Err, "cache is full") || response.DiskPath != "" {
					t.Fatalf("protected artifacts did not enforce the local %s limit: %+v, %v", budget.name, response, err)
				}
				for _, path := range []string{first.DiskPath, second.DiskPath} {
					if contents, err := os.ReadFile(path); err != nil || string(contents) != "abc" {
						t.Fatalf("capacity pressure removed protected artifact: %q, %v", contents, err)
					}
				}
			}
			full()
			fmt.Fprintln(firstInput, `{"ID":3,"Command":"close"}`)
			readWire(t, firstDecoder, 3)
			if err := firstCommand.Wait(); err != nil {
				t.Fatal(err)
			}
			full()
			expire(first.DiskPath)
			fmt.Fprint(secondInput, other)
			published := readWire(t, secondDecoder, 2)
			if contents, err := os.ReadFile(published.DiskPath); err != nil || string(contents) != "def" {
				t.Fatalf("publication after bounded cleanup: %q, %v", contents, err)
			}
			if _, err := os.Stat(first.DiskPath); !os.IsNotExist(err) {
				t.Fatalf("expired inactive session was not removed: %v", err)
			}
			secondInput.Close()
			if err := secondCommand.Wait(); err != nil {
				t.Fatal(err)
			}
			var payloadBytes int64
			err := filepath.WalkDir(filepath.Join(parent, "artifacts"), func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				mode := os.FileMode(0o600)
				if entry.IsDir() {
					mode = 0o700
				} else if !strings.HasPrefix(entry.Name(), ".") {
					payloadBytes += info.Size()
				}
				if info.Mode().Perm() != mode {
					t.Errorf("artifact cache privacy: %s has mode %o, want %o", path, info.Mode().Perm(), mode)
				}
				return nil
			})
			if err != nil || payloadBytes != 6 {
				t.Fatalf("retained payload budget: %d bytes, %v", payloadBytes, err)
			}
		})
	}
}
