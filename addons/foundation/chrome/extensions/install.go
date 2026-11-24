package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	fileutil "github.com/4evy/tweezle/internal/file"
)

// The Chromium-sensitive contracts in this file were audited on 2026-08-23.
// Chromium main (CRX, external providers, APIs):
// https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/
// Chromium base used by the audited Helium revision (151.0.7922.173):
// https://chromium.googlesource.com/chromium/src/+/a96602f30358e9b5d256a0464e7e4d4bec223004/
// Helium patch set:
// https://github.com/imputnet/helium/tree/208a56bab803ceda8202f8c79dd0e9bfcffe600a
const (
	crxExtensionDir        = "extensions/crx"
	zipExtensionArchiveDir = "extensions/zip"
	unpackedExtensionDir   = "extensions/unpacked"
	externalStateFile      = "extensions/external-definitions.json"
	externalStateVersion   = 1
	crxFileExtension       = ".crx"
	zipFileExtension       = ".zip"
	manifestFilename       = "manifest.json"
	manifestKeyMaxBytes    = 100 * 1024
	manifestKeyPEMBegin    = "-----BEGIN"
	manifestKeyPEMEnd      = "-----END"
	manifestKeyHeaderEnd   = "KEY-----"

	// Chromium's external-pref provider accepts exactly one of these shapes:
	// {external_crx, external_version} for a local CRX, or
	// {external_update_url} for a browser-managed download. The filename stem
	// supplies the extension ID; it is intentionally not repeated in the JSON.
	// Source: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/chrome/browser/extensions/external_provider_impl.cc#300
	externalCRXPathKey   = "external_crx"
	externalVersionKey   = "external_version"
	externalUpdateURLKey = "external_update_url"

	externalManifestFileExtension = ".json"
	chromeStoreResponseKey        = "response"
	chromeStoreProductVersionKey  = "prodversion"
	chromeStoreAcceptFormatKey    = "acceptformat"
	chromeStoreOSKey              = "os"
	chromeStoreArchitectureKey    = "arch"
	chromeStoreProductKey         = "prod"
	chromeStoreExtensionKey       = "x"
	chromeStoreResponseRedirect   = "redirect"
	// This installer needs a complete CRX archive, so it requests CRX3 only. The
	// Chromium updater also advertises "puff", but this installer has no delta
	// patch pipeline. The artifact preflight accepts only full CRX3 files.
	// Chromium updater query: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/components/update_client/update_query_params.cc#89
	// Chromium verifier formats: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/components/crx_file/crx_verifier.h#16
	chromeStoreCRXFormats    = "crx3"
	chromeStoreProduct       = "chromiumcrx"
	chromeStoreInstallSuffix = "&uc"
)

type installedCRXExtension struct {
	DownloadedExtension
	Path string
}

type externalExtensionDefinition struct {
	ID       string
	Manifest externalExtensionManifest
}

// externalExtensionManifest remains string-only because every value Chromium
// accepts in the two external-pref shapes above is a string. Keeping this type
// narrow also makes ownership-state comparison exact and fail-closed.
type externalExtensionManifest map[string]string
type externalExtensionDirectoryState map[string]externalExtensionManifest

// externalExtensionState is browser's private ownership ledger, not a file
// Chromium reads. It records the exact definitions written during the previous
// run so stale files can be removed only when the user has not edited them.
type externalExtensionState struct {
	Version     int                                        `json:"version"`
	Directories map[string]externalExtensionDirectoryState `json:"directories"`
}

type unpackedExtensionPaths struct {
	archive     string
	unpackedDir string
	target      string
	tempPrefix  string
}

type installedUnpackedExtension struct {
	ID   string
	Path string
}

type stagedUnpackedExtension struct {
	root string
	path string
	id   string
}

func Install(ctx context.Context, options Options) (Result, error) {
	if err := ValidateCatalog(options.Catalog); err != nil {
		return Result{}, err
	}
	if options.Root == "" {
		return Result{}, errors.New("extension installation root is required")
	}
	if options.Download == nil && catalogRequiresDownload(options.Catalog) {
		return Result{}, errors.New("extension download function is required")
	}
	if err := normalizeInstallPaths(&options); err != nil {
		return Result{}, err
	}
	crxDir := filepath.Join(options.Root, crxExtensionDir)
	if err := os.MkdirAll(crxDir, fileutil.DefaultDirPerm); err != nil {
		return Result{}, err
	}
	verifyFile := options.Verify
	if verifyFile == nil {
		verifyFile = VerifyFileSHA256
	}
	installed, err := installChromeStoreExtensions(ctx, options, crxDir)
	if err != nil {
		return Result{}, err
	}
	downloaded, err := installDownloadedExtensions(ctx, options, crxDir, verifyFile)
	if err != nil {
		return Result{}, err
	}
	installed = append(installed, downloaded...)
	result, err := installUnpackedExtensions(ctx, options, verifyFile)
	if err != nil {
		return Result{}, err
	}
	if len(installed) > 0 {
		result.CRXPaths = make(map[string]string, len(installed))
		for _, extension := range installed {
			result.CRXPaths[extension.ID] = extension.Path
		}
	}
	definitions := externalExtensionDefinitions(options, installed)
	if err := syncExternalExtensionDefinitions(options, definitions); err != nil {
		return Result{}, err
	}
	return result, nil
}

