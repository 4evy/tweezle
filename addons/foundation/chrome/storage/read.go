package storage

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/4evy/tweezle/addons/foundation/chrome/extensions"
	"github.com/syndtr/goleveldb/leveldb"
)

// ReadLocalValues reads named values from an existing extension store without
// creating storage or repairing an incomplete database
func ReadLocalValues(profile, id string, keys ...string) (map[string]jsontext.Value, error) {
	if !extensions.ValidExtensionID(id) {
		return nil, fmt.Errorf("invalid extension ID %q", id)
	}
	values := make(map[string]jsontext.Value, len(keys))
	err := withExistingStorage(profile, localExtensionSettingsDir, id, func(db *leveldb.DB) error {
		for _, key := range keys {
			value, err := db.Get([]byte(key), nil)
			if errors.Is(err, leveldb.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read %s/%s storage key %q: %w", localExtensionSettingsDir, id, key, err)
			}
			values[key] = value
		}
		return nil
	})
	return values, err
}
