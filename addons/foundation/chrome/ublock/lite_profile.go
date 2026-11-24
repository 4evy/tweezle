package ublock

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/4evy/tweezle/addons/foundation/chrome/extensions"
	"github.com/4evy/tweezle/addons/foundation/chrome/storage"
)

const liteInitializationMessage = "install with tweezle apply --no-profile, launch the browser once with Lite installed, then close it and run tweezle profile apply; use ublock.lite.managed for install-time policy"

// ProfileStorageSource patches editable settings only after the installed
// release has run its own first-run and migration code. It never synthesizes
// rulesetConfig.version or defaultRulesetIds
func (config LiteConfig) ProfileStorageSource(profile, installedID string) (storage.SettingsSource, error) {
	settings, err := config.Settings()
	if err != nil || reflect.ValueOf(settings).IsZero() {
		return storage.SettingsSource{}, err
	}
	manifestPath, err := registeredLiteManifest(profile, installedID)
	if err != nil {
		return storage.SettingsSource{}, err
	}
	manifest, err := loadLiteManifest(manifestPath)
	if err != nil {
		return storage.SettingsSource{}, err
	}
	values, err := storage.ReadLocalValues(profile, installedID, "rulesetConfig", "defaultRulesetIds")
	if errors.Is(err, os.ErrNotExist) {
		return storage.SettingsSource{}, errors.New(liteInitializationMessage)
	}
	if err != nil {
		return storage.SettingsSource{}, err
	}
	var initialized struct {
		Version string `json:"version"`
	}
	if data := values["rulesetConfig"]; len(data) != 0 {
		if err := json.Unmarshal(data, &initialized); err != nil {
			return storage.SettingsSource{}, fmt.Errorf("read Lite rulesetConfig: %w", err)
		}
	}
	if initialized.Version != manifest.Version {
		return storage.SettingsSource{}, fmt.Errorf("installed Lite manifest version %q does not match storage version %q: %s", manifest.Version, initialized.Version, liteInitializationMessage)
	}
	var defaults []string
	if slices.Contains(settings.Rulesets, "default") {
		if data := values["defaultRulesetIds"]; len(data) != 0 {
			if err := json.Unmarshal(data, &defaults); err != nil {
				return storage.SettingsSource{}, fmt.Errorf("read Lite defaultRulesetIds: %w", err)
			}
		}
		if defaults == nil {
			return storage.SettingsSource{}, fmt.Errorf("no native default rulesets recorded by Lite: %s", liteInitializationMessage)
		}
	}
	return config.storageSource(settings, manifest, defaults)
}

func registeredLiteManifest(profile, id string) (string, error) {
	if !extensions.ValidExtensionID(id) {
		return "", fmt.Errorf("invalid Lite extension ID %q", id)
	}
	// Chromium tracks the running version in extensions.settings.<id>.path.
	// A newer directory may be an update that is not yet active
	var registration struct {
		Extensions struct {
			Settings map[string]struct {
				Path string `json:"path"`
			} `json:"settings"`
		} `json:"extensions"`
	}
	for _, name := range []string{"Secure Preferences", "Preferences"} {
		file, err := os.Open(filepath.Join(profile, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		registration.Extensions.Settings = nil
		err = errors.Join(json.UnmarshalRead(file, &registration), file.Close())
		if err != nil {
			return "", fmt.Errorf("read extension registration from %s: %w", name, err)
		}
		entry, ok := registration.Extensions.Settings[id]
		if !ok {
			continue
		}
		base, err := filepath.Abs(filepath.Join(profile, "Extensions"))
		if err != nil {
			return "", err
		}
		relative := entry.Path
		if filepath.IsAbs(relative) {
			relative, err = filepath.Rel(base, relative)
			if err != nil {
				return "", err
			}
		}
		if !filepath.IsLocal(relative) || !strings.HasPrefix(filepath.Clean(relative), id+string(filepath.Separator)) {
			return "", fmt.Errorf("extension registration path %q for Lite is outside Extensions/%s", entry.Path, id)
		}
		return filepath.Join(base, relative, "manifest.json"), nil
	}
	return "", fmt.Errorf("no Lite registration in the selected profile: %s", liteInitializationMessage)
}