func catalogRequiresDownload(catalog Catalog) bool {
	return len(catalog.ChromeStore) > 0 || len(catalog.CRX) > 0 || len(catalog.ZIP) > 0
}

func normalizeInstallPaths(options *Options) error {
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return fmt.Errorf("resolve extension installation root: %w", err)
	}
	options.Root = root

	directories := make([]string, 0, len(options.ExternalDirs))
	seen := map[string]bool{}
	for index, directory := range options.ExternalDirs {
		if strings.TrimSpace(directory) == "" {
			return fmt.Errorf("external extension directory %d must not be empty", index)
		}
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return fmt.Errorf("resolve external extension directory %q: %w", directory, err)
		}
		if seen[absolute] {
			continue
		}
		seen[absolute] = true
		directories = append(directories, absolute)
	}
	options.ExternalDirs = directories
	return nil
}

func installChromeStoreExtensions(
	ctx context.Context,
	options Options,
	crxDir string,
) ([]installedCRXExtension, error) {
	installed := make(
		[]installedCRXExtension,
		0,
		len(options.Catalog.ChromeStore),
	)
	for _, extension := range options.Catalog.ChromeStore {
		if options.ExcludedIDs[extension.ID] {
			continue
		}
		if options.ChromeVersion == "" {
			return nil, errors.New(
				"chrome product version is required for Chrome Web Store extensions",
			)
		}
		downloadURL, err := ChromeStoreCRXDownloadURLForVersion(
			options.Catalog.ChromeStoreUpdateURL,
			extension.ID,
			options.ChromeVersion,
		)
		if err != nil {
			return nil, err
		}
		download := DownloadedExtension{
			ExtensionIdentity: extension.ExtensionIdentity, URL: downloadURL,
		}
		installedExtension, err := installChromeStoreCRX(ctx, options, crxDir, download)
		if err != nil {
			return nil, err
		}
		installed = append(installed, installedExtension)
	}
	return installed, nil
}

func installChromeStoreCRX(
	ctx context.Context,
	options Options,
	directory string,
	extension DownloadedExtension,
) (result installedCRXExtension, err error) {
	if options.VerifyCRX != nil {
		if options.Resolve == nil {
			return result, errors.New("custom Chrome Store CRX verification requires a version resolver")
		}
		resolved, err := options.Resolve(ctx, extension.URL)
		if err != nil {
			return result, err
		}
		if !validHTTPURL(resolved) {
			return result, errors.New("resolved Chrome Store CRX URL must use HTTP(S)")
		}
		extension.Version, err = ChromeStoreVersionFromCRXURL(extension.ID, resolved)
		if err != nil {
			return result, err
		}
		return installCRXExtension(ctx, directory, extension, options.Download, func(path string) error {
			return options.VerifyCRX(path, extension.ID)
		})
	}

	// The update service picks a version at GET time. Authenticate that exact
	// payload and use its manifest, not a separate request or a URL filename,
	// to identify the artifact we publish.
	err = withStagedDownload(ctx, directory, extension.ID+crxFileExtension, extension.URL, options.Download, func(path string) error {
		version, err := inspectCRX(path, extension.ID, true)
		if err != nil {
			return err
		}
		extension.Version = version
		target := crxCachePath(directory, extension)
		if err := os.Rename(path, target); err != nil {
			return err
		}
		result = installedCRXExtension{DownloadedExtension: extension, Path: target}
		return nil
	})
	return result, err
}

