package extensions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultGitExecutable = "git"
	gitTemporaryPrefix   = "tweezle-extension-git-"
	maxGitCommandOutput  = 64 << 10
	maxGitCheckoutTime   = 5 * time.Minute
)

type GitClient struct {
	Executable string
}

type hostedGitProvider struct {
	baseURL        string
	suffix         string
	allowSubgroups bool
	tildeOwner     bool
}

var hostedGitProviders = map[GitProvider]hostedGitProvider{
	GitProviderGitHub:    {baseURL: "https://github.com/", suffix: ".git"},
	GitProviderGitLab:    {baseURL: "https://gitlab.com/", suffix: ".git", allowSubgroups: true},
	GitProviderCodeberg:  {baseURL: "https://codeberg.org/", suffix: ".git"},
	GitProviderSourceHut: {baseURL: "https://git.sr.ht/", tildeOwner: true},
}

func GitCloneURL(extension GitExtension) (string, error) {
	provider := extension.gitProvider()
	if provider == GitProviderGit {
		if extension.Repository != "" || !ValidGitURL(extension.URL) {
			return "", fmt.Errorf("invalid Git URL %q", extension.URL)
		}
		return extension.URL, nil
	}
	if extension.URL != "" {
		return "", fmt.Errorf("%s Git source must not set URL", provider)
	}
	if !ValidHostedGitRepository(provider, extension.Repository) {
		return "", fmt.Errorf(
			"invalid %s repository %q",
			provider,
			extension.Repository,
		)
	}
	spec := hostedGitProviders[provider]
	return spec.baseURL + extension.Repository + spec.suffix, nil
}

