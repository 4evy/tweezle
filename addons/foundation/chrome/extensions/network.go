package extensions

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	fileutil "github.com/4evy/tweezle/internal/file"
	"github.com/google/go-github/v93/github"
	"github.com/gophertools/requests/v2"
	"github.com/hashicorp/go-retryablehttp"
)

const (
	defaultHTTPTimeout      = time.Minute
	defaultHTTPRetryWaitMin = 200 * time.Millisecond
	defaultHTTPRetryWaitMax = 2 * time.Second
	defaultHTTPRetryMax     = 3
	maxChromeVersionBytes   = 4 << 10
	maxReleaseMetadataBytes = 4 << 20
	latestChromeVersionURL  = "https://googlechromelabs.github.io/chrome-for-testing/LATEST_RELEASE_STABLE"
	defaultUserAgent        = "github.com/4evy/tweezle"
	userAgentPrefix         = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/"
	// Safari/537.36 is Chromium's frozen compatibility token, not Safari's version
	userAgentSuffix = " Safari/537.36"
	githubAPIURL    = "https://api.github.com/"
)

func requireSuccessfulHTTPStatus(response *http.Response) error {
	if response.StatusCode >= http.StatusOK &&
		response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	return fmt.Errorf(
		"%w: unexpected status: %d",
		(*requests.ResponseError)(response),
		response.StatusCode,
	)
}

type HTTPClient struct {
	Client        *http.Client
	GitHubToken   string
	UserAgent     string
	ChromeVersion string
	Headers       map[string]string
	Timeout       time.Duration
	RetryMax      *int
	RetryWaitMin  time.Duration
	RetryWaitMax  time.Duration
}

// Read downloads a response body using the same headers and retry policy as
// Download, without writing a file
func (client HTTPClient) Read(ctx context.Context, rawURL string) ([]byte, error) {
	var data bytes.Buffer
	if err := client.request(rawURL, maxArtifactBytes).Handle(requests.ToBytesBuffer(&data)).Fetch(ctx); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func (client HTTPClient) Download(ctx context.Context, target, rawURL string) error {
	return fileutil.WriteWith(
		target,
		fileutil.DefaultFilePerm,
		func(writer io.Writer) error {
			return client.request(rawURL, maxArtifactBytes).Handle(func(response *http.Response) error {
				_, err := io.Copy(writer, response.Body)
				return err
			}).Fetch(ctx)
		},
	)
}

func (client HTTPClient) ResolveURL(ctx context.Context, rawURL string) (string, error) {
	var resolvedURL string
	err := client.request(rawURL, maxArtifactBytes).
		Head().
		Handle(func(response *http.Response) error {
			if response.Request == nil || response.Request.URL == nil {
				return errors.New("extension response has no request URL")
			}
			resolvedURL = response.Request.URL.String()
			return nil
		}).
		Fetch(ctx)
	if err != nil {
		return "", err
	}
	return resolvedURL, nil
}

func (client HTTPClient) ResolveLatestChromeVersion(ctx context.Context) (string, error) {
	var version string
	err := client.request(latestChromeVersionURL, maxChromeVersionBytes).
		ToString(&version).
		Fetch(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch latest stable Chrome version: %w", err)
	}
	version = strings.TrimSpace(version)
	if !ValidExternalVersion(version) {
		return "", fmt.Errorf("latest stable Chrome version is invalid: %q", version)
	}
	return version, nil
}

func (client HTTPClient) ResolveLatestGitHubRelease(
	ctx context.Context,
	repository,
	assetTemplate string,
) (ReleaseArtifact, error) {
	if !ValidGitHubRepository(repository) {
		return ReleaseArtifact{}, fmt.Errorf("invalid GitHub repository %q", repository)
	}
	owner, name, _ := strings.Cut(repository, "/")
	options := []github.ClientOptionsFunc{
		github.WithHTTPClient(client.httpClient(githubAPIURL, maxReleaseMetadataBytes)),
		github.WithUserAgent(client.userAgent()),
	}
	if client.GitHubToken != "" {
		options = append(options, github.WithAuthToken(client.GitHubToken))
	}
	api, err := github.NewClient(options...)
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf("create GitHub client: %w", err)
	}
	release, _, err := api.Repositories.GetLatestRelease(ctx, owner, name)
	if err != nil {
		return ReleaseArtifact{}, fmt.Errorf("get latest GitHub release for %s: %w", repository, err)
	}
	tag := release.GetTagName()
	if tag == "" {
		return ReleaseArtifact{}, fmt.Errorf("latest GitHub release for %s has no tag", repository)
	}
	assetName := strings.ReplaceAll(assetTemplate, releaseTagPlaceholder, tag)
	for _, asset := range release.Assets {
		if asset.GetName() != assetName {
			continue
		}
		checksum, err := NormalizeSHA256(asset.GetDigest())
		if err != nil {
			return ReleaseArtifact{}, fmt.Errorf("asset %s has no usable GitHub digest: %w", assetName, err)
		}
		version, _ := strings.CutPrefix(tag, releaseVersionPrefix)
		address := asset.GetBrowserDownloadURL()
		if !validHTTPURL(address) {
			return ReleaseArtifact{}, fmt.Errorf("asset %s download URL must use HTTP(S)", assetName)
		}
		return ReleaseArtifact{
			Version: version,
			URL:     address,
			SHA256:  checksum,
		}, nil
	}
	return ReleaseArtifact{}, fmt.Errorf("release %s has no asset named %s", tag, assetName)
}