func installDownloadedExtensions(
	ctx context.Context,
	options Options,
	crxDir string,
	verifyFile func(path, checksum string) error,
) ([]installedCRXExtension, error) {
	installed := make([]installedCRXExtension, 0, len(options.Catalog.CRX))
	for _, extension := range options.Catalog.CRX {
		if options.ExcludedIDs[extension.ID] {
			continue
		}
		installedExtension, err := installCRXExtension(
			ctx,
			crxDir,
			extension,
			options.Download,
			func(path string) error {
				if err := verifyFile(path, extension.SHA256); err != nil {
					return fmt.Errorf("verify %s: %w", extension.Name, err)
				}
				if options.VerifyCRX != nil {
					return options.VerifyCRX(path, extension.ID)
				}
				version, err := inspectCRX(path, extension.ID, false)
				if err != nil {
					return err
				}
				return checkManifestVersion(extension.Name, version, extension.Version)
			},
		)
		if err != nil {
			return nil, err
		}
		installed = append(installed, installedExtension)
	}
	return installed, nil
}

func installUnpackedExtensions(
	ctx context.Context,
	options Options,
	verify func(path, checksum string) error,
) (Result, error) {
	result := Result{ExtensionIDAliases: map[string]string{}}
	for _, extension := range options.Catalog.ZIP {
		if options.ExcludedIDs[extension.ID] {
			continue
		}
		installed, err := installUnpackedExtension(ctx, options, verify, extension)
		if err != nil {
			return Result{}, err
		}
		if extension.LoadUnpacked {
			result.LoadExtensionPaths = append(result.LoadExtensionPaths, installed.Path)
			result.ExtensionIDAliases[extension.ID] = installed.ID
		}
	}
	for _, extension := range options.Catalog.Git {
		if options.ExcludedIDs[extension.ID] {
			continue
		}
		installed, err := installGitExtension(ctx, options, extension)
		if err != nil {
			return Result{}, err
		}
		if extension.LoadUnpacked {
			result.LoadExtensionPaths = append(result.LoadExtensionPaths, installed.Path)
			result.ExtensionIDAliases[extension.ID] = installed.ID
		}
	}
	return result, nil
}

func externalExtensionDefinitions(
	options Options,
	installed []installedCRXExtension,
) []externalExtensionDefinition {
	definitions := make(
		[]externalExtensionDefinition,
		0,
		len(installed)+len(options.Catalog.UpdateURL),
	)
	for _, extension := range installed {
		definitions = append(definitions, externalExtensionDefinition{
			ID: extension.ID,
			Manifest: map[string]string{
				externalCRXPathKey: extension.Path,
				externalVersionKey: extension.Version,
			},
		})
	}
	for _, extension := range options.Catalog.UpdateURL {
		if options.ExcludedIDs[extension.ID] {
			continue
		}
		definitions = append(definitions, externalExtensionDefinition{
			ID: extension.ID,
			Manifest: map[string]string{
				externalUpdateURLKey: extension.UpdateURL,
			},
		})
	}
	return definitions
}

// syncExternalExtensionDefinitions mirrors definitions to every configured
// Chromium search directory. Chromium scans non-recursive <extension-id>.json
// files in these directories. Removal is intentionally compare-before-delete:
// files not in the ledger, or whose contents no longer match it, belong to the
// user and survive reconciliation.
// Loader source: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/chrome/browser/extensions/external_pref_loader.cc#73
func syncExternalExtensionDefinitions(
	options Options,
	definitions []externalExtensionDefinition,
) error {
	statePath := filepath.Join(options.Root, externalStateFile)
	previous, err := readExternalExtensionState(statePath)
	if err != nil {
		return err
	}

	current := externalExtensionState{
		Version:     externalStateVersion,
		Directories: make(map[string]externalExtensionDirectoryState, len(options.ExternalDirs)),
	}
	for _, externalDir := range options.ExternalDirs {
		if err := os.MkdirAll(externalDir, fileutil.DefaultDirPerm); err != nil {
			return err
		}
		manifests := make(externalExtensionDirectoryState, len(definitions))
		for _, definition := range definitions {
			if err := writeExternalJSON(
				filepath.Join(externalDir, definition.ID+externalManifestFileExtension),
				definition.Manifest,
			); err != nil {
				return err
			}
			manifests[definition.ID] = maps.Clone(definition.Manifest)
		}
		current.Directories[externalDir] = manifests
	}

	for _, directory := range slices.Sorted(maps.Keys(previous.Directories)) {
		for _, id := range slices.Sorted(maps.Keys(previous.Directories[directory])) {
			if _, stillManaged := current.Directories[directory][id]; stillManaged {
				continue
			}
			path := filepath.Join(directory, id+externalManifestFileExtension)
			if err := removeExternalJSONIfUnchanged(
				path,
				previous.Directories[directory][id],
			); err != nil {
				return err
			}
		}
	}
	_, err = fileutil.WriteJSONIfChanged(
		statePath,
		current,
		fileutil.PrivateFilePerm,
	)
	return err
}

