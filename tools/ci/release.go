package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orglane/orglane/tools/ci/internal/release"
)

// runReleaseVersion validates a release tag and writes the step outputs the
// Images workflow's build job needs:
//
//	version     the tag without "v" (the exact image tag and the build's VERSION)
//	prerelease  true or false
func runReleaseVersion(args []string, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("release-version", flag.ContinueOnError)
	tag := fs.String("tag", "", "release tag, for example v0.1.0")
	output := fs.String("github-output", os.Getenv("GITHUB_OUTPUT"), "file to append step outputs (version, prerelease) to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	v, err := release.ParseTag(*tag)
	if err != nil {
		return err
	}
	pre := v.Prerelease != ""
	fmt.Fprintf(stdout, "release %s: version %s, prerelease: %v\n", *tag, v, pre)
	return appendFile(*output, fmt.Sprintf("version=%s\nprerelease=%v\n", v, pre))
}

// runReleaseAliases lists the image's tags in the registry and prints where
// every moving alias (latest, X.Y) must point: one "<alias> <version>" line
// each, also written to the "aliases" step output. The Images workflow runs
// it in its serialized promote job, right before it moves the aliases.
func runReleaseAliases(ctx context.Context, args []string, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("release-aliases", flag.ContinueOnError)
	image := fs.String("image", "", "image repository, for example ghcr.io/orglane/orglane")
	registryURL := fs.String("registry-url", "", "registry base URL (default: https:// and the image's registry host)")
	output := fs.String("github-output", os.Getenv("GITHUB_OUTPUT"), "file to append the aliases step output to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, repository, err := splitImage(*image)
	if err != nil {
		return err
	}
	if *registryURL == "" {
		*registryURL = "https://" + host
	}

	reg := release.Registry{
		URL:      *registryURL,
		Username: os.Getenv("REGISTRY_USERNAME"),
		Password: os.Getenv("REGISTRY_PASSWORD"),
		Client:   &http.Client{Timeout: time.Minute},
	}
	tags, err := reg.ListTags(ctx, repository)
	if err != nil {
		return err
	}
	aliases := release.Aliases(tags)

	var lines strings.Builder
	for _, a := range aliases {
		fmt.Fprintf(&lines, "%s %s\n", a.Tag, a.Version)
	}
	fmt.Fprintf(stdout, "%s: %d tags, %d aliases\n%s", *image, len(tags), len(aliases), lines.String())
	return appendFile(*output, "aliases<<EOF\n"+lines.String()+"EOF\n")
}

// splitImage splits "ghcr.io/orglane/orglane" into the registry host and the
// repository.
func splitImage(image string) (host, repository string, err error) {
	host, repository, ok := strings.Cut(image, "/")
	name := repository[strings.LastIndex(repository, "/")+1:]
	if !ok || host == "" || name == "" || strings.ContainsAny(repository, ":@ \t\n") ||
		strings.ContainsAny(host, " \t\n/") || strings.ToLower(image) != image {
		return "", "", fmt.Errorf("-image %q must be a lower-case registry/repository without a tag or digest", image)
	}
	return host, repository, nil
}