func (client GitClient) Checkout(
	ctx context.Context,
	destination string,
	extension GitExtension,
) (_ GitRevision, err error) {
	if err := validateGitExtension(extension); err != nil {
		return GitRevision{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, maxGitCheckoutTime)
	defer cancel()
	cloneURL, err := GitCloneURL(extension)
	if err != nil {
		return GitRevision{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return GitRevision{}, fmt.Errorf("create Git export destination: %w", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return GitRevision{}, fmt.Errorf("read Git export destination: %w", err)
	}
	if len(entries) != 0 {
		return GitRevision{}, errors.New("git export destination must be empty")
	}
	workDir, err := os.MkdirTemp("", gitTemporaryPrefix)
	if err != nil {
		return GitRevision{}, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(workDir)) }()

	fetchTarget := extension.Ref
	pinned := extension.updatePolicy() == UpdatePolicyPinned
	if pinned && fetchTarget == "" {
		fetchTarget = strings.ToLower(extension.Commit)
	}
	if fetchTarget == "" {
		fetchTarget = "HEAD"
	}
	objectID := extension.Commit
	if !pinned && ValidGitCommit(fetchTarget) {
		objectID = strings.ToLower(fetchTarget)
		fetchTarget = objectID
	}
	if objectID == "" {
		output, err := client.output(ctx, "identify remote object format", workDir, cloneURL,
			"ls-remote", "--exit-code", "--", cloneURL, fetchTarget,
		)
		if err != nil {
			return GitRevision{}, err
		}
		objectID, _, _ = strings.Cut(output, "\t")
		if !ValidGitCommit(objectID) {
			return GitRevision{}, errors.New("git returned no valid remote object ID")
		}
	}
	objectFormat := "sha1"
	if len(objectID) == 64 {
		objectFormat = "sha256"
	}

	repositoryDir := filepath.Join(workDir, "repository")
	archivePath := filepath.Join(workDir, "extension.zip")
	if err := client.run(ctx, "initialize repository", "", cloneURL,
		"init", "--quiet", "--object-format="+objectFormat, repositoryDir,
	); err != nil {
		return GitRevision{}, err
	}
	if err := client.run(ctx, "configure repository remote", "", cloneURL,
		"-C", repositoryDir, "remote", "add", "origin", cloneURL,
	); err != nil {
		return GitRevision{}, err
	}
	if err := client.run(ctx, "fetch repository revision", "", cloneURL,
		"-c", "core.hooksPath=/dev/null",
		"-c", "fetch.unpackLimit=1",
		"-C", repositoryDir,
		"fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", "origin", fetchTarget,
	); err != nil {
		return GitRevision{}, err
	}
	output, err := client.output(ctx, "resolve repository revision", repositoryDir, cloneURL,
		"rev-parse", "--verify", "FETCH_HEAD^{commit}",
	)
	if err != nil {
		return GitRevision{}, err
	}
	commit := strings.TrimSpace(output)
	if !ValidGitCommit(commit) {
		return GitRevision{}, fmt.Errorf("git returned invalid commit %q", commit)
	}
	if pinned && !strings.EqualFold(commit, extension.Commit) {
		return GitRevision{}, fmt.Errorf(
			"git ref %q resolved to commit %s, want pinned commit %s",
			extension.Ref,
			commit,
			extension.Commit,
		)
	}
	treeish := commit
	if extension.Subdirectory != "" {
		treeish += ":" + extension.Subdirectory
	}
	archive, err := os.Create(archivePath)
	if err != nil {
		return GitRevision{}, err
	}
	// Store files without compression so the write budget also bounds the
	// expanded export Git can produce before our ZIP validation runs
	exportErr := client.runTo(ctx, "export repository revision", repositoryDir, cloneURL, archive, maxArtifactBytes,
		"-c", "core.bigFileThreshold=1m", "archive", "--format=zip", "-0", treeish,
	)
	if err := errors.Join(exportErr, archive.Close()); err != nil {
		return GitRevision{}, err
	}
	if err := ExtractZipFile(archivePath, destination); err != nil {
		return GitRevision{}, fmt.Errorf("extract Git revision %s: %w", commit, err)
	}
	return GitRevision{Commit: commit}, nil
}

func (client GitClient) run(
	ctx context.Context,
	operation,
	directory,
	cloneURL string,
	arguments ...string,
) error {
	return client.runTo(ctx, operation, directory, cloneURL, io.Discard, 0, arguments...)
}

func (client GitClient) output(
	ctx context.Context,
	operation,
	directory,
	cloneURL string,
	arguments ...string,
) (string, error) {
	var output bytes.Buffer
	err := client.runTo(ctx, operation, directory, cloneURL, &output, maxGitCommandOutput, arguments...)
	return output.String(), err
}

func (client GitClient) runTo(
	ctx context.Context,
	operation, directory, cloneURL string,
	writer io.Writer,
	limit int64,
	arguments ...string,
) error {
	executable := client.Executable
	if executable == "" {
		executable = defaultGitExecutable
	}
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	command.WaitDelay = time.Second
	output := gitOutputBudget{writer: writer, remaining: limit}
	command.Stdout = writer
	if limit > 0 {
		command.Stdout = &output
	}
	var stderr bytes.Buffer
	diagnostic := gitOutputBudget{writer: &stderr, remaining: maxGitCommandOutput}
	command.Stderr = &diagnostic
	err := command.Run()
	if output.exceeded {
		return fmt.Errorf("%s: git output exceeds %d bytes", operation, limit)
	}
	if diagnostic.exceeded {
		return fmt.Errorf("%s: git diagnostics exceed %d bytes", operation, maxGitCommandOutput)
	}
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", operation, ctxErr)
	}
	detail := strings.TrimSpace(stderr.String())
	if detail != "" {
		detail = strings.ReplaceAll(detail, cloneURL, safeGitURL(cloneURL))
		return fmt.Errorf("%s: %w: %s", operation, err, detail)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type gitOutputBudget struct {
	writer    io.Writer
	remaining int64
	exceeded  bool
}

func (output *gitOutputBudget) Write(data []byte) (int, error) {
	if int64(len(data)) > output.remaining {
		output.exceeded = true
		return 0, errors.New("git output exceeds its byte budget")
	}
	written, err := output.writer.Write(data)
	output.remaining -= int64(written)
	return written, err
}

func safeGitURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}