func readExternalExtensionState(path string) (externalExtensionState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return externalExtensionState{
			Version:     externalStateVersion,
			Directories: map[string]externalExtensionDirectoryState{},
		}, nil
	}
	if err != nil {
		return externalExtensionState{}, err
	}
	var state externalExtensionState
	if err := json.Unmarshal(data, &state); err != nil {
		return externalExtensionState{}, fmt.Errorf(
			"parse managed external extension state %s: %w",
			path,
			err,
		)
	}
	if state.Version != externalStateVersion {
		return externalExtensionState{}, fmt.Errorf(
			"managed external extension state %s has unsupported version %d",
			path,
			state.Version,
		)
	}
	for directory, manifests := range state.Directories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			return externalExtensionState{}, fmt.Errorf(
				"managed external extension state has invalid directory %q",
				directory,
			)
		}
		for id, manifest := range manifests {
			if !ValidExtensionID(id) {
				return externalExtensionState{}, fmt.Errorf(
					"managed external extension state has invalid extension ID %q",
					id,
				)
			}
			if !validExternalManifest(manifest) {
				return externalExtensionState{}, fmt.Errorf(
					"managed external extension state has invalid manifest for %q",
					id,
				)
			}
		}
	}
	if state.Directories == nil {
		state.Directories = map[string]externalExtensionDirectoryState{}
	}
	return state, nil
}

func validExternalManifest(manifest externalExtensionManifest) bool {
	if updateURL, ok := manifest[externalUpdateURLKey]; ok {
		return len(manifest) == 1 && ValidURL(updateURL)
	}
	crxPath, hasCRX := manifest[externalCRXPathKey]
	version, hasVersion := manifest[externalVersionKey]
	return len(manifest) == 2 && hasCRX && hasVersion &&
		filepath.IsAbs(crxPath) && filepath.Clean(crxPath) == crxPath &&
		ValidExternalVersion(version)
}

func removeExternalJSONIfUnchanged(path string, expected externalExtensionManifest) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current externalExtensionManifest
	if err := json.Unmarshal(data, &current); err != nil || !maps.Equal(current, expected) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale external extension definition %s: %w", path, err)
	}
	return nil
}

func installCRXExtension(
	ctx context.Context,
	directory string,
	extension DownloadedExtension,
	download DownloadFunc,
	verify func(path string) error,
) (installedCRXExtension, error) {
	target := crxCachePath(directory, extension)
	if err := ensureDownloaded(ctx, target, extension.URL, download, verify); err != nil {
		return installedCRXExtension{}, err
	}
	return installedCRXExtension{
		DownloadedExtension: extension,
		Path:                target,
	}, nil
}

func crxCachePath(directory string, extension DownloadedExtension) string {
	return filepath.Join(directory, extension.ID+"-"+extension.Version+crxFileExtension)
}

func installUnpackedExtension(
	ctx context.Context,
	options Options,
	verify func(path, checksum string) error,
	extension ZIPExtension,
) (_ installedUnpackedExtension, err error) {
	artifact, err := resolveReleaseArtifact(ctx, options, extension)
	if err != nil {
		return installedUnpackedExtension{}, err
	}
	if !ValidExternalVersion(artifact.Version) || !validHTTPURL(artifact.URL) || !ValidSHA256(artifact.SHA256) {
		return installedUnpackedExtension{}, fmt.Errorf(
			"%s release metadata is incomplete",
			extension.Name,
		)
	}
	paths, err := prepareUnpackedExtensionPaths(options.Root, extension.ID, artifact.Version)
	if err != nil {
		return installedUnpackedExtension{}, err
	}
	var staged stagedUnpackedExtension
	err = ensureDownloaded(
		ctx,
		paths.archive,
		artifact.URL,
		options.Download,
		func(path string) error {
			if err := verify(path, artifact.SHA256); err != nil {
				return fmt.Errorf("verify %s: %w", extension.Name, err)
			}
			var err error
			staged, err = stageUnpackedArchive(paths, extension, path, artifact.Version)
			return err
		},
	)
	if staged.root != "" {
		defer func() { err = errors.Join(err, os.RemoveAll(staged.root)) }()
	}
	if err != nil {
		return installedUnpackedExtension{}, err
	}
	return publishUnpackedDirectory(staged.path, paths.target, extension.Name, staged.id)
}

