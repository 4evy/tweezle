package zen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/4evy/tweezle/addons/firefox"
	jsonutil "github.com/4evy/tweezle/internal/json"
)

var modHTTPClient = http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" {
			return errors.New("zen Mod asset redirected away from HTTPS")
		}
		if len(via) >= 10 {
			return errors.New("too many Zen Mod redirects")
		}
		return nil
	},
}

func download(ctx context.Context, address string) ([]byte, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("zen Mod asset requires HTTPS: %q", address)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("download Zen Mod asset %s: %w", address, err)
	}
	response, err := modHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download Zen Mod asset %s: %w", address, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download Zen Mod asset %s: HTTP %s", address, response.Status)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download Zen Mod asset %s: %w", address, err)
	}
	if len(data) > limit {
		return nil, errors.New("zen Mod asset exceeds 8 MiB")
	}
	return data, nil
}

func (config Config) mods(profile firefox.Profile) ([]firefox.ProfileFile, map[string]any, error) {
	ctx := profile.Context
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Join(profile.Directory, "zen-themes.json")
	order, manifests, err := loadMods(path)
	if err != nil {
		return nil, nil, err
	}
	// ZenCommonUtils maps Gecko's Darwin platform to the store's macos token
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	var files []firefox.ProfileFile
	styles := map[string][]byte{}
	preferences := map[string]any{}
	for _, id := range slices.Sorted(maps.Keys(config.Mods)) {
		configured := config.Mods[id]
		modPreferences := map[string]any{}
		manifest, installed := manifests[id]
		refresh := !installed || (configured.Revision != "" && manifest["tweezleRevision"] != configured.Revision)
		if refresh {
			address := "https://zen-browser.github.io/theme-store/themes/" + id + "/theme.json"
			if configured.Revision != "" {
				address = "https://raw.githubusercontent.com/zen-browser/theme-store/" + configured.Revision + "/themes/" + id + "/theme.json"
			}
			data, err := download(ctx, address)
			if err != nil {
				return nil, nil, err
			}
			var updated map[string]any
			if err := jsonutil.Default.DecodeInto(bytes.NewReader(data), &updated); err != nil {
				return nil, nil, err
			}
			if updated["id"] != id {
				return nil, nil, fmt.Errorf("zen Mod manifest ID does not match %s", id)
			}
			if installed {
				if oldEnabled, present := manifest["enabled"]; present {
					updated["enabled"] = oldEnabled
				} else {
					delete(updated, "enabled")
				}
			} else {
				order = append(order, id)
			}
			manifest = updated
			if configured.Revision != "" {
				manifest["tweezleRevision"] = configured.Revision
			}
		}
		if configured.Enabled != nil {
			manifest["enabled"] = *configured.Enabled
		}
		root := filepath.Join(profile.Directory, "chrome", "zen-themes", id)
		var preferenceData []byte
		for _, asset := range []struct{ field, file string }{{"style", "chrome.css"}, {"readme", "readme.md"}, {"preferences", "preferences.json"}} {
			address, _ := manifest[asset.field].(string)
			if address == "" {
				if asset.field == "style" {
					return nil, nil, fmt.Errorf("zen Mod %s has no stylesheet URL", id)
				}
				continue
			}
			destination := filepath.Join(root, asset.file)
			data, err := os.ReadFile(destination)
			if refresh || errors.Is(err, os.ErrNotExist) {
				if configured.Revision != "" {
					address = strings.Replace(address, "https://raw.githubusercontent.com/zen-browser/theme-store/main/", "https://raw.githubusercontent.com/zen-browser/theme-store/"+configured.Revision+"/", 1)
					manifest[asset.field] = address
				}
				data, err = download(ctx, address)
				if err != nil {
					return nil, nil, err
				}
				files = append(files, firefox.ProfileFile{Path: destination, Data: data})
			} else if err != nil {
				return nil, nil, err
			}
			if asset.field == "style" && configured.StyleSHA256 != "" {
				if !strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(data)), configured.StyleSHA256) {
					return nil, nil, fmt.Errorf("zen Mod %s stylesheet SHA-256 mismatch", id)
				}
			}
			if asset.field == "style" {
				styles[id] = data
			}
			if asset.field == "preferences" {
				preferenceData = data
			}
		}
		var definitions []struct {
			Property   string   `json:"property"`
			Type       string   `json:"type"`
			Default    any      `json:"defaultValue"`
			DisabledOn []string `json:"disabledOn"`
		}
		if len(preferenceData) > 0 {
			if err := json.Unmarshal(preferenceData, &definitions); err != nil {
				return nil, nil, err
			}
		}
		types := map[string]string{}
		enabled := modEnabled(manifest)
		for _, definition := range definitions {
			if slices.Contains(definition.DisabledOn, platform) {
				continue
			}
			types[definition.Property] = definition.Type
			if !enabled {
				continue
			}
			if _, existing := profile.Preferences[definition.Property]; existing {
				continue
			}
			if definition.Type == "checkbox" {
				if value, ok := definition.Default.(bool); ok {
					modPreferences[definition.Property] = value
				}
			} else {
				switch value := definition.Default.(type) {
				case string:
					modPreferences[definition.Property] = value
				case float64:
					modPreferences[definition.Property] = fmt.Sprint(value)
				}
			}
		}
		for name, value := range configured.Preferences {
			typ, exists := types[name]
			if !exists {
				return nil, nil, fmt.Errorf("zen Mod %s has no preference %q", id, name)
			}
			_, boolean := value.(bool)
			_, text := value.(string)
			if (typ == "checkbox" && !boolean) || (typ != "checkbox" && !text) {
				return nil, nil, fmt.Errorf("zen Mod preference %s has the wrong type", name)
			}
			modPreferences[name] = value
		}
		for name, value := range modPreferences {
			if previous, exists := preferences[name]; exists && previous != value {
				return nil, nil, fmt.Errorf("mods configure conflicting values for %s", name)
			}
			preferences[name] = value
		}
		manifests[id] = manifest
	}
	data, err := encodeMods(order, manifests)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, firefox.ProfileFile{Path: path, Data: data})
	// Zen reads the aggregate sheet on startup and rebuilds it only on updates
	// Rebuild every enabled Mod in native insertion order, including unmanaged
	// Mods, so installing or toggling one does not remove the others' CSS
	var combined strings.Builder
	combined.WriteString("/* Generated Zen Mods stylesheet */\n")
	for _, id := range order {
		if !modEnabled(manifests[id]) {
			continue
		}
		style, prepared := styles[id]
		if !prepared {
			style, err = os.ReadFile(filepath.Join(profile.Directory, "chrome", "zen-themes", id, "chrome.css"))
			if err != nil {
				return nil, nil, err
			}
		}
		combined.Write(style)
		combined.WriteByte('\n')
	}
	files = append(files, firefox.ProfileFile{Path: filepath.Join(profile.Directory, "chrome", "zen-themes.css"), Data: []byte(combined.String())})
	return files, preferences, nil
}

