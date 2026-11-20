package prog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/piotrrojek/gocacheport/internal/server"
)

type Config struct {
	URL            string
	Directory      string
	Token          string
	Timeout        time.Duration
	ConnectTimeout time.Duration
	HeaderTimeout  time.Duration
	Workers        int
	MaxObjectBytes int64
	Strict         bool
	Logger         *slog.Logger
}

type Request struct {
	ID       int64
	Command  string
	ActionID []byte
	OutputID []byte
	BodySize int64
}

type Response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

type localEntry struct {
	output  []byte
	size    int64
	created time.Time
	path    string
}

type program struct {
	config     Config
	client     *http.Client
	directory  string
	localMu    sync.Mutex
	local      map[string]localEntry
	remote     *remoteGate
	lastWarn   atomic.Int64
	localHits  atomic.Uint64
	remoteHits atomic.Uint64
	misses     atomic.Uint64
	errors     atomic.Uint64
}

func Run(ctx context.Context, input io.Reader, output io.Writer, config Config) error {
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("cache URL must be HTTP(S), without credentials, query, or fragment")
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = time.Second
	}
	if config.HeaderTimeout == 0 {
		config.HeaderTimeout = time.Second
	}
	if config.Workers < 1 || config.Timeout <= 0 || config.ConnectTimeout <= 0 || config.HeaderTimeout <= 0 || config.MaxObjectBytes < 1 || config.Logger == nil {
		return errors.New("invalid cache program limits")
	}
	if config.Directory != "" {
		config.Directory, err = filepath.Abs(config.Directory)
		if err != nil {
			return err
		}
		if err := privateParent(config.Directory); err != nil {
			return err
		}
	}
	directory, err := os.MkdirTemp(config.Directory, "gocacheport-")
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = config.Workers * 2
	transport.MaxIdleConnsPerHost = config.Workers
	transport.MaxConnsPerHost = config.Workers
	transport.DisableCompression = true
	transport.DialContext = (&net.Dialer{Timeout: config.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = config.ConnectTimeout
	transport.ResponseHeaderTimeout = config.HeaderTimeout
	defer transport.CloseIdleConnections()
	program := &program{
		config: config, directory: directory, local: make(map[string]localEntry),
		remote: newRemoteGate(ctx, config.Timeout, config.Strict),
		client: &http.Client{
			Timeout: config.Timeout, Transport: otelhttp.NewTransport(transport),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	defer program.remote.close()
	defer func() {
		config.Logger.Info("cache program finished", "local_hits", program.localHits.Load(),
			"remote_hits", program.remoteHits.Load(), "misses", program.misses.Load(), "remote_errors", program.errors.Load())
	}()
	encoder := json.NewEncoder(output)
	var outputMu sync.Mutex
	var outputErr error
	respond := func(response Response) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if outputErr == nil {
			outputErr = encoder.Encode(response)
		}
	}
	respond(Response{KnownCommands: []string{"get", "put", "close"}})
	reader := bufio.NewReaderSize(input, 16<<10)
	active := make(chan struct{}, config.Workers)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) == 0 {
			workers.Wait()
			return outputErr
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read protocol header: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var request Request
		if err := json.Unmarshal(line, &request); err != nil {
			return fmt.Errorf("decode protocol header: %w", err)
		}
		if request.Command == "close" {
			workers.Wait()
			respond(Response{ID: request.ID})
			return outputErr
		}
		if request.Command != "get" && request.Command != "put" {
			respond(Response{ID: request.ID, Err: "unsupported command"})
			continue
		}
		if len(request.ActionID) != sha256.Size || request.BodySize < 0 || request.BodySize > 1<<40 {
			return errors.New("invalid protocol action ID or body size")
		}
		var entry localEntry
		if request.Command == "put" {
			if len(request.OutputID) != sha256.Size {
				return errors.New("invalid protocol output ID")
			}
			body := io.Reader(bytes.NewReader(nil))
			if request.BodySize > 0 {
				if err := openingQuote(reader); err != nil {
					return err
				}
				encodedSize := (request.BodySize + 2) / 3 * 4
				body = base64.NewDecoder(base64.StdEncoding, io.LimitReader(reader, encodedSize))
			}
			var stageErr error
			entry, stageErr = program.save(request.OutputID, request.BodySize, time.Now().UTC(), body)
			if stageErr != nil {
				if _, err := io.Copy(io.Discard, body); err != nil {
					return fmt.Errorf("drain protocol body: %w", err)
				}
			}
			if request.BodySize > 0 {
				closing, err := reader.ReadByte()
				if err != nil || closing != '"' {
					return errors.New("missing protocol body closing quote")
				}
			}
			if stageErr != nil {
				respond(Response{ID: request.ID, Err: "local artifact staging failed: " + stageErr.Error()})
				continue
			}
			program.remember(hex.EncodeToString(request.ActionID), entry)
		}
		select {
		case active <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		workers.Go(func() {
			defer func() { <-active }()
			if request.Command == "put" {
				respond(program.put(request, entry))
			} else {
				respond(program.get(request))
			}
		})
	}
}

func openingQuote(reader *bufio.Reader) error {
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if value == '"' {
			return nil
		}
		if value != '\n' && value != '\r' && value != ' ' && value != '\t' {
			return errors.New("missing protocol body opening quote")
		}
	}
}

func (program *program) save(output []byte, size int64, created time.Time, body io.Reader) (localEntry, error) {
	file, err := os.CreateTemp(program.directory, ".tmp-")
	if err != nil {
		return localEntry{}, err
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, size+1))
	if err != nil {
		return localEntry{}, err
	}
	if written != size || !bytes.Equal(digest.Sum(nil), output) {
		return localEntry{}, errors.New("artifact size or SHA-256 mismatch")
	}
	if err := file.Close(); err != nil {
		return localEntry{}, err
	}
	path := filepath.Join(program.directory, hex.EncodeToString(output))
	if err := os.Rename(file.Name(), path); err != nil {
		return localEntry{}, err
	}
	return localEntry{output: bytes.Clone(output), size: size, created: created, path: path}, nil
}

