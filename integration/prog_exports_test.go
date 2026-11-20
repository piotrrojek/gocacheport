package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

func TestRealGoExportArchivesOutliveHelper(t *testing.T) {
	settings := storageConfig(t.TempDir())
	settings.DiskBytes, settings.MaxObjectBytes = 512<<20, 64<<20
	settings.MaxEntries = 10000
	cache, data, _ := startHTTP(t, settings)
	fixture := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":     "module example.com/export-fixture\n\ngo 1.27.3\n",
		"fixture.go": "package fixture\nimport \"fmt\"\nfunc Message() string { return fmt.Sprintf(\"%s\", \"cached\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	closed := filepath.Join(fixture, "helper-closed")
	helper := filepath.Join(fixture, "helper.sh")
	invocation := fmt.Sprintf("%q prog --url %q --local-dir %q --workers 4 --strict", programBinary, data.URL, privateTempDir(t))
	script := fmt.Sprintf("#!/bin/sh\n%s\nresult=$?\nprintf 'closed\\n' >> %q\nexit \"$result\"\n", invocation, closed)
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := append(cleanEnvironment(), "GOCACHE="+t.TempDir(), "GOCACHEPROG="+helper,
		"GOCACHEPORT_TOKEN=integration-token", "GOTOOLCHAIN=local")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "list", "-p", "4", "-deps", "-export", "-f", "{{if .Export}}{{.ImportPath}}\t{{.Export}}{{end}}", ".")
	command.Dir, command.Env = fixture, environment
	var logs bytes.Buffer
	command.Stderr = &logs
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, &logs)
	}
	if contents, err := os.ReadFile(closed); err != nil || string(contents) != "closed\n" {
		t.Fatalf("go list returned before helper exit: %q, %v", contents, err)
	}
	exports := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		importPath, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("unexpected export listing: %q", line)
		}
		exports[importPath] = path
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("export %s disappeared after helper exit: %v", importPath, err)
		}
		var header [8]byte
		_, readErr := io.ReadFull(file, header[:])
		file.Close()
		if readErr != nil || string(header[:]) != "!<arch>\n" {
			t.Fatalf("export %s is not a Go archive: %q, %v", importPath, header, readErr)
		}
	}
	if exports["fmt"] == "" || exports["example.com/export-fixture"] == "" {
		t.Fatalf("missing independently expected exports: %v", exports)
	}
	before := cache.Snapshot()
	loaded, err := packages.Load(&packages.Config{
		Context: ctx, Dir: fixture, Env: environment, Mode: packages.LoadSyntax, BuildFlags: []string{"-p=4"},
	}, ".")
	if err != nil || len(loaded) != 1 {
		t.Fatalf("go/packages load: %v, packages=%d", err, len(loaded))
	}
	if len(loaded[0].Errors) != 0 || loaded[0].IllTyped || loaded[0].Types == nil {
		t.Fatalf("go/packages could not consume exports: %+v", loaded[0].Errors)
	}
	message := loaded[0].Types.Scope().Lookup("Message")
	if message == nil || message.Type().String() != "func() string" {
		t.Fatalf("go/packages did not resolve fixture types: %v", message)
	}
	after := cache.Snapshot()
	if after.DiskHits+after.MemoryHits <= before.DiskHits+before.MemoryHits {
		t.Fatal("go/packages did not exercise remotely downloaded exports")
	}
	t.Logf("read %d export archives after helper exit; go/packages resolved Message and fmt", len(exports))
}
