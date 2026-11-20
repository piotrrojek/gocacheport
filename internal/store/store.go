package store

import (
	"bytes"
	"container/heap"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const headerSize = 56

var (
	ErrMiss      = errors.New("cache miss")
	ErrTooLarge  = errors.New("artifact exceeds cache limit")
	ErrIntegrity = errors.New("artifact size or SHA-256 mismatch")
)

type Config struct {
	Directory         string
	DiskBytes         int64
	MaxEntries        int
	MaxObjectBytes    int64
	MemoryBytes       int64
	MemoryObjectBytes int64
	TTL               time.Duration
}

type Metadata struct {
	OutputID [sha256.Size]byte
	Size     int64
	Created  time.Time
}

type Artifact struct {
	Metadata
	Body io.ReadCloser
	Tier string
}

type Stats struct {
	DiskBytes, DiskEntries, MemoryBytes, MemoryEntries int64
	MemoryHits, DiskHits, Misses, Writes, Evictions    uint64
	MemoryEvictions, Corruptions                       uint64
}

type record struct {
	key string
	Metadata
}

type recordHeap []record

func (records recordHeap) Len() int { return len(records) }
func (records recordHeap) Less(left, right int) bool {
	return records[left].Created.Before(records[right].Created)
}

func (records recordHeap) Swap(left, right int) {
	records[left], records[right] = records[right], records[left]
}
func (records *recordHeap) Push(value any) { *records = append(*records, value.(record)) }
func (records *recordHeap) Pop() any {
	last := len(*records) - 1
	value := (*records)[last]
	*records = (*records)[:last]
	return value
}

type memoryRecord struct {
	key string
	Metadata
	body []byte
}

type Store struct {
	mu      sync.Mutex
	root    *os.Root
	lock    *os.File
	config  Config
	disk    map[string]*list.Element
	diskLRU list.List
	memory  map[string]*list.Element
	memLRU  list.List
	stats   Stats
	closed  bool
}

func Open(config Config) (*Store, error) {
	if config.Directory == "" || config.DiskBytes <= headerSize || config.MaxEntries < 1 ||
		config.MaxObjectBytes < 1 || config.MaxObjectBytes > config.DiskBytes-headerSize ||
		config.MemoryBytes < 0 || config.MemoryObjectBytes < 0 || config.TTL < 0 {
		return nil, errors.New("invalid storage limits or directory")
	}
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		return nil, err
	}
	lock, err := lockDirectory(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	store := &Store{
		root: root, lock: lock, config: config,
		disk: make(map[string]*list.Element), memory: make(map[string]*list.Element),
	}
	if err := store.load(); err != nil {
		store.Close()
		return nil, fmt.Errorf("load cache inventory: %w", err)
	}
	return store, nil
}

func ValidID(key string) bool {
	if len(key) != sha256.Size*2 || key != strings.ToLower(key) {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}

func filename(key string) string {
	return key[:2] + "/" + key
}

func (store *Store) load() error {
	var records recordHeap
	err := store.walkBatches(".", func(entry fs.DirEntry) error {
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-") {
			return store.root.Remove(name)
		}
		if !entry.IsDir() || len(name) != 2 {
			return nil
		}
		return store.walkBatches(name, func(candidate fs.DirEntry) error {
			key := candidate.Name()
			if !ValidID(key) || key[:2] != name {
				return nil
			}
			file, err := openRegular(store.root, filename(key))
			var metadata Metadata
			if err == nil {
				metadata, err = readHeader(file)
				file.Close()
			}
			if err != nil || store.expired(metadata.Created) || metadata.Size > store.config.MaxObjectBytes {
				store.stats.Corruptions += boolCount(err != nil)
				return store.root.Remove(filename(key))
			}
			heap.Push(&records, record{key: key, Metadata: metadata})
			store.stats.DiskBytes += metadata.Size + headerSize
			for len(records) > store.config.MaxEntries || store.stats.DiskBytes > store.config.DiskBytes {
				oldest := heap.Pop(&records).(record)
				if err := store.root.Remove(filename(oldest.key)); err != nil {
					return err
				}
				store.stats.DiskBytes -= oldest.Size + headerSize
				store.stats.Evictions++
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	slices.SortFunc(records, func(left, right record) int {
		return left.Created.Compare(right.Created)
	})
	for _, entry := range records {
		store.disk[entry.key] = store.diskLRU.PushFront(entry)
	}
	return nil
}

func (store *Store) walkBatches(name string, visit func(fs.DirEntry) error) error {
	directory, err := openDirectory(store.root, name)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, err := directory.ReadDir(128)
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func boolCount(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func readHeader(file *os.File) (Metadata, error) {
	info, err := file.Stat()
	if err != nil {
		return Metadata{}, err
	}
	if !info.Mode().IsRegular() {
		return Metadata{}, ErrIntegrity
	}
	var header [headerSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return Metadata{}, err
	}
	if string(header[:8]) != "GCPORT01" {
		return Metadata{}, ErrIntegrity
	}
	metadata := Metadata{
		OutputID: [sha256.Size]byte(header[8:40]),
		Size:     int64(binary.BigEndian.Uint64(header[40:48])),
		Created:  time.Unix(0, int64(binary.BigEndian.Uint64(header[48:56]))).UTC(),
	}
	if metadata.Size < 0 || metadata.Size > info.Size() ||
		info.Size()-metadata.Size != headerSize || metadata.Created.After(time.Now().Add(time.Minute)) {
		return Metadata{}, ErrIntegrity
	}
	return metadata, nil
}

func writeHeader(file *os.File, metadata Metadata) error {
	var header [headerSize]byte
	copy(header[:8], "GCPORT01")
	copy(header[8:40], metadata.OutputID[:])
	binary.BigEndian.PutUint64(header[40:48], uint64(metadata.Size))
	binary.BigEndian.PutUint64(header[48:56], uint64(metadata.Created.UnixNano()))
	_, err := file.Write(header[:])
	return err
}

func (store *Store) expired(created time.Time) bool {
	return store.config.TTL > 0 && time.Since(created) >= store.config.TTL
}

func (store *Store) Get(key string) (Artifact, error) {
	if !ValidID(key) {
		return Artifact{}, ErrMiss
	}
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return Artifact{}, os.ErrClosed
	}
	element, ok := store.disk[key]
	if !ok {
		store.stats.Misses++
		store.mu.Unlock()
		return Artifact{}, ErrMiss
	}
	entry := element.Value.(record)
	if store.expired(entry.Created) {
		if err := store.remove(element); err != nil {
			store.mu.Unlock()
			return Artifact{}, err
		}
		store.stats.Misses++
		store.mu.Unlock()
		return Artifact{}, ErrMiss
	}
	store.diskLRU.MoveToFront(element)
	if cached, ok := store.memory[key]; ok {
		store.memLRU.MoveToFront(cached)
		store.stats.MemoryHits++
		memory := cached.Value.(memoryRecord)
		store.mu.Unlock()
		return Artifact{Metadata: memory.Metadata, Body: io.NopCloser(bytes.NewReader(memory.body)), Tier: "memory"}, nil
	}
	file, err := openRegular(store.root, filename(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			store.forget(element)
			store.stats.Misses++
			store.mu.Unlock()
			return Artifact{}, ErrMiss
		}
		store.mu.Unlock()
		if errors.Is(err, ErrIntegrity) {
			return store.corrupt(key, element)
		}
		return Artifact{}, err
	}
	metadata, err := readHeader(file)
	store.mu.Unlock()
	if err != nil || metadata != entry.Metadata {
		file.Close()
		return store.corrupt(key, element)
	}
	var body []byte
	valid := false
	if store.cacheable(metadata.Size) {
		body, err = io.ReadAll(io.LimitReader(file, metadata.Size+1))
		valid = err == nil && int64(len(body)) == metadata.Size && sha256.Sum256(body) == metadata.OutputID
	} else {
		digest := sha256.New()
		var copied int64
		copied, err = io.Copy(digest, io.LimitReader(file, metadata.Size+1))
		valid = err == nil && copied == metadata.Size && bytes.Equal(digest.Sum(nil), metadata.OutputID[:])
	}
	if !valid {
		file.Close()
		return store.corrupt(key, element)
	}
	store.mu.Lock()
	store.stats.DiskHits++
	if body != nil && store.disk[key] == element {
		if _, exists := store.memory[key]; !exists {
			store.remember(key, metadata, body)
		}
	}
	store.mu.Unlock()
	if body != nil {
		file.Close()
		return Artifact{Metadata: metadata, Body: io.NopCloser(bytes.NewReader(body)), Tier: "disk"}, nil
	}
	if _, err := file.Seek(headerSize, io.SeekStart); err != nil {
		file.Close()
		return Artifact{}, err
	}
	return Artifact{Metadata: metadata, Body: file, Tier: "disk"}, nil
}

func (store *Store) corrupt(key string, generation *list.Element) (Artifact, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.stats.Corruptions++
	store.stats.Misses++
	if store.disk[key] == generation {
		if err := store.remove(generation); err != nil {
			return Artifact{}, err
		}
	}
	return Artifact{}, ErrMiss
}

func (store *Store) Put(ctx context.Context, key string, output [sha256.Size]byte, size int64, body io.Reader) (Metadata, error) {
	if !ValidID(key) || size < 0 {
		return Metadata{}, ErrIntegrity
	}
	if size > store.config.MaxObjectBytes {
		return Metadata{}, ErrTooLarge
	}
	metadata := Metadata{OutputID: output, Size: size, Created: time.Now().UTC()}
	temporary := ".tmp-" + rand.Text()
	file, err := store.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return Metadata{}, err
	}
	defer func() {
		file.Close()
		store.root.Remove(temporary)
	}()
	if err := writeHeader(file, metadata); err != nil {
		return Metadata{}, err
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, size+1))
	if err != nil {
		return Metadata{}, err
	}
	if written != size || !bytes.Equal(digest.Sum(nil), output[:]) {
		return Metadata{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := file.Sync(); err != nil {
		return Metadata{}, err
	}
	if err := file.Close(); err != nil {
		return Metadata{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return Metadata{}, os.ErrClosed
	}
	additionalBytes, additionalEntries := size+headerSize, 1
	existing := store.disk[key]
	if existing != nil {
		additionalBytes -= existing.Value.(record).Size + headerSize
		additionalEntries = 0
		store.diskLRU.MoveToFront(existing)
	}
	if err := store.makeRoom(additionalBytes, additionalEntries); err != nil {
		return Metadata{}, err
	}
	if err := store.root.MkdirAll(key[:2], 0o750); err != nil {
		return Metadata{}, err
	}
	if err := store.root.Rename(temporary, filename(key)); err != nil {
		return Metadata{}, err
	}
	if existing != nil {
		store.forget(existing)
	}
	store.disk[key] = store.diskLRU.PushFront(record{key: key, Metadata: metadata})
	store.stats.DiskBytes += size + headerSize
	store.stats.Writes++
	return metadata, nil
}

func (store *Store) cacheable(size int64) bool {
	return size <= store.config.MemoryObjectBytes && size+256 <= store.config.MemoryBytes
}

func (store *Store) remember(key string, metadata Metadata, body []byte) {
	for store.stats.MemoryBytes+metadata.Size+256 > store.config.MemoryBytes {
		oldest := store.memLRU.Back()
		store.forgetMemory(oldest.Value.(memoryRecord).key)
		store.stats.MemoryEvictions++
	}
	store.memory[key] = store.memLRU.PushFront(memoryRecord{key: key, Metadata: metadata, body: body})
	store.stats.MemoryBytes += metadata.Size + 256
}

func (store *Store) forgetMemory(key string) {
	if element, ok := store.memory[key]; ok {
		store.stats.MemoryBytes -= element.Value.(memoryRecord).Size + 256
		store.memLRU.Remove(element)
		delete(store.memory, key)
	}
}

func (store *Store) forget(element *list.Element) {
	entry := element.Value.(record)
	store.stats.DiskBytes -= entry.Size + headerSize
	store.diskLRU.Remove(element)
	delete(store.disk, entry.key)
	store.forgetMemory(entry.key)
}

func (store *Store) remove(element *list.Element) error {
	err := store.root.Remove(filename(element.Value.(record).key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	store.forget(element)
	return nil
}

func (store *Store) makeRoom(bytes int64, entries int) error {
	for store.stats.DiskBytes+bytes > store.config.DiskBytes || len(store.disk)+entries > store.config.MaxEntries {
		if err := store.remove(store.diskLRU.Back()); err != nil {
			return err
		}
		store.stats.Evictions++
	}
	return nil
}

func (store *Store) Sweep() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return os.ErrClosed
	}
	for _, element := range store.disk {
		if store.expired(element.Value.(record).Created) {
			if err := store.remove(element); err != nil {
				return err
			}
			store.stats.Evictions++
		}
	}
	return nil
}

func (store *Store) Snapshot() Stats {
	store.mu.Lock()
	defer store.mu.Unlock()
	stats := store.stats
	stats.DiskEntries = int64(len(store.disk))
	stats.MemoryEntries = int64(len(store.memory))
	return stats
}

func (store *Store) Ready() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return os.ErrClosed
	}
	_, err := store.root.Stat(".")
	return err
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return errors.Join(store.lock.Close(), store.root.Close())
}
