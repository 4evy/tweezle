package zen

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/4evy/tweezle/addons/firefox"
	jsonutil "github.com/4evy/tweezle/internal/json"
	"github.com/Jeffail/gabs/v2"
)

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	object, err := jsonutil.Default.Decode[map[string]any](bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must contain an object", path)
	}
	return object, nil
}
func objectFile(path string, object map[string]any) (firefox.ProfileFile, error) {
	data, err := json.Marshal(object, json.Deterministic(true))
	return firefox.ProfileFile{Path: path, Data: data}, err
}
func patchObject(object, patch map[string]any) error {
	return gabs.Wrap(object).MergeFn(
		gabs.Wrap(patch),
		func(_, source any) any { return source },
	)
}

// Zen's shortcut categories pair native <key> IDs with their label IDs
// https://github.com/zen-browser/desktop/blob/1.24.3b/src/zen/kbs/ZenKeyboardShortcuts.sys.mjs#L44-L90
var shortcutIDs = map[string]string{
	"zen-workspace-shortcut-create":              "zen-workspace-create",
	"zen-toggle-pin-tab-shortcut":                "zen-toggle-pin-tab",
	"zen-pinned-tab-shortcut-reset":              "zen-pinned-tab-reset-shortcut",
	"zen-duplicate-tab-shortcut":                 "zen-duplicate-tab",
	"zen-close-all-unpinned-tabs-shortcut":       "zen-close-all-unpinned-tabs",
	"zen-text-action-copy-url-shortcut":          "zen-copy-url",
	"zen-text-action-copy-url-markdown-shortcut": "zen-copy-url-markdown",
	"zen-split-view-shortcut-grid":               "zen-split-view-grid",
	"zen-split-view-shortcut-vertical":           "zen-split-view-vertical",
	"zen-split-view-shortcut-horizontal":         "zen-split-view-horizontal",
	"zen-split-view-shortcut-unsplit":            "zen-split-view-unsplit",
	"zen-new-empty-split-view-shortcut":          "zen-new-empty-split-view",
	"zen-compact-mode-shortcut-toggle":           "zen-compact-mode-toggle",
	"zen-compact-mode-shortcut-show-sidebar":     "zen-compact-mode-show-sidebar",
	"zen-sidebar-shortcut-toggle":                "zen-toggle-sidebar",
	"zen-library-shortcut-toggle":                "zen-library-toggle",
}

func shortcutID(id string) string {
	if native, ok := shortcutIDs[id]; ok {
		return native
	}
	if index, ok := strings.CutPrefix(id, "zen-workspace-shortcut-switch-"); ok {
		switch index {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "10":
			return "zen-workspace-switch-" + index
		}
	}
	return id
}

func (config Config) shortcuts(profile firefox.Profile) (firefox.ProfileFile, error) {
	path := filepath.Join(profile.Directory, "customKeys.json")
	object, err := readObject(path)
	if errors.Is(err, os.ErrNotExist) {
		object = map[string]any{}
	} else if err != nil {
		return firefox.ProfileFile{}, err
	}
	found := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(config.Shortcuts)) {
		patch := config.Shortcuts[id]
		id = shortcutID(id)
		if found[id] {
			return firefox.ProfileFile{}, fmt.Errorf("shortcut %q is configured via multiple IDs", id)
		}
		found[id] = true
		if len(patch) == 0 {
			// CustomKeys uses an empty binding to clear a default shortcut
			object[id] = map[string]any{}
			continue
		}
		binding, ok := object[id].(map[string]any)
		if !ok {
			if _, exists := object[id]; exists {
				return firefox.ProfileFile{}, fmt.Errorf("invalid native Zen shortcut entry %q", id)
			}
			binding = map[string]any{}
		}
		if _, configured := patch["key"]; configured {
			delete(binding, "keycode")
		}
		if _, configured := patch["keycode"]; configured {
			delete(binding, "key")
		}
		for name, value := range patch {
			if name == "modifiers" && value == "" {
				delete(binding, name)
			} else {
				binding[name] = value
			}
		}
		key, _ := binding["key"].(string)
		keycode, _ := binding["keycode"].(string)
		if key == "" && keycode == "" {
			return firefox.ProfileFile{}, fmt.Errorf("shortcut %q needs a key or keycode before setting modifiers", id)
		}
		object[id] = binding
	}
	return objectFile(path, object)
}

func (config Config) workspaces(profile firefox.Profile) (firefox.ProfileFile, error) {
	path := filepath.Join(profile.Directory, "zen-sessions.jsonlz4")
	encoded, err := os.ReadFile(path)
	if err != nil {
		return firefox.ProfileFile{}, fmt.Errorf("open Zen once to initialize zen-sessions.jsonlz4: %w", err)
	}
	data, err := decodeSession(encoded)
	if err != nil {
		return firefox.ProfileFile{}, err
	}
	object, err := jsonutil.Default.Decode[map[string]any](bytes.NewReader(data))
	if err != nil {
		return firefox.ProfileFile{}, err
	}
	spaces, ok := object["spaces"].([]any)
	if !ok || len(spaces) == 0 {
		return firefox.ProfileFile{}, errors.New("zen session must contain initialized spaces")
	}
	found := map[string]any{}
	for _, entry := range spaces {
		space, ok := entry.(map[string]any)
		if !ok {
			return firefox.ProfileFile{}, errors.New("invalid Zen space entry")
		}
		id, _ := space["uuid"].(string)
		if !validWorkspaceID(id) {
			return firefox.ProfileFile{}, fmt.Errorf("invalid native Zen workspace UUID %q", id)
		}
		if _, exists := found[id]; exists {
			return firefox.ProfileFile{}, fmt.Errorf("duplicate Zen workspace UUID %s", id)
		}
		found[id] = space
		if patch, configured := config.Workspaces[id]; configured {
			if err := patchObject(space, patch); err != nil {
				return firefox.ProfileFile{}, err
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(config.Workspaces)) {
		if _, exists := found[id]; exists {
			continue
		}
		patch := config.Workspaces[id]
		if name, _ := patch["name"].(string); name == "" {
			return firefox.ProfileFile{}, fmt.Errorf("new Zen workspace %s requires name", id)
		}
		space := map[string]any{"uuid": id, "name": "", "icon": nil, "containerTabId": 0, "theme": map[string]any{"type": "gradient", "gradientColors": []any{}, "opacity": 0.5, "texture": 0}}
		if err := patchObject(space, patch); err != nil {
			return firefox.ProfileFile{}, err
		}
		spaces = append(spaces, space)
		found[id] = space
	}
	if len(config.WorkspaceOrder) > 0 {
		ordered := make([]any, 0, len(spaces))
		selected := map[string]bool{}
		for _, id := range config.WorkspaceOrder {
			space, exists := found[id]
			if !exists {
				return firefox.ProfileFile{}, fmt.Errorf("workspace_order UUID %s is absent", id)
			}
			ordered = append(ordered, space)
			selected[id] = true
		}
		for _, entry := range spaces {
			id, _ := entry.(map[string]any)["uuid"].(string)
			if !selected[id] {
				ordered = append(ordered, entry)
			}
		}
		spaces = ordered
	}
	object["spaces"] = spaces
	data, err = json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return firefox.ProfileFile{}, err
	}
	encoded, err = encodeSession(data)
	return firefox.ProfileFile{Path: path, Data: encoded}, err
}
