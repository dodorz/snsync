// Package update checks GitHub releases for a newer snsync and replaces the
// running executable with it.
//
// Everything is done with the standard library: the release metadata comes
// from the public GitHub API, the binary and its checksum from the release
// assets, and the swap is a rename next to the running binary.
package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRepo is the GitHub repository snsync is released from.
	DefaultRepo = "dodorz/snsync"
	// DefaultAPIBase is the public GitHub API.
	DefaultAPIBase = "https://api.github.com"

	checksumAsset = "checksums.txt"
	// maxAssetSize caps a download so a broken (or hostile) response cannot
	// fill the disk.
	maxAssetSize = 128 << 20
	// requestTimeout bounds one API call; downloads get downloadTimeout because
	// a release binary can take minutes on a slow link.
	requestTimeout  = 60 * time.Second
	downloadTimeout = 15 * time.Minute
	// attempts is how often a request is retried on a transient failure.
	attempts = 3
)

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is the subset of a GitHub release snsync needs.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Notes       string    `json:"body"`
	URL         string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// AssetByName returns the asset with the given name.
func (r *Release) AssetByName(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client talks to the GitHub API for one repository.
type Client struct {
	// Repo is "owner/name".
	Repo string
	// APIBase overrides the GitHub API root (tests, mirrors).
	APIBase string
	// HTTP overrides the HTTP client used for the API and downloads.
	HTTP *http.Client
	// Target overrides the executable to replace; empty means the running one.
	// It exists for tests.
	Target string
}

// New builds a client for the default repository.
func New() *Client { return &Client{Repo: DefaultRepo} }

func (c *Client) repo() string {
	if c.Repo == "" {
		return DefaultRepo
	}
	return c.Repo
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimSuffix(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{}
}

// get performs a GET and returns the body. Transient failures (network errors,
// truncated reads, 5xx, 429) are retried with a short backoff, which matters on
// links where GitHub's redirect to its CDN occasionally times out.
func (c *Client) get(ctx context.Context, url string, limit int64, accept string) ([]byte, int, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, 0, lastErr
			case <-time.After(time.Duration(attempt-1) * 2 * time.Second):
			}
		}
		b, status, err := c.getOnce(ctx, url, limit, accept)
		if err == nil {
			return b, status, nil
		}
		lastErr = err
		if !retryable(status, err) {
			return nil, status, err
		}
	}
	return nil, 0, lastErr
}

func retryable(status int, err error) bool {
	if status == http.StatusTooManyRequests || status >= 500 {
		return true
	}
	if status != 0 {
		return false
	}
	return !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) getOnce(ctx context.Context, url string, limit int64, accept string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "snsync")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, resp.StatusCode, fmt.Errorf("not found: %s", url)
		}
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return body, resp.StatusCode, nil
}

// Latest fetches the newest published (non-prerelease) release.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.fetchRelease(ctx, c.apiBase()+"/repos/"+c.repo()+"/releases/latest")
}

// Release fetches one release by tag. Tags in this repository carry no "v"
// prefix, but a leading one is stripped so either spelling works.
func (c *Client) Release(ctx context.Context, tag string) (*Release, error) {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if tag == "" {
		return nil, errors.New("empty release tag")
	}
	return c.fetchRelease(ctx, c.apiBase()+"/repos/"+c.repo()+"/releases/tags/"+tag)
}

func (c *Client) fetchRelease(ctx context.Context, url string) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, status, err := c.get(ctx, url, 4<<20, "application/vnd.github+json")
	if err != nil {
		if status == http.StatusNotFound {
			return nil, fmt.Errorf("github: no such release (%s)", url)
		}
		if status == http.StatusForbidden {
			return nil, fmt.Errorf("github: rate limited or forbidden (HTTP %d); try again later", status)
		}
		return nil, fmt.Errorf("github: %w", err)
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("github: bad response: %w", err)
	}
	if rel.Tag == "" {
		return nil, errors.New("github: release has no tag")
	}
	return &rel, nil
}