func modEnabled(manifest map[string]any) bool {
	enabled, configured := manifest["enabled"].(bool)
	return !configured || enabled
}

// Use the token stream for the outer object to retain native stylesheet
// order, and decode each manifest once with the exact-number policy
func loadMods(path string) ([]string, map[string]map[string]any, error) {
	manifests := map[string]map[string]any{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, manifests, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	decoder := jsontext.NewDecoder(file)
	token, err := decoder.ReadToken()
	if err != nil {
		return nil, nil, err
	}
	if token.Kind() != jsontext.KindBeginObject {
		return nil, nil, fmt.Errorf("%s must contain an object", path)
	}
	var order []string
	for decoder.PeekKind() != jsontext.KindEndObject {
		token, err := decoder.ReadToken()
		if err != nil {
			return nil, nil, err
		}
		id := token.String()
		if !uuid.MatchString(id) || decoder.PeekKind() != jsontext.KindBeginObject {
			return nil, nil, fmt.Errorf("invalid installed Zen Mod %q", id)
		}
		var manifest map[string]any
		if err := jsonutil.Default.DecodeFrom(decoder, &manifest); err != nil {
			return nil, nil, err
		}
		manifests[id] = manifest
		order = append(order, id)
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, nil, err
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("%s must contain exactly one JSON object", path)
		}
		return nil, nil, err
	}
	return order, manifests, nil
}

func encodeMods(order []string, manifests map[string]map[string]any) ([]byte, error) {
	var data bytes.Buffer
	encoder := jsontext.NewEncoder(&data)
	if err := encoder.WriteToken(jsontext.BeginObject); err != nil {
		return nil, err
	}
	for _, id := range order {
		if err := encoder.WriteToken(jsontext.String(id)); err != nil {
			return nil, err
		}
		if err := json.MarshalEncode(encoder, manifests[id], json.Deterministic(true)); err != nil {
			return nil, err
		}
	}
	if err := encoder.WriteToken(jsontext.EndObject); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}