func prepareUnpackedExtensionPaths(
	root,
	extensionID,
	version string,
) (unpackedExtensionPaths, error) {
	archiveDir := filepath.Join(root, zipExtensionArchiveDir)
	unpackedDir := filepath.Join(root, unpackedExtensionDir)
	for _, directory := range []string{archiveDir, unpackedDir} {
		if err := os.MkdirAll(directory, fileutil.DefaultDirPerm); err != nil {
			return unpackedExtensionPaths{}, err
		}
	}
	return unpackedExtensionPaths{
		archive: filepath.Join(
			archiveDir,
			extensionID+"-"+version+zipFileExtension,
		),
		unpackedDir: unpackedDir,
		target:      filepath.Join(unpackedDir, extensionID),
		tempPrefix:  "." + extensionID + "-",
	}, nil
}

func stageUnpackedArchive(
	paths unpackedExtensionPaths,
	extension ZIPExtension,
	archive, version string,
) (_ stagedUnpackedExtension, err error) {
	temporaryDir, err := os.MkdirTemp(paths.unpackedDir, paths.tempPrefix)
	if err != nil {
		return stagedUnpackedExtension{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(temporaryDir))
		}
	}()
	if err := ExtractZipFile(archive, temporaryDir); err != nil {
		return stagedUnpackedExtension{}, fmt.Errorf("extract %s: %w", extension.Name, err)
	}
	sourceDir := filepath.Join(temporaryDir, filepath.FromSlash(extension.ArchiveRoot))
	if extension.ManifestKey != "" {
		manifestPath := filepath.Join(sourceDir, manifestFilename)
		data, err := readUnpackedManifest(manifestPath)
		if err != nil {
			return stagedUnpackedExtension{}, err
		}
		var manifest map[string]jsontext.Value
		if err := json.Unmarshal(data, &manifest); err != nil {
			return stagedUnpackedExtension{}, err
		}
		if manifest == nil {
			return stagedUnpackedExtension{}, errors.New("manifest must be a JSON object")
		}
		key, err := json.Marshal(extension.ManifestKey)
		if err != nil {
			return stagedUnpackedExtension{}, err
		}
		manifest["key"] = key
		if _, err := fileutil.WriteJSONIfChanged(manifestPath, manifest, fileutil.DefaultFilePerm); err != nil {
			return stagedUnpackedExtension{}, err
		}
	}
	id, err := validateUnpackedManifest(sourceDir, extension.Name, version)
	if err != nil {
		return stagedUnpackedExtension{}, err
	}
	return stagedUnpackedExtension{root: temporaryDir, path: sourceDir, id: id}, nil
}

func installGitExtension(
	ctx context.Context,
	options Options,
	extension GitExtension,
) (_ installedUnpackedExtension, err error) {
	if options.CheckoutGit == nil {
		return installedUnpackedExtension{}, errors.New(
			"git checkout function is required for Git extensions",
		)
	}
	unpackedDir := filepath.Join(options.Root, unpackedExtensionDir)
	if err := os.MkdirAll(unpackedDir, fileutil.DefaultDirPerm); err != nil {
		return installedUnpackedExtension{}, err
	}
	temporaryDir, err := os.MkdirTemp(unpackedDir, "."+extension.ID+"-git-")
	if err != nil {
		return installedUnpackedExtension{}, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temporaryDir)) }()
	revision, err := options.CheckoutGit(ctx, temporaryDir, extension)
	if err != nil {
		return installedUnpackedExtension{}, fmt.Errorf(
			"check out Git extension %s: %w",
			extension.Name,
			err,
		)
	}
	if !ValidGitCommit(revision.Commit) {
		return installedUnpackedExtension{}, fmt.Errorf(
			"%s Git revision metadata is incomplete",
			extension.Name,
		)
	}
	if extension.updatePolicy() == UpdatePolicyPinned &&
		!strings.EqualFold(revision.Commit, extension.Commit) {
		return installedUnpackedExtension{}, fmt.Errorf(
			"%s resolved Git commit %s, want pinned commit %s",
			extension.Name,
			revision.Commit,
			extension.Commit,
		)
	}
	id, err := validateUnpackedManifest(temporaryDir, extension.Name, "")
	if err != nil {
		return installedUnpackedExtension{}, err
	}
	return publishUnpackedDirectory(temporaryDir, filepath.Join(unpackedDir, extension.ID), extension.Name, id)
}

func publishUnpackedDirectory(sourceDir, target, name, installedID string) (installedUnpackedExtension, error) {
	// Replacing the whole staged tree removes files deleted by the update
	if err := replaceDirectory(sourceDir, target); err != nil {
		return installedUnpackedExtension{}, err
	}
	if installedID == "" {
		resolvedPath, err := filepath.EvalSymlinks(target)
		if err != nil {
			return installedUnpackedExtension{}, fmt.Errorf(
				"resolve installed %s path: %w",
				name,
				err,
			)
		}
		installedID = UnpackedExtensionID(resolvedPath)
	}
	return installedUnpackedExtension{ID: installedID, Path: target}, nil
}

