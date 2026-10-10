//go:build cloudflare

// Package cloudflaresqlite retains the actual SQLite engine with Cloudflare durable pages.
package cloudflaresqlite

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed" // Install the real embedded SQLite engine.
	"github.com/ncruces/go-sqlite3/vfs"
)

const pageSize = 4096

type remoteVFS struct {
	base       vfs.VFS
	dir        string
	endpoint   string
	token      string
	generation string
	onFailure  func()
	owner      string
	epoch      int64
	revision   int64
	client     *http.Client
	mu         sync.Mutex
	failed     atomic.Bool
	notified   atomic.Bool
}

type fileChange struct {
	Name    string       `json:"name"`
	Size    int64        `json:"size"`
	Deleted bool         `json:"deleted,omitempty"`
	Pages   []pageChange `json:"pages"`
}
type pageChange struct {
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}
type metadata struct {
	Epoch    int64 `json:"epoch"`
	Revision int64 `json:"revision"`
	Files    []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// Open creates one SQLite connection with a disposable cache. Main database
// commits are atomic in the storage authority. Rollback journals are scratch
// files; WAL, attached databases and weakened durability settings are rejected.
type Options struct {
	GenerationCapability string
	OnFailure            func()
	TimeFormat           string
}

func Open(endpoint, token string) (*sql.DB, func(), error) {
	return OpenWithOptions(endpoint, token, Options{GenerationCapability: os.Getenv("SQLITE_STORAGE_GENERATION_CAPABILITY")})
}

func OpenWithOptions(endpoint, token string, options Options) (*sql.DB, func(), error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, errors.New("invalid durable SQLite endpoint")
	}
	ip := net.ParseIP(target.Hostname())
	private := target.Hostname() == "rift.sqlite" || target.Hostname() == "config.sqlite" || target.Hostname() == "logs.sqlite" || (ip != nil && ip.IsLoopback())
	_, tokenErr := hex.DecodeString(token)
	_, generationErr := hex.DecodeString(options.GenerationCapability)
	if err != nil || target.Scheme != "http" || !private || target.User != nil || target.RawQuery != "" || target.Fragment != "" || tokenErr != nil || len(token) != 64 || generationErr != nil || len(options.GenerationCapability) != 64 {
		return nil, nil, errors.New("durable SQLite storage configuration required")
	}
	dir, err := os.MkdirTemp("", "durable-sqlite-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	f := &remoteVFS{base: vfs.Find(""), dir: dir, endpoint: strings.TrimRight(endpoint, "/"), token: token, generation: options.GenerationCapability, onFailure: options.OnFailure, owner: uuid(), client: &http.Client{Timeout: 15 * time.Second}}
	var claimed metadata
	if err := f.call(http.MethodPost, "/claim", nil, &claimed); err != nil {
		cleanup()
		return nil, nil, err
	}
	f.epoch, f.revision = claimed.Epoch, claimed.Revision
	if err := f.restore(); err != nil {
		cleanup()
		return nil, nil, err
	}
	name := "cloudflare-" + f.owner
	vfs.Register(name, f)
	timeFormat := options.TimeFormat
	if timeFormat == "" {
		timeFormat = "sqlite"
	}
	dsn := "file:" + filepath.Join(dir, "main.db") + "?vfs=" + name + "&_timefmt=" + url.QueryEscape(timeFormat) + "&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)&_pragma=cache_spill(OFF)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(1000)"
	db, err := driver.Open(dsn, func(conn *sqlite3.Conn) error {
		return conn.SetAuthorizer(func(action sqlite3.AuthorizerActionCode, name, value, _, _ string) sqlite3.AuthorizerReturnCode {
			if f.failed.Load() {
				return sqlite3.AUTH_DENY
			}
			return authorize(action, name, value)
		})
	}, func(*sqlite3.Conn) error {
		// Keep registration until explicit release so a recycled connection calls
		// this VFS, fails closed, and notifies the process rather than opening anew.
		f.failed.Store(true)
		cleanup()
		return nil
	})
	if err != nil {
		vfs.Unregister(name)
		cleanup()
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	release := func() { _ = db.Close(); vfs.Unregister(name); cleanup() }
	if err := db.Ping(); err != nil {
		release()
		return nil, nil, err
	}
	var integrity string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		release()
		return nil, nil, errors.New("durable SQLite integrity check failed")
	}
	return db, release, nil
}

func (f *remoteVFS) call(method, path string, body []byte, result any) error {
	request, err := http.NewRequest(method, f.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("X-Storage-Capability", f.token)
	request.Header.Set("X-Storage-Owner", f.owner)
	request.Header.Set("X-Storage-Generation", f.generation)
	request.Header.Set("X-Storage-Epoch", fmt.Sprint(f.epoch))
	request.Header.Set("X-Storage-Revision", fmt.Sprint(f.revision))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := f.client.Do(request)
	if err != nil {
		// Endpoint validation excludes credentials and query secrets; network errors
		// identify DNS, connection and timeout failures without exposing headers.
		return fmt.Errorf("durable SQLite storage transport failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("durable SQLite storage rejected request: HTTP %d", response.StatusCode)
	}
	if writer, ok := result.(io.Writer); ok {
		_, err = io.Copy(writer, io.LimitReader(response.Body, 256*1024+1))
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(result)
}

func (f *remoteVFS) restore() error {
	var state metadata
	if err := f.call(http.MethodGet, "/meta", nil, &state); err != nil {
		return err
	}
	if state.Epoch != f.epoch || state.Revision != f.revision {
		return errors.New("durable SQLite snapshot changed")
	}
	for _, file := range state.Files {
		if !validName(file.Name) || file.Size < 0 || file.Size > 1024*1024*1024 {
			return errors.New("invalid durable SQLite file metadata")
		}
		local, err := os.OpenFile(filepath.Join(f.dir, file.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		// Bound cold-load concurrency and memory while writing disjoint snapshot
		// ranges. Each request is revision-fenced by the durable authority.
		var next atomic.Int64
		var stopped atomic.Bool
		var first sync.Once
		var loadErr error
		var readers sync.WaitGroup
		for range 8 {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for !stopped.Load() {
					offset := next.Add(256*1024) - 256*1024
					if offset >= file.Size {
						return
					}
					length := min(int64(256*1024), file.Size-offset)
					var buffer bytes.Buffer
					err := f.call(http.MethodGet, fmt.Sprintf("/range?name=%s&offset=%d&length=%d", url.QueryEscape(file.Name), offset, length), nil, &buffer)
					if err == nil && int64(buffer.Len()) != length {
						err = errors.New("durable SQLite snapshot truncated")
					}
					if err == nil {
						_, err = local.WriteAt(buffer.Bytes(), offset)
					}
					if err != nil {
						first.Do(func() { loadErr = err; stopped.Store(true) })
						return
					}
				}
			}()
		}
		readers.Wait()
		if loadErr != nil {
			_ = local.Close()
			return loadErr
		}
		if err := local.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (f *remoteVFS) commit(change fileChange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed.Load() {
		return sqlite3.IOERR
	}
	// Stage bounded batches outside the visible database, then atomically publish
	// every changed page. A crash during staging cannot modify committed state.
	id := uuid()
	sort.Slice(change.Pages, func(i, j int) bool { return change.Pages[i].Offset < change.Pages[j].Offset })
	begin, err := json.Marshal(struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
		Size     int64  `json:"size"`
		Count    int    `json:"count"`
	}{id, f.revision, change.Size, len(change.Pages)})
	if err != nil {
		return sqlite3.IOERR
	}
	var response struct {
		Revision int64 `json:"revision"`
	}
	if err := f.retry(http.MethodPost, "/begin", begin, &response); err != nil {
		f.poison()
		return sqlite3.IOERR
	}
	for start := 0; start < len(change.Pages); start += 128 {
		body, err := json.Marshal(struct {
			ID    string       `json:"id"`
			Pages []pageChange `json:"pages"`
		}{id, change.Pages[start:min(start+128, len(change.Pages))]})
		if err != nil {
			f.poison()
			return sqlite3.IOERR
		}
		if err := f.retry(http.MethodPost, "/stage", body, &response); err != nil {
			f.poison()
			return sqlite3.IOERR
		}
	}
	body, err := json.Marshal(struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}{id, f.revision})
	if err != nil {
		f.poison()
		return sqlite3.IOERR
	}
	response.Revision = 0
	if err := f.retry(http.MethodPost, "/commit", body, &response); err != nil {
		// A lost acknowledgement is retried with the same identity. If recovery is
		// uncertain, no further SQL can use this local cache.
		f.poison()
		return sqlite3.IOERR
	}
	state := metadata{Revision: response.Revision}
	if state.Revision != f.revision+1 {
		f.poison()
		return sqlite3.IOERR
	}
	f.revision = state.Revision
	return nil
}

func (f *remoteVFS) Open(_ string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	return nil, flags, sqlite3.CANTOPEN
}

func (f *remoteVFS) OpenFilename(filename *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	if f.failed.Load() {
		f.poison()
		return nil, flags, sqlite3.IOERR
	}
	name := filename.String()
	if flags&(vfs.OPEN_WAL|vfs.OPEN_SUPER_JOURNAL) != 0 {
		return nil, flags, sqlite3.CANTOPEN
	}
	if flags&vfs.OPEN_MAIN_DB == 0 {
		file, outFlags, err := f.base.(vfs.VFSFilename).OpenFilename(filename, flags)
		if err != nil {
			f.poison()
			return nil, outFlags, err
		}
		return &scratchFile{File: file, owner: f}, outFlags, nil
	}
	if filepath.Base(name) != "main.db" || filepath.Dir(name) != f.dir {
		return nil, flags, sqlite3.CANTOPEN
	}
	file, outFlags, err := f.base.(vfs.VFSFilename).OpenFilename(filename, flags)
	if err != nil {
		return nil, outFlags, err
	}
	return &remoteFile{File: file, owner: f, name: filepath.Base(name), main: flags&vfs.OPEN_MAIN_DB != 0, dirty: make(map[int64]bool)}, outFlags, nil
}
func (f *remoteVFS) FullPathname(name string) (string, error) { return f.base.FullPathname(name) }
func (f *remoteVFS) Access(name string, flags vfs.AccessFlag) (bool, error) {
	return f.base.Access(name, flags)
}
func (f *remoteVFS) Delete(name string, syncDir bool) error {
	if filepath.Base(name) == "main.db" {
		return sqlite3.IOERR_DELETE
	}
	err := f.base.Delete(name, syncDir)
	if err != nil {
		f.poison()
	}
	return err
}

// Scratch failures can occur after a durable main-page commit. Reusing that
// connection could roll its cache back behind the authority; always reload it.
type scratchFile struct {
	vfs.File
	owner *remoteVFS
}

func (f *scratchFile) WriteAt(data []byte, offset int64) (int, error) {
	n, err := f.File.WriteAt(data, offset)
	if err != nil {
		f.owner.poison()
	}
	return n, err
}
func (f *scratchFile) Truncate(size int64) error {
	err := f.File.Truncate(size)
	if err != nil {
		f.owner.poison()
	}
	return err
}
func (f *scratchFile) Sync(flags vfs.SyncFlag) error {
	err := f.File.Sync(flags)
	if err != nil {
		f.owner.poison()
	}
	return err
}
func (f *scratchFile) Close() error {
	err := f.File.Close()
	if err != nil {
		f.owner.poison()
	}
	return err
}

type remoteFile struct {
	vfs.File
	owner   *remoteVFS
	name    string
	main    bool
	atomic  bool
	dirty   map[int64]bool
	changed bool
}

func (f *remoteFile) ReadAt(bytes []byte, offset int64) (int, error) {
	if f.owner.failed.Load() {
		return 0, sqlite3.IOERR
	}
	return f.File.ReadAt(bytes, offset)
}
func (f *remoteFile) WriteAt(bytes []byte, offset int64) (int, error) {
	if f.owner.failed.Load() {
		return 0, sqlite3.IOERR
	}
	if offset < 0 {
		return 0, sqlite3.IOERR
	}
	for page := offset / pageSize * pageSize; page < offset+int64(len(bytes)); page += pageSize {
		f.dirty[page] = true
	}
	f.changed = true
	return f.File.WriteAt(bytes, offset)
}
func (f *remoteFile) Truncate(size int64) error {
	if f.owner.failed.Load() {
		return sqlite3.IOERR
	}
	f.changed = true
	return f.File.Truncate(size)
}
func (f *remoteFile) DeviceCharacteristics() vfs.DeviceCharacteristic {
	if f.main {
		return vfs.IOCAP_BATCH_ATOMIC
	}
	return 0
}
func (f *remoteFile) SectorSize() int { return pageSize }
func (f *remoteFile) Sync(_ vfs.SyncFlag) error {
	if f.atomic {
		return nil
	}
	return f.flush()
}
func (f *remoteFile) BeginAtomicWrite() error {
	if !f.main || f.atomic || f.owner.failed.Load() {
		return sqlite3.IOERR
	}
	f.atomic = true
	return nil
}
func (f *remoteFile) CommitAtomicWrite() error {
	if !f.atomic {
		return sqlite3.IOERR
	}
	err := f.flush()
	f.atomic = false
	return err
}
func (f *remoteFile) RollbackAtomicWrite() error {
	// SQLite may have overwritten local pages. Refuse to reuse that cache;
	// a replacement engine reloads the last durable database.
	f.atomic = false
	f.owner.poison()
	return nil
}
func (f *remoteFile) Lock(lock vfs.LockLevel) error {
	if f.owner.failed.Load() {
		return sqlite3.IOERR
	}
	if f.main && lock == vfs.LOCK_SHARED {
		f.owner.mu.Lock()
		var state metadata
		err := f.owner.retry(http.MethodGet, "/meta", nil, &state)
		if err != nil || state.Epoch != f.owner.epoch || state.Revision != f.owner.revision {
			f.owner.poison()
		}
		f.owner.mu.Unlock()
		if f.owner.failed.Load() {
			return sqlite3.IOERR
		}
	}
	return f.File.Lock(lock)
}
func (f *remoteFile) flush() error {
	if f.owner.failed.Load() {
		return sqlite3.IOERR
	}
	if !f.changed {
		return nil
	}
	size, err := f.Size()
	if err != nil {
		return err
	}
	change := fileChange{Name: f.name, Size: size, Pages: []pageChange{}}
	for offset := range f.dirty {
		if offset >= size {
			continue
		}
		bytes := make([]byte, min(int64(pageSize), size-offset))
		if _, err := f.File.ReadAt(bytes, offset); err != nil {
			return err
		}
		change.Pages = append(change.Pages, pageChange{Offset: offset, Data: base64.StdEncoding.EncodeToString(bytes)})
	}
	if err := f.owner.commit(change); err != nil {
		return err
	}
	clear(f.dirty)
	f.changed = false
	return nil
}

func validName(name string) bool { return name == "main.db" }
func uuid() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("secure randomness unavailable")
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:])
}

// retry preserves request identity across a lost acknowledgement. The authority
// checks replayed stage bytes and returns the original committed revision.
func (f *remoteVFS) retry(method, path string, body []byte, result any) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 100 * time.Millisecond)
		}
		if err = f.call(method, path, body, result); err == nil {
			return nil
		}
	}
	return err
}

