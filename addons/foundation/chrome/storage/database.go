package storage

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	fileutil "github.com/4evy/tweezle/internal/file"
	jsonutil "github.com/4evy/tweezle/internal/json"
	lzstring "github.com/daku10/go-lz-string"
	"github.com/syndtr/goleveldb/leveldb"
	leveldberrors "github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

func (area Area) directory() (string, error) {
	switch area {
	case AreaLocal:
		return localExtensionSettingsDir, nil
	case AreaSync:
		return syncExtensionSettingsDir, nil
	default:
		return "", fmt.Errorf("unsupported storage area %q", area)
	}
}

func rawJSONValue(raw jsontext.Value) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return jsonutil.Default.Decode[any](bytes.NewReader(raw))
}

func decodeStorageValue(raw []byte, encoding Encoding) (any, error) {
	if encoding == EncodingLZStringURI {
		var compressed string
		if err := json.Unmarshal(raw, &compressed); err != nil {
			return nil, err
		}
		decoded, err := lzstring.DecompressFromEncodedURIComponent(compressed)
		if err != nil {
			return nil, err
		}
		raw = []byte(decoded)
	} else if encoding != EncodingJSON {
		return nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return jsonutil.Default.Decode[any](bytes.NewReader(raw))
}

func encodeStorageValue(document any, encoding Encoding) ([]byte, error) {
	encoded, err := json.Marshal(document, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if encoding == EncodingJSON {
		return encoded, nil
	}
	if encoding != EncodingLZStringURI {
		return nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
	compressed, err := lzstring.CompressToEncodedURIComponent(string(encoded))
	if err != nil {
		return nil, err
	}
	return json.Marshal(compressed, json.Deterministic(true))
}

func validateSyncStorageState(state map[string][]byte) error {
	totalBytes := 0
	for _, key := range slices.Sorted(maps.Keys(state)) {
		value := state[key]
		size := len(key) + len(value)
		if size > syncStorageQuotaBytesPerItem {
			return fmt.Errorf(
				"sync item %q uses %d bytes, exceeding the %d-byte limit",
				key,
				size,
				syncStorageQuotaBytesPerItem,
			)
		}
		totalBytes += size
	}
	if len(state) > syncStorageMaxItems {
		return fmt.Errorf(
			"sync storage contains %d items, exceeding the %d-item limit",
			len(state),
			syncStorageMaxItems,
		)
	}
	if totalBytes > syncStorageQuotaBytes {
		return fmt.Errorf(
			"sync storage uses %d bytes, exceeding the %d-byte limit",
			totalBytes,
			syncStorageQuotaBytes,
		)
	}
	return nil
}

var openDatabases sync.Map

type databaseDirectory struct {
	device uint64
	inode  uint64
}

func withStorage(
	profileDir,
	area,
	extensionID string,
	operation func(*leveldb.DB) error,
) error {
	return withStorageMode(profileDir, area, extensionID, false, operation)
}

func withExistingStorage(profileDir, area, extensionID string, operation func(*leveldb.DB) error) error {
	return withStorageMode(profileDir, area, extensionID, true, operation)
}

func withStorageMode(profileDir, area, extensionID string, readOnly bool, operation func(*leveldb.DB) error) (err error) {
	// Chromium uses one LevelDB directory per extension and storage area. Open
	// the database directly only while the browser is closed: LevelDB permits a
	// single writer and Chromium keeps these stores locked while running.
	// Source: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/extensions/browser/api/storage/value_store_util.cc#17
	path := filepath.Join(profileDir, area, extensionID)
	if !readOnly {
		if err := os.MkdirAll(path, fileutil.DefaultDirPerm); err != nil {
			return fmt.Errorf("create storage directory %s: %w", path, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("storage path is not a directory: %s", path)
	}
	stat := info.Sys().(*syscall.Stat_t)
	key := databaseDirectory{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
	// Reject a second in-process open before opening any lock descriptors
	if _, loaded := openDatabases.LoadOrStore(key, struct{}{}); loaded {
		return fmt.Errorf("lock storage %s: %w", path, storage.ErrLocked)
	}
	defer openDatabases.Delete(key)
	lock, err := lockChromiumLevelDB(path, readOnly)
	if err != nil {
		return fmt.Errorf("lock storage %s: %w", path, err)
	}
	if lock != nil {
		defer func() { err = errors.Join(err, lock.Close()) }()
	}
	database, err := leveldb.OpenFile(path, &opt.Options{ReadOnly: readOnly, ErrorIfMissing: readOnly})
	if err != nil {
		return fmt.Errorf("open storage %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, database.Close()) }()
	return operation(database)
}

func lockChromiumLevelDB(path string, readOnly bool) (*os.File, error) {
	// Linux keeps flock and fcntl locks separate. Hold both until goleveldb
	// has closed all files and stopped its background work
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if !readOnly {
		flags |= os.O_CREATE
	}
	name := filepath.Join(path, "LOCK")
	if info, err := os.Lstat(name); err == nil {
		if !validLockFile(info) {
			return nil, errors.New("LevelDB LOCK must be a regular file without hard links")
		}
	} else if readOnly || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Darwin coordinates both lock types, so goleveldb's flock is sufficient.
	// Taking an extra record lock there would make goleveldb block itself
	if levelDBRecordLockCommand == -1 {
		return nil, nil
	}
	lock, err := os.OpenFile(name, flags, fileutil.DefaultFilePerm)
	if err != nil {
		return nil, err
	}
	// A standard LevelDB LOCK has one name. Refuse aliases that would evade the
	// in-process guard and cause a second descriptor to release the first lock
	info, err := lock.Stat()
	if err == nil && !validLockFile(info) {
		err = errors.New("LevelDB LOCK must be a regular file without hard links")
	}
	if err == nil {
		err = syscall.FcntlFlock(lock.Fd(), levelDBRecordLockCommand, &syscall.Flock_t{
			Type: syscall.F_WRLCK, Whence: 0,
		})
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
			err = errors.Join(storage.ErrLocked, err)
		}
	}
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}

func validLockFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Sys().(*syscall.Stat_t).Nlink == 1
}

// IsTemporarilyUnavailable reports whether Chromium storage should be retried
// after the browser releases its LevelDB files.
func IsTemporarilyUnavailable(err error) bool {
	if errors.Is(err, storage.ErrLocked) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	corrupted, ok := errors.AsType[*leveldberrors.ErrCorrupted](err)
	if !ok {
		return false
	}
	_, ok = errors.AsType[*leveldberrors.ErrMissingFiles](corrupted.Err)
	return ok
}