// Download fetches a release asset into memory.
func (c *Client) Download(ctx context.Context, url string) ([]byte, error) {
	if url == "" {
		return nil, errors.New("download: empty URL")
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, _, err := c.get(ctx, url, maxAssetSize, "")
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return data, nil
}

// Checksums returns the sha256 sums published with a release, keyed by asset
// name. A release without a checksum file yields an empty map and no error, so
// older releases can still be installed (with a warning by the caller).
func (c *Client) Checksums(ctx context.Context, rel *Release) (map[string]string, error) {
	a, ok := rel.AssetByName(checksumAsset)
	if !ok {
		return nil, nil
	}
	data, err := c.Download(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	return ParseChecksums(data), nil
}

// ParseChecksums reads "sha256␠␠name" lines as written by sha256sum.
func ParseChecksums(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = sum
	}
	return out
}

// AssetName is the file name of the raw binary for a platform, as the release
// workflow builds it.
func AssetName(tag, goos, goarch string) string {
	name := fmt.Sprintf("snsync-%s-%s-%s", tag, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// PlatformAsset picks the asset to install for the given platform. A raw
// binary is preferred; a .zip holding it is accepted as well (that is how
// Windows builds can be published, since a running .exe cannot be replaced any
// other way).
func PlatformAsset(rel *Release, goos, goarch string) (Asset, error) {
	candidates := []string{
		AssetName(rel.Tag, goos, goarch),
		fmt.Sprintf("snsync-%s-%s-%s.zip", rel.Tag, goos, goarch),
	}
	var found []string
	for _, name := range candidates {
		if a, ok := rel.AssetByName(name); ok {
			return a, nil
		}
		found = append(found, name)
	}
	return Asset{}, fmt.Errorf(
		"release %s has no downloadable asset for %s/%s (looked for %s); get it from %s",
		rel.Tag, goos, goarch, strings.Join(found, ", "), rel.URL)
}

// BinaryFrom extracts the snsync executable from a downloaded asset. A raw
// binary is returned unchanged; a .zip is searched for snsync(.exe).
func BinaryFrom(name string, data []byte) ([]byte, error) {
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		if len(data) == 0 {
			return nil, errors.New("downloaded file is empty")
		}
		return data, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	for _, f := range zr.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if base != "snsync" && base != "snsync.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("read zip entry: %w", err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxAssetSize+1))
		if err != nil {
			return nil, fmt.Errorf("read zip entry: %w", err)
		}
		if len(b) == 0 {
			return nil, errors.New("zip entry is empty")
		}
		return b, nil
	}
	return nil, errors.New("zip does not contain snsync")
}

// Verify compares data with the published sha256 sum for name. A missing sum
// (older releases, or a name the checksum file does not list) reports
// ok=false without an error.
func Verify(checksums map[string]string, name string, data []byte) (bool, error) {
	want, ok := checksums[name]
	if !ok {
		return false, nil
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return false, fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	return true, nil
}

// Install downloads the matching asset for this platform, verifies it when a
// checksum is published, and replaces the running executable. It returns the
// path that was replaced.
func (c *Client) Install(ctx context.Context, rel *Release) (string, error) {
	return c.InstallFor(ctx, rel, runtime.GOOS, runtime.GOARCH)
}

// InstallFor is Install with an explicit platform, for tests.
func (c *Client) InstallFor(ctx context.Context, rel *Release, goos, goarch string) (string, error) {
	asset, err := PlatformAsset(rel, goos, goarch)
	if err != nil {
		return "", err
	}
	data, err := c.Download(ctx, asset.URL)
	if err != nil {
		return "", err
	}
	sums, err := c.Checksums(ctx, rel)
	if err != nil {
		return "", err
	}
	if _, err := Verify(sums, asset.Name, data); err != nil {
		return "", err
	}
	bin, err := BinaryFrom(asset.Name, data)
	if err != nil {
		return "", err
	}
	target := c.Target
	if target == "" {
		if target, err = SelfPath(); err != nil {
			return "", err
		}
	}
	if err := Replace(target, bin); err != nil {
		return "", err
	}
	return target, nil
}

// SelfPath is the real path of the running executable, with symlinks resolved
// so an update replaces the binary the symlink points at instead of the link.
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// Replace overwrites target with data.
//
// On Unix the new binary is written next to the old one and renamed over it,
// which is atomic: a crash leaves either the old or the new executable. On
// Windows a running executable cannot be replaced, so the old one is renamed
// aside and removed on a later start (see CleanupOldBinaries).
func Replace(target string, data []byte) error {
	if len(data) == 0 {
		return errors.New("replace: empty binary")
	}
	dir := filepath.Dir(target)
	mode := os.FileMode(0o755)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".snsync-update-*")
	if err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("replace: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("replace: %w", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Rename(tmpName, target); err != nil {
			return fmt.Errorf("replace %s: %w (is the directory writable?)", target, err)
		}
		return nil
	}

	// Windows: move the running binary out of the way, put the new one in place
	// and restore the old one if that fails.
	old := target + ".old"
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("replace %s: %w (close other snsync processes and retry)", target, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Rename(old, target)
		return fmt.Errorf("replace %s: %w", target, err)
	}
	_ = os.Remove(old)
	return nil
}

// CleanupOldBinaries removes "<exe>.old", the leftover of a previous Windows
// update. It is best effort and never reports an error.
func CleanupOldBinaries() {
	exe, err := SelfPath()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
}

// IsNewer reports whether latest is a newer version than current.
//
// current may be "dev" (a build from source), in which case any published
// release counts as newer: the caller decides whether to act on it.
func IsNewer(current, latest string) bool {
	latest = strings.TrimSpace(latest)
	if latest == "" {
		return false
	}
	cur := strings.TrimSpace(current)
	if cur == "" || cur == "dev" {
		return true
	}
	return CompareVersions(latest, cur) > 0
}

// CompareVersions compares two version strings, returning -1, 0 or 1.
//
// It understands the plain "0.2.0" tags this repository uses, an optional "v"
// prefix, "-rc1"-style prereleases and the "-4-gabc123[-dirty]" suffix that
// `git describe` produces, which is stripped: such a build is the tagged
// version plus untagged commits, and treating it as a prerelease would make it
// look older than the release it descends from.
func CompareVersions(a, b string) int {
	ac, ap := parseVersion(a)
	bc, bp := parseVersion(b)
	for i := 0; i < len(ac) || i < len(bc); i++ {
		var x, y int
		if i < len(ac) {
			x = ac[i]
		}
		if i < len(bc) {
			y = bc[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	// A prerelease sorts before the release it leads up to.
	switch {
	case len(ap) == 0 && len(bp) == 0:
		return 0
	case len(ap) == 0:
		return 1
	case len(bp) == 0:
		return -1
	}
	for i := 0; i < len(ap) || i < len(bp); i++ {
		if i >= len(ap) {
			return -1
		}
		if i >= len(bp) {
			return 1
		}
		if c := comparePrerelease(ap[i], bp[i]); c != 0 {
			return c
		}
	}
	return 0
}

func comparePrerelease(a, b string) int {
	an, aNum := strconv.Atoi(a)
	bn, bNum := strconv.Atoi(b)
	switch {
	case aNum == nil && bNum == nil:
		if an == bn {
			return 0
		}
		if an < bn {
			return -1
		}
		return 1
	case aNum == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case bNum == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// gitDescribeRe matches the suffix `git describe` adds to a tag:
// "-4-gabc123" plus an optional "-dirty".
var gitDescribeRe = regexp.MustCompile(`-\d+-g[0-9a-fA-F]+(-dirty)?$`)

func parseVersion(v string) (core []int, pre []string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	v = gitDescribeRe.ReplaceAllString(v, "")
	// A lone "-dirty" (a build from modified sources at a tag) is not a
	// prerelease; it is that same version.
	v = strings.TrimSuffix(v, "-dirty")
	if i := strings.IndexByte(v, '+'); i >= 0 { // build metadata is ignored
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = strings.Split(v[i+1:], ".")
		v = v[:i]
	}
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(leadingDigits(part))
		if err != nil {
			n = 0
		}
		core = append(core, n)
	}
	for len(core) > 0 && core[len(core)-1] == 0 {
		core = core[:len(core)-1]
	}
	return core, pre
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return "0"
	}
	return s[:i]
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
