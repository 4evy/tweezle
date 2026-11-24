// Package zen implements Zen preferences, shortcut overrides, Mods, and spaces
package zen

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"

	"github.com/4evy/tweezle/addons/firefox"
	"github.com/4evy/tweezle/addons/foundation/firefox/settings"
	jsonutil "github.com/4evy/tweezle/internal/json"
)

//go:embed metadata.json
var embeddedMetadata []byte
var catalog = settings.LoadCompiled(embeddedMetadata)

// schemas.json describes native shortcut and workspace patch fields
//
//go:embed schemas.json
var embeddedSchemas []byte

var (
	shortcutSchema, workspaceSchema = loadSchemas()
	uuid                            = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	modRevision                     = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	modStyleSHA256                  = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
)

func validWorkspaceID(id string) bool {
	if len(id) == 38 && id[0] == '{' && id[len(id)-1] == '}' {
		id = id[1 : len(id)-1]
	}
	return uuid.MatchString(id)
}

type Config struct {
	Preferences    map[string]any            `toml:"preferences"`
	Shortcuts      map[string]map[string]any `toml:"shortcuts"`
	Mods           map[string]Mod            `toml:"mods"`
	Workspaces     map[string]map[string]any `toml:"workspaces"`
	WorkspaceOrder []string                  `toml:"workspace_order"`
}
type Mod struct {
	Enabled     *bool          `toml:"enabled"`
	Revision    string         `toml:"revision"`
	StyleSHA256 string         `toml:"style_sha256"`
	Preferences map[string]any `toml:"preferences"`
}

func Addon() firefox.ProviderAddon { return firefox.NewProviderAddon[Config]("zen") }
func (Config) Variant() string     { return "zen" }
func Metadata() settings.Catalog   { return settings.Load(embeddedMetadata) }
func (config Config) Validate() error {
	var errs []error
	errs = append(errs, catalog.Validate(config.Preferences))
	shortcutNames := make(map[string]string, len(config.Shortcuts))
	for id, patch := range config.Shortcuts {
		if id == "" {
			errs = append(errs, errors.New("shortcut ID must not be empty"))
		}
		errs = append(errs, shortcutSchema.Validate(patch, "shortcuts."+id))
		native := shortcutID(id)
		if previous, duplicate := shortcutNames[native]; duplicate {
			errs = append(errs, fmt.Errorf("shortcut IDs %q and %q target %q", previous, id, native))
		}
		shortcutNames[native] = id
		if _, key := patch["key"]; key {
			if _, keycode := patch["keycode"]; keycode {
				errs = append(errs, fmt.Errorf("shortcut %s cannot set both key and keycode", id))
			}
		}
	}
	for id, patch := range config.Workspaces {
		if !validWorkspaceID(id) {
			errs = append(errs, fmt.Errorf("invalid workspace UUID %q", id))
		}
		errs = append(errs, workspaceSchema.Validate(patch, "workspaces."+id))
	}
	seen := map[string]bool{}
	for _, id := range config.WorkspaceOrder {
		if !validWorkspaceID(id) || seen[id] {
			errs = append(errs, fmt.Errorf("invalid or duplicate workspace_order UUID %q", id))
		}
		seen[id] = true
	}
	for id, mod := range config.Mods {
		if !uuid.MatchString(id) {
			errs = append(errs, fmt.Errorf("invalid Mod UUID %q", id))
		}
		if mod.Revision != "" && !modRevision.MatchString(mod.Revision) {
			errs = append(errs, fmt.Errorf("zen Mod %s revision must be a full theme-store commit SHA", id))
		}
		if mod.StyleSHA256 != "" && !modStyleSHA256.MatchString(mod.StyleSHA256) {
			errs = append(errs, fmt.Errorf("zen Mod %s style_sha256 must have 64 hex digits", id))
		}
		for name, value := range mod.Preferences {
			if name == "" {
				errs = append(errs, errors.New("zen Mod preference name must not be empty"))
			}
			switch value.(type) {
			case bool, string:
			default:
				errs = append(errs, fmt.Errorf("zen Mod preference %s must be boolean or string", name))
			}
		}
	}
	return errors.Join(errs...)
}
func (config Config) Prepare(profile firefox.Profile) (firefox.Contribution, error) {
	preferences, err := catalog.Values(profile, config.Preferences)
	if err != nil {
		return firefox.Contribution{}, err
	}
	contribution := firefox.Contribution{Preferences: preferences}
	if len(config.Shortcuts) > 0 {
		file, err := config.shortcuts(profile)
		if err != nil {
			return contribution, err
		}
		contribution.Files = append(contribution.Files, file)
	}
	if len(config.Workspaces) > 0 || len(config.WorkspaceOrder) > 0 {
		file, err := config.workspaces(profile)
		if err != nil {
			return contribution, err
		}
		contribution.Files = append(contribution.Files, file)
	}
	if len(config.Mods) > 0 {
		files, prefs, err := config.mods(profile)
		if err != nil {
			return contribution, err
		}
		contribution.Files = append(contribution.Files, files...)
		for name, value := range prefs {
			if _, present := preferences[name]; present {
				return contribution, fmt.Errorf("mod preference %s conflicts with Zen preferences", name)
			}
			preferences[name] = value
		}
	}
	return contribution, nil
}

func loadSchemas() (*settings.CompiledSchema, *settings.CompiledSchema) {
	schemas, err := jsonutil.Strict.Decode[struct {
		Shortcut  settings.Schema `json:"shortcut"`
		Workspace settings.Schema `json:"workspace"`
	}](bytes.NewReader(embeddedSchemas))
	if err != nil {
		panic(fmt.Errorf("decode Zen schemas: %w", err))
	}
	shortcut, err := schemas.Shortcut.Compile()
	if err != nil {
		panic(fmt.Errorf("compile Zen shortcut schema: %w", err))
	}
	workspace, err := schemas.Workspace.Compile()
	if err != nil {
		panic(fmt.Errorf("compile Zen workspace schema: %w", err))
	}
	return shortcut, workspace
}