// replaceDirectory swaps a staged directory into place without first deleting
// the working installation. The old tree is parked beside the target and
// restored if the staged rename fails. All paths stay on the same filesystem,
// so the individual renames remain atomic.
func replaceDirectory(source, target string) (err error) {
	parent := filepath.Dir(target)
	backupRoot, err := os.MkdirTemp(parent, "."+filepath.Base(target)+"-previous-")
	if err != nil {
		return fmt.Errorf("reserve previous extension directory: %w", err)
	}
	keepBackup := false
	defer func() {
		if !keepBackup {
			err = errors.Join(err, os.RemoveAll(backupRoot))
		}
	}()

	backup := filepath.Join(backupRoot, "tree")
	hadTarget := true
	if err := os.Rename(target, backup); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("park previous extension directory %s: %w", target, err)
		}
		hadTarget = false
	}
	if err := os.Rename(source, target); err != nil {
		installErr := fmt.Errorf("install extension directory %s: %w", target, err)
		if !hadTarget {
			return installErr
		}
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			keepBackup = true
			return errors.Join(
				installErr,
				fmt.Errorf("restore previous extension directory %s (backup retained at %s): %w", target, backup, restoreErr),
			)
		}
		return installErr
	}
	return nil
}

func resolveReleaseArtifact(
	ctx context.Context,
	options Options,
	extension ZIPExtension,
) (ReleaseArtifact, error) {
	if extension.updatePolicy() == UpdatePolicyPinned {
		return ReleaseArtifact{
			Version: extension.Version,
			URL:     extension.URL,
			SHA256:  extension.SHA256,
		}, nil
	}
	if options.ResolveLatestRelease == nil {
		return ReleaseArtifact{}, errors.New(
			"latest GitHub release resolver is required for latest ZIP extensions",
		)
	}
	artifact, err := options.ResolveLatestRelease(
		ctx,
		extension.Repository,
		extension.AssetTemplate,
	)
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf(
			"resolve latest %s release: %w",
			extension.Name,
			err,
		)
	}
	return artifact, nil
}

func ensureDownloaded(
	ctx context.Context,
	target,
	rawURL string,
	download DownloadFunc,
	verify func(path string) error,
) error {
	if _, err := boundedFileSize(target, maxArtifactBytes); err == nil {
		if err := verify(target); err == nil {
			return nil
		}
	}
	return withStagedDownload(ctx, filepath.Dir(target), filepath.Base(target), rawURL, download, func(path string) error {
		if err := verify(path); err != nil {
			return err
		}
		return os.Rename(path, target)
	})
}