func (program *program) remember(action string, entry localEntry) {
	program.localMu.Lock()
	defer program.localMu.Unlock()
	program.local[action] = entry
}

func (program *program) get(request Request) Response {
	action := hex.EncodeToString(request.ActionID)
	program.localMu.Lock()
	entry, found := program.local[action]
	program.localMu.Unlock()
	if found {
		program.localHits.Add(1)
		return hit(request.ID, entry)
	}
	attempt, allowed := program.remote.begin()
	if !allowed {
		program.misses.Add(1)
		return Response{ID: request.ID, Miss: true}
	}
	unavailable := false
	defer func() { program.remote.finish(attempt, unavailable) }()
	req, err := program.newRequest(attempt.context, http.MethodGet, action, nil)
	if err != nil {
		return program.failure(request.ID, err, true, "")
	}
	response, err := program.client.Do(req)
	if err != nil {
		unavailable = true
		return program.failure(request.ID, err, true, "")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		program.misses.Add(1)
		return Response{ID: request.ID, Miss: true}
	}
	if response.StatusCode != http.StatusOK {
		unavailable = remoteUnavailable(response)
		return program.failure(request.ID, fmt.Errorf("remote GET returned HTTP %d", response.StatusCode), true, "")
	}
	output, err := hex.DecodeString(response.Header.Get(server.OutputHeader))
	if err != nil || len(output) != sha256.Size || response.ContentLength < 0 || response.ContentLength > program.config.MaxObjectBytes {
		return program.failure(request.ID, errors.New("invalid or oversized remote artifact metadata"), true, "")
	}
	created, err := time.Parse(time.RFC3339Nano, response.Header.Get(server.TimeHeader))
	if err != nil {
		return program.failure(request.ID, errors.New("invalid remote artifact timestamp"), true, "")
	}
	entry, err = program.save(output, response.ContentLength, created, response.Body)
	if err != nil {
		unavailable = networkFailure(err)
		return program.failure(request.ID, err, true, "")
	}
	program.localMu.Lock()
	if _, exists := program.local[action]; !exists {
		program.local[action] = entry
	}
	program.localMu.Unlock()
	program.remoteHits.Add(1)
	return hit(request.ID, entry)
}

func hit(id int64, entry localEntry) Response {
	return Response{ID: id, OutputID: entry.output, Size: entry.size, Time: &entry.created, DiskPath: entry.path}
}

func (program *program) put(request Request, entry localEntry) Response {
	if !program.config.Strict && entry.size > program.config.MaxObjectBytes {
		return Response{ID: request.ID, DiskPath: entry.path}
	}
	file, err := os.Open(entry.path)
	if err != nil {
		return Response{ID: request.ID, Err: err.Error()}
	}
	defer file.Close()
	attempt, allowed := program.remote.begin()
	if !allowed {
		return Response{ID: request.ID, DiskPath: entry.path}
	}
	unavailable := false
	defer func() { program.remote.finish(attempt, unavailable) }()
	req, err := program.newRequest(attempt.context, http.MethodPut, hex.EncodeToString(request.ActionID), file)
	if err != nil {
		return program.failure(request.ID, err, false, entry.path)
	}
	req.ContentLength = entry.size
	if entry.size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set(server.OutputHeader, hex.EncodeToString(request.OutputID))
	response, err := program.client.Do(req)
	if err != nil {
		unavailable = true
		return program.failure(request.ID, err, false, entry.path)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		unavailable = remoteUnavailable(response)
		return program.failure(request.ID, fmt.Errorf("remote PUT returned HTTP %d", response.StatusCode), false, entry.path)
	}
	return Response{ID: request.ID, DiskPath: entry.path}
}

func (program *program) newRequest(ctx context.Context, method, action string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(program.config.URL, "/")+"/v1/cache/"+action, body)
	if err != nil {
		return nil, err
	}
	if program.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+program.config.Token)
	}
	return req, nil
}

func (program *program) failure(id int64, err error, miss bool, path string) Response {
	program.errors.Add(1)
	now := time.Now().UnixNano()
	previous := program.lastWarn.Load()
	if now-previous > int64(5*time.Second) && program.lastWarn.CompareAndSwap(previous, now) {
		program.config.Logger.Warn("remote cache request failed", "error", err, "strict", program.config.Strict)
	}
	if program.config.Strict {
		return Response{ID: id, Err: err.Error()}
	}
	if miss {
		program.misses.Add(1)
	}
	return Response{ID: id, Miss: miss, DiskPath: path}
}

func networkFailure(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, io.ErrUnexpectedEOF)
}

func remoteUnavailable(response *http.Response) bool {
	return response.StatusCode >= 500 &&
		!(response.StatusCode == http.StatusServiceUnavailable && response.Header.Get(server.OverloadHeader) == "true")
}