func (client HTTPClient) request(rawURL string, maxBytes int64) *requests.Builder {
	return requests.URL(rawURL).
		Client(client.httpClient(rawURL, maxBytes)).
		UserAgent(client.userAgent()).
		AddValidator(requireSuccessfulHTTPStatus)
}

func (client HTTPClient) httpClient(headerURL string, maxBytes int64) *http.Client {
	base := http.Client{Timeout: cmp.Or(client.Timeout, defaultHTTPTimeout)}
	if client.Client != nil {
		base = *client.Client
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	origin, _ := url.Parse(headerURL)
	base.Transport = requests.RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !isHTTPURL(request.URL) {
			return nil, errors.New("extension request URL must use HTTP(S)")
		}
		if len(client.Headers) > 0 && origin != nil &&
			request.URL.Scheme == origin.Scheme && request.URL.Host == origin.Host {
			request = request.Clone(request.Context())
			for name, value := range client.Headers {
				request.Header.Set(name, value)
			}
		}
		response, err := transport.RoundTrip(request)
		if err == nil && response.Body != nil {
			remaining := maxBytes + 1
			if response.ContentLength > maxBytes {
				remaining = 0
			}
			response.Body = &boundedResponseBody{
				R: response.Body, N: remaining,
				Closer: response.Body,
				limit:  maxBytes,
			}
		}
		return response, err
	})
	retry := retryablehttp.NewClient()
	retry.HTTPClient = &base
	retry.Logger = nil
	retry.RetryWaitMin = cmp.Or(client.RetryWaitMin, defaultHTTPRetryWaitMin)
	retry.RetryWaitMax = cmp.Or(client.RetryWaitMax, defaultHTTPRetryWaitMax)
	retry.RetryMax = defaultHTTPRetryMax
	if client.RetryMax != nil {
		retry.RetryMax = *client.RetryMax
	}
	return retry.StandardClient()
}

type boundedResponseBody struct {
	io.LimitedReader
	io.Closer
	limit int64
}

func (body *boundedResponseBody) Read(data []byte) (int, error) {
	n, err := body.LimitedReader.Read(data)
	if body.N == 0 {
		// The last byte proves the body exceeded its budget. Do not deliver it.
		return max(0, n-1), fmt.Errorf("HTTP response exceeds %d bytes", body.limit)
	}
	return n, err
}

func (client HTTPClient) userAgent() string {
	if client.UserAgent != "" {
		return client.UserAgent
	}
	if client.ChromeVersion != "" {
		return chromeUserAgent(client.ChromeVersion)
	}
	return defaultUserAgent
}

func (config NetworkConfig) HTTPClient(githubToken string) HTTPClient {
	return HTTPClient{
		GitHubToken:   githubToken,
		UserAgent:     config.UserAgent,
		ChromeVersion: config.ChromeVersion,
		Headers:       config.Headers,
		Timeout:       time.Duration(config.TimeoutSeconds) * time.Second,
		RetryMax:      config.RetryMax,
		RetryWaitMin:  time.Duration(config.RetryWaitMinMilliseconds) * time.Millisecond,
		RetryWaitMax:  time.Duration(config.RetryWaitMaxMilliseconds) * time.Millisecond,
	}
}

func chromeUserAgent(version string) string {
	return userAgentPrefix + version + userAgentSuffix
}
