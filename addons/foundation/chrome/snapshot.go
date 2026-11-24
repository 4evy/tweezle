package chromium

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/4evy/tweezle/addons/foundation/chrome/extensions"
	"github.com/4evy/tweezle/internal/profilestage"
)

const extensionStageProfile = "Default"

func extensionIndexedDBPaths(id string) (levelDB, blob, sqlite string) {
	origin := "chrome-extension_" + id + "_0"
	// SQLite uses the bare serialized origin directory, including sidecars
	// https://chromium.googlesource.com/chromium/src/+/main/content/browser/indexed_db/file_path_util.cc
	return filepath.Join("IndexedDB", origin+".indexeddb.leveldb"),
		filepath.Join("IndexedDB", origin+".indexeddb.blob"),
		filepath.Join("IndexedDB", origin)
}

type indexedDBBackend uint8

const (
	indexedDBLevelDB indexedDBBackend = 1 << iota
	indexedDBSQLite
)

func extensionIndexedDBBackend(profile, id string) (indexedDBBackend, error) {
	levelDB, _, sqlite := extensionIndexedDBPaths(id)
	var backend indexedDBBackend
	for _, store := range [...]struct {
		path string
		kind indexedDBBackend
	}{{levelDB, indexedDBLevelDB}, {sqlite, indexedDBSQLite}} {
		entries, err := os.ReadDir(filepath.Join(profile, store.path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if len(entries) != 0 {
			backend |= store.kind
		}
	}
	return backend, nil
}

type extensionStorageStage struct {
	stage   *profilestage.Stage
	profile string
	root    string
	paths   []string
}

func stageExtensionStorage(profile, id string) (*extensionStorageStage, error) {
	if !extensions.ValidExtensionID(id) {
		return nil, fmt.Errorf("invalid extension ID %q", id)
	}
	levelDB, blob, sqlite := extensionIndexedDBPaths(id)
	stage := &extensionStorageStage{
		profile: profile,
		paths: []string{
			filepath.Join("Local Extension Settings", id),
			filepath.Join("Sync Extension Settings", id),
			levelDB, blob, sqlite,
		},
	}
	snapshot, err := profilestage.NewDirectoryStores(profile, stage.paths, extensionStageProfile)
	if err != nil {
		return nil, err
	}
	stage.stage = snapshot
	stage.root = snapshot.Root()
	return stage, nil
}

func (stage *extensionStorageStage) remove() {
	stage.stage.Remove()
}

func (stage *extensionStorageStage) commit(profile string, checks ...func(string) error) error {
	if filepath.Clean(profile) != filepath.Clean(stage.profile) {
		return fmt.Errorf("extension storage stage belongs to %s, not %s", stage.profile, profile)
	}
	return stage.stage.Commit(func() error {
		if len(checks) == 0 {
			return EnsureProfileNotRunning(profile)
		}
		for _, check := range checks {
			if check == nil {
				return errors.New("profile lock check is required")
			}
			if err := check(profile); err != nil {
				return err
			}
		}
		return nil
	})
}