func authorize(action sqlite3.AuthorizerActionCode, name, value string) sqlite3.AuthorizerReturnCode {
	if action == sqlite3.AUTH_ATTACH {
		return sqlite3.AUTH_DENY
	}
	if action != sqlite3.AUTH_PRAGMA || value == "" {
		return sqlite3.AUTH_OK
	}
	value = strings.ToLower(value)
	switch strings.ToLower(name) {
	case "journal_mode":
		if value != "delete" {
			return sqlite3.AUTH_DENY
		}
	case "synchronous":
		if value != "full" && value != "extra" && value != "2" && value != "3" {
			return sqlite3.AUTH_DENY
		}
	case "cache_spill":
		if value != "off" && value != "false" && value != "0" {
			return sqlite3.AUTH_DENY
		}
	case "locking_mode":
		if value != "normal" {
			return sqlite3.AUTH_DENY
		}
	case "page_size":
		if value != "4096" {
			return sqlite3.AUTH_DENY
		}
	case "writable_schema", "journal_size_limit":
		return sqlite3.AUTH_DENY
	}
	return sqlite3.AUTH_OK
}

func (f *remoteVFS) poison() {
	f.failed.Store(true)
	if f.notified.CompareAndSwap(false, true) && f.onFailure != nil {
		go f.onFailure()
	}
}