func withStagedDownload(
	ctx context.Context,
	directory, name, rawURL string,
	download DownloadFunc,
	publish func(path string) error,
) (err error) {
	if !validHTTPURL(rawURL) {
		return errors.New("extension download URL must use HTTP(S)")
	}
	stage, err := os.MkdirTemp(directory, ".download-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	path := filepath.Join(stage, name)
	if err := download(ctx, path, rawURL); err != nil {
		return err
	}
	if _, err := boundedFileSize(path, maxArtifactBytes); err != nil {
		return err
	}
	return publish(path)
}

func validateUnpackedManifest(
	extensionDir,
	name,
	version string,
) (string, error) {
	data, err := readUnpackedManifest(filepath.Join(extensionDir, manifestFilename))
	if err != nil {
		return "", fmt.Errorf("read %s manifest: %w", name, err)
	}
	// "version" is the extension package version used for release matching;
	// it is unrelated to the integer "manifest_version" API contract. Do not
	// reject Manifest V2 here: Chromium owns manifest/API validation, and Helium
	// deliberately keeps Chromium's MV2 paths enabled.
	// Upstream gate: https://chromium.googlesource.com/chromium/src/+/a96602f30358e9b5d256a0464e7e4d4bec223004/extensions/browser/manifest_v2_handler.cc#137
	// Helium source: https://github.com/imputnet/helium/blob/208a56bab803ceda8202f8c79dd0e9bfcffe600a/patches/ungoogled-chromium/extensions-manifestv2.patch
	var manifest struct {
		Version string         `json:"version"`
		Key     jsontext.Value `json:"key"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse %s manifest: %w", name, err)
	}
	if err := checkManifestVersion(name, manifest.Version, version); err != nil {
		return "", err
	}
	if len(manifest.Key) == 0 {
		return "", nil
	}
	var key string
	if err := json.Unmarshal(manifest.Key, &key); err != nil {
		return "", fmt.Errorf("parse %s manifest public key: %w", name, err)
	}
	installedID, err := extensionIDFromManifestKey(key)
	if err != nil {
		return "", fmt.Errorf("parse %s manifest public key: %w", name, err)
	}
	return installedID, nil
}

func checkManifestVersion(name, actual, expected string) error {
	if expected == "" {
		if !ValidExternalVersion(actual) {
			return fmt.Errorf("%s manifest has invalid version %q", name, actual)
		}
		return nil
	}
	order, err := CompareExternalVersions(actual, expected)
	if err != nil {
		return fmt.Errorf("%s manifest version: %w", name, err)
	}
	if order != 0 {
		return fmt.Errorf("%s manifest version is %q, want %q", name, actual, expected)
	}
	return nil
}

// UnpackedManifestID returns the identity Chromium derives from a manifest key
// or, for extensions without a key, their resolved installation path
func UnpackedManifestID(directory string) (string, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	id, err := validateUnpackedManifest(directory, "extension", "")
	if err != nil || id != "" {
		return id, err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	return UnpackedExtensionID(resolved), nil
}

// extensionIDFromManifestKey mirrors Extension::ParsePEMKeyBytes followed by
// crx_file::id_util::GenerateId. Chromium treats the decoded key as opaque
// bytes; parsing or re-marshalling it as X.509 would reject valid manifest
// inputs and could derive a different ID.
// Source: extensions/common/extension.cc ComputeExtensionID and
// Extension::ParsePEMKeyBytes in the audited Chromium revision.
func extensionIDFromManifestKey(input string) (string, error) {
	if input == "" || len(input) > manifestKeyMaxBytes {
		return "", errors.New("public key is empty or too large")
	}
	encoded := input
	if strings.HasPrefix(encoded, manifestKeyPEMBegin) {
		encoded = collapsePEMWhitespace(encoded)
		var hasHeader, hasFooter bool
		_, encoded, hasHeader = strings.Cut(encoded, manifestKeyHeaderEnd)
		encoded, _, hasFooter = strings.CutLast(encoded, manifestKeyPEMEnd)
		if !hasHeader || !hasFooter {
			return "", errors.New("public key has invalid PEM markers")
		}
		if encoded == "" {
			return "", errors.New("public key has empty PEM data")
		}
	}
	// encoding/base64 intentionally ignores CR and LF. Chromium's strict
	// Base64Decode does not, so reject any ASCII whitespace left after the PEM
	// newline-collapse step.
	if strings.ContainsAny(encoded, " \t\n\v\f\r") {
		return "", errors.New("public key base64 contains whitespace")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode public key: %w", err)
	}
	return extensionIDFromBytes(decoded), nil
}

// collapsePEMWhitespace mirrors CollapseWhitespaceASCII(value, true): leading
// and trailing whitespace is removed, whitespace runs become one space, and a
// run containing CR or LF is removed completely. This matters because a plain
// space inside PEM data remains invalid under Chromium's strict base64 decoder.
func collapsePEMWhitespace(value string) string {
	result := make([]byte, 0, len(value))
	inWhitespace := true
	alreadyTrimmed := true
	for index := range len(value) {
		character := value[index]
		if strings.ContainsRune(" \t\n\v\f\r", rune(character)) {
			if !inWhitespace {
				inWhitespace = true
				result = append(result, ' ')
			}
			if !alreadyTrimmed && (character == '\n' || character == '\r') {
				alreadyTrimmed = true
				result = result[:len(result)-1]
			}
			continue
		}
		inWhitespace = false
		alreadyTrimmed = false
		result = append(result, character)
	}
	if inWhitespace && !alreadyTrimmed {
		result = result[:len(result)-1]
	}
	return string(result)
}

// VerifyFileSHA256 streams path through SHA-256 and compares it with checksum.
// checksum may be lowercase/uppercase hex, "sha256:"-prefixed hex, or an SRI
// "sha256-" base64 digest; parseSHA256 requires exactly 32 decoded bytes.
// This authenticates pinned bytes independently of the browser's CRX policy
func VerifyFileSHA256(path, checksum string) (err error) {
	want, err := parseSHA256(checksum)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	verifier := want.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("SHA-256 checksum mismatch for %s", path)
	}
	return nil
}

// UnpackedExtensionID mirrors Chromium's GenerateIdForPath on the supported
// POSIX platforms: SHA-256 of the normalized path bytes, first 16 digest bytes,
// hex nibbles mapped from 0-f to a-p. Callers resolve symlinks first because
// Chromium canonicalizes an unpacked path before deriving its stable ID.
// Source: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/components/crx_file/id_util.cc#65
func UnpackedExtensionID(extensionPath string) string {
	return extensionIDFromBytes([]byte(filepath.Clean(extensionPath)))
}

func extensionIDFromBytes(input []byte) string {
	sum := sha256.Sum256(input)
	return extensionIDFromHashPrefix(sum[:extensionIDLength/extensionIDCharactersPerByte])
}

func extensionIDFromHashPrefix(prefix []byte) string {
	var id [extensionIDLength]byte
	for index, value := range prefix {
		id[index*extensionIDCharactersPerByte] =
			extensionIDAlphabetStart + value>>extensionIDHighNibbleShift
		id[index*extensionIDCharactersPerByte+1] =
			extensionIDAlphabetStart + value&extensionIDNibbleMask
	}
	return string(id[:])
}

// ChromeStoreCRXDownloadURLForVersion builds the redirect form of the Omaha
// extension update request. The x value is itself an encoded query fragment
// ("id=<id>&uc"); prodversion participates in server-side compatibility
// selection, and acceptformat asks the service for a complete CRX artifact.
// This request runs through Options.Download, outside the browser. Helium's
// in-browser extension proxy therefore cannot rewrite it; configure this
// package's update URL/network path explicitly when that privacy boundary
// matters.
// Source: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/extensions/browser/webstore_installer.cc#166
// Helium proxy patch: https://github.com/imputnet/helium/blob/208a56bab803ceda8202f8c79dd0e9bfcffe600a/patches/helium/core/proxy-extension-downloads.patch
func ChromeStoreCRXDownloadURLForVersion(updateURL, id, chromeVersion string) (string, error) {
	if !ValidExtensionID(id) {
		return "", fmt.Errorf("invalid Chrome Store extension ID %q", id)
	}
	if !ValidExternalVersion(chromeVersion) {
		return "", fmt.Errorf("invalid Chrome product version %q", chromeVersion)
	}
	parsed, err := url.Parse(updateURL)
	if err != nil {
		return "", fmt.Errorf("parse Chrome Store update URL for %s: %w", id, err)
	}
	if !isHTTPURL(parsed) {
		return "", errors.New("expected an HTTP(S) Chrome Store update URL")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", fmt.Errorf("parse Chrome Store update query for %s: %w", id, err)
	}
	query.Set(chromeStoreResponseKey, chromeStoreResponseRedirect)
	query.Set(chromeStoreProductVersionKey, chromeVersion)
	query.Set(chromeStoreAcceptFormatKey, chromeStoreCRXFormats)
	query.Set(chromeStoreOSKey, chromiumUpdateOS())
	query.Set(chromeStoreArchitectureKey, chromiumUpdateArchitecture())
	query.Set(chromeStoreProductKey, chromeStoreProduct)
	query.Set(chromeStoreExtensionKey, "id="+id+chromeStoreInstallSuffix)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func chromiumUpdateOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "win"
	default:
		return runtime.GOOS
	}
}

func chromiumUpdateArchitecture() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	case "mips64le":
		return "mips64el"
	case "mipsle":
		return "mipsel"
	case "loong64":
		return "loongarch64"
	case "ppc64le":
		return "ppc64"
	default:
		return runtime.GOARCH
	}
}

// ChromeStoreVersionFromCRXURL extracts the version from the redirect target's
// conventional <UPPERCASE_ID>_<underscore-version>.crx filename. This is update
// service response parsing for custom integrations. The built-in installer
// derives the version from the verified manifest instead
func ChromeStoreVersionFromCRXURL(id, crxURL string) (string, error) {
	parsed, err := url.Parse(crxURL)
	if err != nil {
		return "", fmt.Errorf("parse Chrome Store CRX URL for %s: %w", id, err)
	}
	filename := filepath.Base(parsed.Path)
	prefix := strings.ToUpper(id) + "_"
	version, ok := strings.CutPrefix(filename, prefix)
	if !ok {
		return "", fmt.Errorf("parse Chrome Store CRX version for %s from %s", id, crxURL)
	}
	version, ok = strings.CutSuffix(version, crxFileExtension)
	if !ok {
		return "", fmt.Errorf("parse Chrome Store CRX version for %s from %s", id, crxURL)
	}
	version = strings.ReplaceAll(version, "_", ".")
	if !ValidExternalVersion(version) {
		return "", fmt.Errorf("invalid Chrome Store CRX version %q for %s", version, id)
	}
	return version, nil
}

func writeExternalJSON(path string, value map[string]string) error {
	_, err := fileutil.WriteJSONIfChanged(path, value, fileutil.DefaultFilePerm)
	return err
}
