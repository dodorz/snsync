package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeGitHub serves a release plus its assets.
func fakeGitHub(t *testing.T, tag string, assets map[string][]byte, checksums bool) *Client {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/repos/dodorz/snsync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		writeRelease(w, srv.URL, tag, assets, checksums)
	})
	mux.HandleFunc("/repos/dodorz/snsync/releases/tags/"+tag, func(w http.ResponseWriter, r *http.Request) {
		writeRelease(w, srv.URL, tag, assets, checksums)
	})
	for name, data := range assets {
		data := data
		mux.HandleFunc("/download/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(data)
		})
	}
	if checksums {
		mux.HandleFunc("/download/"+checksumAsset, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(checksumFile(tag, assets))
		})
	}
	return &Client{Repo: "dodorz/snsync", APIBase: srv.URL, HTTP: srv.Client()}
}

func writeRelease(w http.ResponseWriter, base, tag string, assets map[string][]byte, checksums bool) {
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	}
	rel := struct {
		TagName string  `json:"tag_name"`
		Name    string  `json:"name"`
		Body    string  `json:"body"`
		HTMLURL string  `json:"html_url"`
		Assets  []asset `json:"assets"`
	}{TagName: tag, Name: tag, Body: "notes", HTMLURL: "https://example/rel"}
	for name, data := range assets {
		rel.Assets = append(rel.Assets, asset{Name: name, URL: base + "/download/" + name, Size: int64(len(data))})
	}
	if checksums {
		sums := checksumFile(tag, assets)
		rel.Assets = append(rel.Assets, asset{Name: checksumAsset, URL: base + "/download/" + checksumAsset, Size: int64(len(sums))})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rel)
}

func checksumFile(tag string, assets map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range assets {
		b.WriteString(sha(data) + "  " + name + "\n")
	}
	return []byte(b.String())
}

func TestLatestRelease(t *testing.T) {
	bin := []byte("binary")
	c := fakeGitHub(t, "0.3.0", map[string][]byte{AssetName("0.3.0", "linux", "amd64"): bin}, true)

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "0.3.0" || len(rel.Assets) != 2 {
		t.Fatalf("unexpected release: %+v", rel)
	}
	if a, ok := rel.AssetByName(AssetName("0.3.0", "linux", "amd64")); !ok || a.Name == "" {
		t.Fatalf("platform asset missing: %+v", rel.Assets)
	}

	sums, err := c.Checksums(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(sums, AssetName("0.3.0", "linux", "amd64"), bin); !ok || err != nil {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
}

func TestReleaseByTagStripsVPrefix(t *testing.T) {
	c := fakeGitHub(t, "0.3.0", map[string][]byte{"x": []byte("y")}, false)
	if _, err := c.Release(context.Background(), "v0.3.0"); err != nil {
		t.Fatalf("v-prefixed tag should work: %v", err)
	}
}

func TestVerifyRejectsTamperedAsset(t *testing.T) {
	good := []byte("binary")
	sums := map[string]string{"snsync-0.3.0-linux-amd64": sha(good)}
	if _, err := Verify(sums, "snsync-0.3.0-linux-amd64", []byte("evil")); err == nil {
		t.Fatal("tampered asset passed verification")
	}
	// An asset the checksum file does not mention is accepted but unverified.
	if ok, err := Verify(sums, "other-asset", good); ok || err != nil {
		t.Fatalf("unlisted asset: ok=%v err=%v", ok, err)
	}
}

func TestPlatformAsset(t *testing.T) {
	rel := &Release{Tag: "0.3.0", Assets: []Asset{
		{Name: "snsync-0.3.0-linux-arm64"},
		{Name: "snsync-0.3.0-windows-amd64.7z"},
		{Name: "snsync-0.3.0-windows-amd64.zip"},
	}}
	if a, err := PlatformAsset(rel, "linux", "arm64"); err != nil || a.Name != "snsync-0.3.0-linux-arm64" {
		t.Fatalf("linux/arm64: %v %v", a.Name, err)
	}
	if a, err := PlatformAsset(rel, "windows", "amd64"); err != nil || a.Name != "snsync-0.3.0-windows-amd64.zip" {
		t.Fatalf("windows/amd64: %v %v", a.Name, err)
	}
	if _, err := PlatformAsset(rel, "plan9", "mips"); err == nil {
		t.Fatal("missing platform should error")
	} else if !strings.Contains(err.Error(), "http") && !strings.Contains(err.Error(), "looked for") {
		t.Fatalf("error should say what was looked for: %v", err)
	}
}

func zipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBinaryFrom(t *testing.T) {
	if got, err := BinaryFrom("snsync-0.3.0-linux-amd64", []byte("raw")); err != nil || string(got) != "raw" {
		t.Fatalf("raw asset: %q %v", got, err)
	}

	z := zipBytes(t, map[string][]byte{"README.md": []byte("docs"), "snsync.exe": []byte("exe")})
	if got, err := BinaryFrom("snsync-0.3.0-windows-amd64.zip", z); err != nil || string(got) != "exe" {
		t.Fatalf("zip asset: %q %v", got, err)
	}

	bad := zipBytes(t, map[string][]byte{"README.md": []byte("docs")})
	if _, err := BinaryFrom("x.zip", bad); err == nil {
		t.Fatal("zip without snsync should fail")
	}
	if _, err := BinaryFrom("snsync-0.3.0-linux-amd64", nil); err == nil {
		t.Fatal("empty download should fail")
	}
}

func TestInstallForReplacesBinary(t *testing.T) {
	bin := []byte("#!/bin/sh\necho new\n")
	name := AssetName("0.3.0", runtime.GOOS, runtime.GOARCH)
	c := fakeGitHub(t, "0.3.0", map[string][]byte{name: bin}, true)

	// Stand in for the running executable.
	dir := t.TempDir()
	target := filepath.Join(dir, "snsync")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.Target = target

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "0.3.0" {
		t.Fatalf("unexpected tag: %q", rel.Tag)
	}
	got, err := c.Install(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("installed to %q, want %q", got, target)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, bin) {
		t.Fatalf("binary not replaced: %q", b)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced binary is not executable: %v", st.Mode())
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftover files: %v", names)
	}
}

// A release whose published checksum does not match the asset must be rejected
// before the running binary is touched.
func TestInstallRejectsBadChecksum(t *testing.T) {
	name := AssetName("0.3.0", runtime.GOOS, runtime.GOARCH)
	payload := []byte("payload")
	wrong := strings.Repeat("0", 64) + "  " + name + "\n"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/dodorz/snsync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		tag := "0.3.0"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []asset{
				{Name: name, URL: srv.URL + "/dl/" + name},
				{Name: checksumAsset, URL: srv.URL + "/dl/" + checksumAsset},
			},
		})
	})
	mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) })
	mux.HandleFunc("/dl/"+checksumAsset, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(wrong)) })

	dir := t.TempDir()
	target := filepath.Join(dir, "snsync")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Client{Repo: "dodorz/snsync", APIBase: srv.URL, HTTP: srv.Client(), Target: target}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Install(context.Background(), rel); err == nil {
		t.Fatal("install accepted a bad checksum")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Fatalf("binary was modified despite the bad checksum: %q", b)
	}
}

