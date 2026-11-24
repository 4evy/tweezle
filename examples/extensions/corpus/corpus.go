// Collect dated store evidence and inspect packages without executing them
package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/4evy/tweezle/addons/foundation/chrome/extensions"
	fileutil "github.com/4evy/tweezle/internal/file"
	repojson "github.com/4evy/tweezle/internal/json"
)

const store = "https://chromewebstore.google.com"

var (
	usersPattern  = regexp.MustCompile(`^([\d,]+)\+?[\s\p{Zs}]*users?$`)
	githubPattern = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+`)
	markers       = map[string]*regexp.Regexp{
		"local":            regexp.MustCompile(`(?i)(?:chrome|browser)\.storage\.local`),
		"sync":             regexp.MustCompile(`(?i)(?:chrome|browser)\.storage\.sync`),
		"managed":          regexp.MustCompile(`(?i)(?:chrome|browser)\.storage\.managed`),
		"session":          regexp.MustCompile(`(?i)(?:chrome|browser)\.storage\.session`),
		"indexeddb":        regexp.MustCompile(`(?i)\bindexedDB\b`),
		"localstorage":     regexp.MustCompile(`(?i)\blocalStorage\b`),
		"compression":      regexp.MustCompile(`(?i)LZString|compressToEncodedURIComponent|pako|fflate`),
		"migration":        regexp.MustCompile(`(?i)\bmigrat(?:e|ion|ions|ing)\b`),
		"dynamic_rules":    regexp.MustCompile(`(?i)updateDynamicRules|updateSessionRules`),
		"native_messaging": regexp.MustCompile(`(?i)connectNative|sendNativeMessage`),
	}
)

type record = map[string]any

type researcher struct {
	root, cache string
	client      extensions.HTTPClient
}

func readJSON(file string, value any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := repojson.Default.DecodeInto(bytes.NewReader(data), value); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func writeJSON(file string, value any) error {
	_, err := fileutil.WriteJSONIfChanged(file, value, fileutil.DefaultFilePerm)
	return err
}

func hash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

type capture struct {
	data []byte
	at   time.Time
}

func readCapture(file string) (capture, error) {
	input, err := os.Open(file)
	if err != nil {
		return capture{}, err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return capture{}, err
	}
	// Older cache files only have a file modification time
	at := info.ModTime()
	var reader io.Reader = input
	if filepath.Ext(file) == ".gz" {
		compressed, err := gzip.NewReader(input)
		if err != nil {
			return capture{}, err
		}
		defer func() { _ = compressed.Close() }()
		if !compressed.ModTime.IsZero() {
			at = compressed.ModTime
		}
		reader = compressed
	}
	data, err := io.ReadAll(reader)
	if err == nil && len(data) == 0 {
		err = fmt.Errorf("empty capture: %s", file)
	}
	return capture{data: data, at: at.UTC()}, err
}

func (r *researcher) fetchPage(address, file string) (capture, error) {
	if saved, err := readCapture(file + ".gz"); !errors.Is(err, os.ErrNotExist) {
		return saved, err
	}
	if saved, err := readCapture(file); !errors.Is(err, os.ErrNotExist) {
		return saved, err
	}
	data, err := r.client.Read(context.Background(), address)
	if err != nil {
		return capture{}, err
	}
	if len(data) == 0 {
		return capture{}, fmt.Errorf("empty response from %s", address)
	}
	at := time.Now().UTC().Truncate(time.Second)
	err = fileutil.WriteWith(file+".gz", fileutil.DefaultFilePerm, func(output io.Writer) error {
		writer := gzip.NewWriter(output)
		writer.ModTime = at
		_, writeErr := writer.Write(data)
		return errors.Join(writeErr, writer.Close())
	})
	if err != nil {
		return capture{}, err
	}
	return capture{data: data, at: at}, nil
}

// Preserve input order while limiting concurrent network and archive work
func pool[T, R any](items []T, fn func(T) R) []R {
	results := make([]R, len(items))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range 6 {
		workers.Go(func() {
			for index := range jobs {
				results[index] = fn(items[index])
			}
		})
	}
	for index := range items {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func attribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Namespace == "" && attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func storePath(href string) string {
	target, err := url.Parse(href)
	if err != nil || (target.Host != "" && target.Host != "chromewebstore.google.com") ||
		(target.Scheme != "" && target.Scheme != "https") {
		return ""
	}
	return path.Clean("/" + target.Path)
}

func userCount(node *html.Node) string {
	for sibling := node.NextSibling; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type == html.ElementNode && sibling.Data == "a" {
			return ""
		}
		var countText string
		switch {
		case sibling.Type == html.TextNode:
			countText = sibling.Data
		case sibling.Type == html.ElementNode && sibling.Data != "script" && sibling.Data != "style":
			var text strings.Builder
			for child := range sibling.Descendants() {
				if child.Type == html.TextNode {
					text.WriteString(child.Data)
				}
			}
			countText = text.String()
		}
		if match := usersPattern.FindStringSubmatch(strings.TrimSpace(countText)); match != nil {
			return match[1]
		}
	}
	return ""
}

type listing struct {
	item record
	at   time.Time
}

func (r *researcher) metadata(id string) listing {
	address := store + "/detail/" + id + "?hl=en"
	saved, err := r.fetchPage(address, filepath.Join(r.cache, "listings", id+".html"))
	failure := func(err error) listing {
		return listing{item: record{"id": id, "error": err.Error()}, at: saved.at}
	}
	if err != nil {
		return failure(err)
	}
	doc, err := html.Parse(bytes.NewReader(saved.data))
	if err != nil {
		return failure(err)
	}
	var title, users string
	repos := map[string]bool{}
	for node := range doc.Descendants() {
		if node.Type != html.ElementNode {
			continue
		}
		switch node.Data {
		case "meta":
			if attribute(node, "property") == "og:title" {
				title = attribute(node, "content")
			}
		case "a":
			href := attribute(node, "href")
			if users == "" {
				listingPath := storePath(href)
				if listingPath == "/category/extensions" || strings.HasPrefix(listingPath, "/category/extensions/") {
					users = userCount(node)
				}
			}
			if repo := githubPattern.FindString(href); strings.HasPrefix(href, "https://github.com/") && repo != "" {
				repos[repo] = true
			}
		}
	}
	if title == "" || users == "" {
		return failure(errors.New("missing title or user count"))
	}
	count, err := strconv.Atoi(strings.ReplaceAll(users, ",", ""))
	if err != nil {
		return failure(err)
	}
	return listing{at: saved.at, item: record{
		"id": id, "name": strings.TrimSuffix(title, " - Chrome Web Store"),
		"users": count, "listing_url": address, "listing_sha256": hash(saved.data),
		"listing_captured_at": saved.at, "github_links": slices.Sorted(maps.Keys(repos)),
	}}
}

func (r *researcher) collect() error {
	candidates := map[string]bool{}
	for _, extension := range research.Supplemental {
		candidates[extension.ID] = true
	}
	var oldest, latest time.Time
	includeCapture := func(at time.Time) {
		if at.IsZero() {
			return
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		if at.After(latest) {
			latest = at
		}
	}
	categoryCounts := map[string]int{}
	for _, category := range research.Categories {
		saved, err := r.fetchPage(store+"/category/extensions/"+category+"?hl=en",
			filepath.Join(r.cache, "categories", strings.ReplaceAll(category, "/", "-")+".html"))
		if err != nil {
			return err
		}
		includeCapture(saved.at)
		doc, err := html.Parse(bytes.NewReader(saved.data))
		if err != nil {
			return err
		}
		ids := map[string]bool{}
		for node := range doc.Descendants() {
			if node.Type == html.ElementNode && node.Data == "a" {
				target := storePath(attribute(node, "href"))
				if id := path.Base(target); strings.HasPrefix(target, "/detail/") && extensions.ValidExtensionID(id) {
					ids[id], candidates[id] = true, true
				}
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("no IDs in category %s", category)
		}
		categoryCounts[category] = len(ids)
	}
	const seedFile = "upstream-top-600.json"
	const seedURL = "https://github.com/HughParry/chrome-extension-identifier/blob/main/data/top-600.json"
	var seed struct {
		GeneratedAt string `json:"generatedAt"`
		Extensions  []struct {
			ID string `json:"id"`
		} `json:"extensions"`
	}
	var seedData record
	if data, err := os.ReadFile(filepath.Join(r.cache, seedFile)); err == nil {
		if err := repojson.Default.DecodeInto(bytes.NewReader(data), &seed); err != nil {
			return fmt.Errorf("%s: %w", seedFile, err)
		}
		if len(seed.Extensions) == 0 {
			return fmt.Errorf("%s: no seed extensions", seedFile)
		}
		for _, item := range seed.Extensions {
			if !extensions.ValidExtensionID(item.ID) {
				return fmt.Errorf("%s: invalid extension ID %q", seedFile, item.ID)
			}
			candidates[item.ID] = true
		}
		seedData = record{"url": seedURL, "sha256": hash(data), "generated_at": seed.GeneratedAt}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	records := pool(slices.Sorted(maps.Keys(candidates)), r.metadata)
	valid, failed := []record{}, []record{}
	for _, result := range records {
		includeCapture(result.at)
		item := result.item
		if _, exists := item["error"]; exists {
			failed = append(failed, item)
		} else {
			valid = append(valid, item)
		}
	}
	slices.SortFunc(valid, func(a, b record) int {
		return cmp.Or(
			cmp.Compare(b["users"].(int), a["users"].(int)),
			cmp.Compare(a["id"].(string), b["id"].(string)),
		)
	})
	if len(valid) < 150 {
		return fmt.Errorf("only %d listings have user counts", len(valid))
	}
	for index, item := range valid[:150] {
		item["rank"] = index + 1
	}
	listingURLs := make([]string, len(research.Supplemental))
	for index, extension := range research.Supplemental {
		listingURLs[index] = store + "/detail/" + extension.ID + "?hl=en"
	}
	method := "150 highest displayed user counts among category-listing IDs and supplemental popular IDs"
	if seedData != nil {
		method += " and IDs from the cached upstream top-600 seed"
	}
	output := record{
		"collected_at":       latest,
		"collection_started": oldest,
		"method":             method + "; ties sorted by ID",
		"limitations": []string{
			"Category discovery cannot prove a global top 150",
			"Store user counts are rounded displayed counts, not exact installs",
			"Popularity does not establish configuration complexity",
			"The collection range uses the capture timestamps of category and listing pages, or cache file modification time for older captures",
		},
		"categories": categoryCounts, "supplemental_listing_urls": listingURLs,
		"candidate_count": len(candidates), "valid_listing_count": len(valid),
		"failed_listings": failed, "extensions": valid[:150],
	}
	if seedData != nil {
		output["seed"] = seedData
	}
	if err := writeJSON(filepath.Join(r.cache, "pool.json"), output); err != nil {
		return err
	}
	fmt.Printf("Collected %d listings from %d candidates\n", len(valid), len(candidates))
	return nil
}

func crxArchive(data []byte) (*zip.Reader, error) {
	if len(data) < 12 || string(data[:4]) != "Cr24" {
		return nil, fmt.Errorf("response is not a CRX or has a truncated header")
	}
	version := binary.LittleEndian.Uint32(data[4:8])
	var offset uint64
	switch version {
	case 3:
		offset = 12 + uint64(binary.LittleEndian.Uint32(data[8:12]))
	case 2:
		if len(data) < 16 {
			return nil, fmt.Errorf("truncated CRX2 header")
		}
		offset = 16 + uint64(binary.LittleEndian.Uint32(data[8:12])) + uint64(binary.LittleEndian.Uint32(data[12:16]))
	default:
		return nil, fmt.Errorf("unsupported CRX version %d", version)
	}
	if offset > uint64(len(data)) {
		return nil, fmt.Errorf("truncated CRX payload")
	}
	return zip.NewReader(bytes.NewReader(data[offset:]), int64(len(data))-int64(offset))
}

func readEntry(entry *zip.File) ([]byte, error) {
	const limit = 30 * 1024 * 1024
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeds 30 MiB inspection limit", entry.Name)
	}
	return data, nil
}

const inspectionVersion = "v1"

func (r *researcher) packageEvidence(id, address, digest string, data []byte) (record, error) {
	archive, err := crxArchive(data)
	if err != nil {
		return nil, err
	}
	manifestFile, err := archive.Open("manifest.json")
	if err != nil {
		return nil, err
	}
	var manifest record
	err = repojson.Default.DecodeInto(io.LimitReader(manifestFile, 30*1024*1024), &manifest)
	closeErr := manifestFile.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	destination := filepath.Join(r.cache, "unpacked", inspectionVersion, id, digest)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	unpacked, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unpacked.Close() }()
	evidence, skipped := []record{}, []record{}
	repos := map[string]bool{}
	counts := map[string]int{}
	for key := range markers {
		counts[key] = 0
	}
	var jsBytes uint64
	jsFiles := 0
	for _, entry := range archive.File {
		if path.IsAbs(entry.Name) || slices.Contains(strings.Split(entry.Name, "/"), "..") {
			return nil, fmt.Errorf("unsafe archive path: %s", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			skipped = append(skipped, record{"file": entry.Name, "reason": "archive symlink"})
			if err := unpacked.Remove(entry.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if entry.UncompressedSize64 > 30*1024*1024 {
			skipped = append(skipped, record{"file": entry.Name, "reason": "larger than 30 MiB inspection limit"})
			continue
		}
		ext := path.Ext(entry.Name)
		if !slices.Contains([]string{".js", ".mjs", ".json", ".html"}, ext) {
			continue
		}
		body, err := readEntry(entry)
		if err != nil {
			return nil, err
		}
		text := strings.ToValidUTF8(string(body), "\uFFFD")
		if err := unpacked.MkdirAll(path.Dir(entry.Name), 0o755); err != nil {
			return nil, err
		}
		if err := unpacked.WriteFile(entry.Name, []byte(text), 0o644); err != nil {
			return nil, err
		}
		if ext == ".js" || ext == ".mjs" {
			jsBytes += entry.UncompressedSize64
			jsFiles++
			hits := map[string]int{}
			total := 0
			for key, pattern := range markers {
				hits[key] = len(pattern.FindAllStringIndex(text, -1))
				counts[key] += hits[key]
				total += hits[key]
			}
			if total > 0 {
				evidence = append(evidence, record{"file": entry.Name, "markers": hits, "sha256": hash(body)})
			}
		}
		for _, repo := range githubPattern.FindAllString(text, -1) {
			repos[repo] = true
		}
	}
	options := manifest["options_page"]
	if options == nil || options == "" {
		options = manifest["options_ui"]
	}
	var storageSchema any
	if storage, ok := manifest["storage"].(map[string]any); ok {
		storageSchema = storage["managed_schema"]
	}
	permissions, exists := manifest["permissions"]
	if !exists {
		permissions = []any{}
	}
	hostPermissions, exists := manifest["host_permissions"]
	if !exists {
		hostPermissions = []any{}
	}
	return record{
		"status": "inspected", "package_url": address, "package_sha256": digest,
		"version": manifest["version"], "manifest_version": manifest["manifest_version"],
		"permissions": permissions, "host_permissions": hostPermissions,
		"options_page": options, "storage_schema": storageSchema,
		"js_file_count": jsFiles, "js_bytes": jsBytes, "markers": counts,
		"evidence": evidence, "skipped_files": skipped,
		"repository_candidates": slices.Sorted(maps.Keys(repos)),
	}, nil
}

type inspection struct {
	result record
	err    error
}

// Listing links can change independently of the package
func includeListingRepositories(result, item record) {
	repos := map[string]bool{}
	for _, links := range []any{result["repository_candidates"], item["github_links"]} {
		switch links := links.(type) {
		case []string:
			for _, repo := range links {
				repos[repo] = true
			}
		case []any:
			for _, link := range links {
				if repo, ok := link.(string); ok {
					repos[repo] = true
				}
			}
		}
	}
	result["repository_candidates"] = slices.Sorted(maps.Keys(repos))
	result["configuration_review"] = "pending"
}

func (r *researcher) inspectPackage(item record) inspection {
	id, ok := item["id"].(string)
	if !ok || !extensions.ValidExtensionID(id) {
		return inspection{err: fmt.Errorf("invalid extension ID: %v", item["id"])}
	}
	address, err := extensions.ChromeStoreCRXDownloadURLForVersion(
		"https://clients2.google.com/service/update2/crx", id, "150.0.0.0",
	)
	if err != nil {
		return inspection{err: err}
	}
	unavailable := func(err error) inspection {
		fmt.Printf("%3v %v: unavailable\n", item["rank"], item["name"])
		return inspection{result: record{"status": "unavailable", "error": err.Error()}}
	}
	packageFile := filepath.Join(r.cache, "packages", id+".crx")
	data, err := os.ReadFile(packageFile)
	downloaded := errors.Is(err, os.ErrNotExist)
	if downloaded {
		data, err = r.client.Read(context.Background(), address)
	}
	if err != nil {
		return unavailable(err)
	}
	digest := hash(data)
	saved := filepath.Join(r.cache, "inspections", inspectionVersion, id+".json")
	unpacked := filepath.Join(r.cache, "unpacked", inspectionVersion, id, digest)
	var result record
	cached := false
	if err := readJSON(saved, &result); err == nil {
		if result["status"] == "inspected" && result["package_url"] == address && result["package_sha256"] == digest {
			info, err := os.Stat(unpacked)
			cached = err == nil && info.IsDir()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return inspection{err: err}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return inspection{err: err}
	}
	if !cached {
		result, err = r.packageEvidence(id, address, digest, data)
		if err != nil {
			return unavailable(err)
		}
	}
	if downloaded {
		// Only keep new downloads after they pass static inspection
		if err := fileutil.WriteFile(packageFile, data, fileutil.DefaultFilePerm); err != nil {
			return inspection{err: err}
		}
	}
	if !cached {
		if err := writeJSON(saved, result); err != nil {
			return inspection{err: err}
		}
		fmt.Printf("%3v %v: inspected\n", item["rank"], item["name"])
	}
	includeListingRepositories(result, item)
	return inspection{result: result}
}

func (r *researcher) inspect() error {
	var output record
	file := filepath.Join(r.cache, "pool.json")
	if err := readJSON(file, &output); err != nil {
		return err
	}
	list, ok := output["extensions"].([]any)
	if !ok {
		return fmt.Errorf("pool.json: missing extensions array")
	}
	items := make([]record, len(list))
	ids := make(map[string]bool, len(items))
	for index, item := range list {
		var ok bool
		items[index], ok = item.(map[string]any)
		if !ok {
			return fmt.Errorf("pool.json: extension %d is not an object", index)
		}
		id, ok := items[index]["id"].(string)
		if !ok || !extensions.ValidExtensionID(id) || ids[id] {
			return fmt.Errorf("pool.json: invalid or duplicated extension ID %v", items[index]["id"])
		}
		ids[id] = true
	}
	for index, result := range pool(items, r.inspectPackage) {
		if result.err != nil {
			return result.err
		}
		items[index]["inspection"] = result.result
	}
	return writeJSON(file, output)
}

func run() error {
	_, source, _, _ := runtime.Caller(0)
	root := flag.String("root", filepath.Dir(source), "directory containing corpus.json")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "Usage: corpus [-root DIRECTORY] collect|inspect|shortlist")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || !slices.Contains([]string{"collect", "inspect", "shortlist"}, flag.Arg(0)) {
		flag.Usage()
		return fmt.Errorf("choose collect, inspect, or shortlist")
	}
	cache := os.Getenv("EXTENSION_RESEARCH_CACHE")
	if cache == "" {
		cache = filepath.Join(*root, "../../../.sources/extension-research")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	r := researcher{root: *root, cache: cache, client: extensions.HTTPClient{
		UserAgent:    "tweezle-extension-config-research/1.0",
		Headers:      map[string]string{"Accept-Language": "en-US,en;q=0.9"},
		RetryWaitMin: time.Second,
		RetryWaitMax: 4 * time.Second,
	}}
	if flag.Arg(0) == "collect" {
		return r.collect()
	}
	if flag.Arg(0) == "inspect" {
		return r.inspect()
	}
	return r.shortlist()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
