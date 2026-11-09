package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/dodorz/snsync/internal/update"
)

const (
	// updateCheckTimeout bounds a check: one API call, at most a couple of retries.
	updateCheckTimeout = 90 * time.Second
	// upgradeTimeout bounds an upgrade: the API call plus a download and
	// a checksum from GitHub's CDN, which is slow on some links.
	upgradeTimeout = 30 * time.Minute
)

// newUpdateClient builds a GitHub release client honoring SN_UPDATE_API (a
// mirror or proxy) and SN_UPDATE_REPO (a fork):
//
//	SN_UPDATE_API=https://api.github.com
//	SN_UPDATE_REPO=owner/name
func newUpdateClient() *update.Client {
	c := update.New()
	if v := os.Getenv("SN_UPDATE_REPO"); v != "" {
		c.Repo = v
	}
	if v := os.Getenv("SN_UPDATE_API"); v != "" {
		c.APIBase = v
	}
	return c
}

// resolveRelease returns the requested release, or the latest one when tag is
// empty.
func resolveRelease(ctx context.Context, c *update.Client, tag string) (*update.Release, error) {
	if tag != "" {
		return c.Release(ctx, tag)
	}
	return c.Latest(ctx)
}

// runUpdateCheck implements `snsync update`.
//
// It only reports; it never touches the running executable.
func runUpdateCheck(c *update.Client, current, tag string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()

	rel, err := resolveRelease(ctx, c, tag)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "current:  %s (%s/%s)\n", displayVersion(current), runtime.GOOS, runtime.GOARCH)
	if rel.PublishedAt.IsZero() {
		fmt.Fprintf(out, "release:  %s\n", rel.Tag)
	} else {
		fmt.Fprintf(out, "release:  %s (published %s)\n", rel.Tag, rel.PublishedAt.Local().Format("2006-01-02"))
	}

	if tag == "" && !update.IsNewer(current, rel.Tag) {
		fmt.Fprintln(out, "up to date")
		return nil
	}

	asset, err := update.PlatformAsset(rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fmt.Fprintf(out, "%s\n", err)
		if rel.URL != "" {
			fmt.Fprintf(out, "release notes: %s\n", rel.URL)
		}
		return nil
	}
	if tag == "" {
		fmt.Fprintf(out, "update available: %s -> %s\n", displayVersion(current), rel.Tag)
	}
	fmt.Fprintf(out, "asset:    %s\n", asset.Name)
	if rel.URL != "" {
		fmt.Fprintf(out, "release notes: %s\n", rel.URL)
	}
	fmt.Fprintln(out, "run `snsync upgrade` to install it")
	return nil
}

// runUpgrade implements `snsync upgrade`.
//
// It replaces the running binary with a release from GitHub.
func runUpgrade(c *update.Client, current, tag string, dryRun bool, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), upgradeTimeout)
	defer cancel()

	rel, err := resolveRelease(ctx, c, tag)
	if err != nil {
		return err
	}
	if tag == "" && !update.IsNewer(current, rel.Tag) {
		fmt.Fprintf(out, "already up to date (%s)\n", rel.Tag)
		return nil
	}
	// Resolve the asset first, so an unsupported platform is reported instead
	// of a plan that cannot be carried out.
	asset, err := update.PlatformAsset(rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	target := c.Target
	if target == "" {
		if target, err = update.SelfPath(); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "current:  %s\n", displayVersion(current))
	fmt.Fprintf(out, "release:  %s\n", rel.Tag)
	if current == "" || current == "dev" {
		fmt.Fprintln(out, "note: this build has no version number (built from source?); installing the release anyway")
	}
	fmt.Fprintf(out, "asset:    %s\n", asset.Name)
	fmt.Fprintf(out, "target:   %s\n", target)
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing was installed")
		return nil
	}

	if _, err := c.Install(ctx, rel); err != nil {
		return err
	}
	fmt.Fprintf(out, "updated to %s\n", rel.Tag)
	fmt.Fprintln(out, "the new version takes effect the next time snsync runs")
	return nil
}

func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return "dev (unversioned build)"
	}
	return v
}