// A platform without an asset is reported before anything is downloaded.
func TestInstallForUnsupportedPlatform(t *testing.T) {
	c := fakeGitHub(t, "0.3.0", map[string][]byte{"other": []byte("x")}, false)
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstallFor(context.Background(), rel, "plan9", "mips"); err == nil {
		t.Fatal("install should fail for an unpublished platform")
	}
}

// The release lookup is retried when GitHub answers with a transient error.
func TestReleaseIsRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"0.3.0"}`))
	}))
	defer srv.Close()

	c := &Client{Repo: "dodorz/snsync", APIBase: srv.URL, HTTP: srv.Client()}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "0.3.0" || calls < 2 {
		t.Fatalf("retry did not happen: tag=%q calls=%d", rel.Tag, calls)
	}
}

// A 404 is not retried: it means the release does not exist.
func TestMissingReleaseIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{Repo: "dodorz/snsync", APIBase: srv.URL, HTTP: srv.Client()}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("missing release should fail")
	}
	if calls != 1 {
		t.Fatalf("404 was retried: %d calls", calls)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.2.0", "0.3.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.2.1", true},
		{"0.3.0", "0.2.9", false},
		{"0.2.0", "v0.3.0", true},
		{"dev", "0.2.0", true},
		{"", "0.2.0", true},
		{"0.2.0", "", false},
		{"0.2.0-4-gabc123", "0.2.0", false},
		{"0.2.0-4-gabc123", "0.3.0", true},
		{"0.2.0-dirty", "0.2.0", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.current, tc.latest); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.1.1", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0", "1.0.0", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"1.0.0-rc2", "1.0.0-beta", 1},
		{"1.0.0+build5", "1.0.0", 0},
		{"0.2.0-3-gdeadbee", "0.2.0", 0},
		{"0.2.0-dirty", "0.2.0", 0},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseChecksumsIgnoresJunk(t *testing.T) {
	sums := ParseChecksums([]byte("# comment\nnot-a-sum  file\n" + strings.Repeat("a", 64) + " *starred\n"))
	if len(sums) != 1 {
		t.Fatalf("parsed %d entries: %v", len(sums), sums)
	}
	if _, ok := sums["starred"]; !ok {
		t.Fatalf("star-prefixed line not handled: %v", sums)
	}
}
