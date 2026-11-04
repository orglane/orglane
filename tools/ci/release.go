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

// runReleaseAliases lists the tags of one or more images in their
// registries and prints where every moving alias (latest, X.Y) must point:
// one "<alias> <version>" line each, also written to the "aliases" step
// output. With several -image flags (the core and the agent image, released
// together) the aliases come from the versions published for every image,
// so one plan moves all of them to the same version and a release that
// published only some of its images moves nothing. The Images workflow runs
// it in its serialized promote job, right before it moves the aliases.
func runReleaseAliases(ctx context.Context, args []string, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("release-aliases", flag.ContinueOnError)
	var images imageList
	fs.Var(&images, "image", "image repository, for example ghcr.io/orglane/orglane; repeat it for images released together")
	registryURL := fs.String("registry-url", "", "registry base URL (default: https:// and the image's registry host)")
	output := fs.String("github-output", os.Getenv("GITHUB_OUTPUT"), "file to append the aliases step output to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(images) == 0 {
		images = imageList{""}
	}
	tagSets := make([][]string, 0, len(images))
	for _, image := range images {
		host, repository, err := splitImage(image)
		if err != nil {
			return err
		}
		base := *registryURL
		if base == "" {
			base = "https://" + host
		}
		reg := release.Registry{
			URL:      base,
			Username: os.Getenv("REGISTRY_USERNAME"),
			Password: os.Getenv("REGISTRY_PASSWORD"),
			Client:   &http.Client{Timeout: time.Minute},
		}
		tags, err := reg.ListTags(ctx, repository)
		if err != nil {
			return fmt.Errorf("%s: %w", image, err)
		}
		if len(images) > 1 {
			fmt.Fprintf(stdout, "%s: %d tags\n", image, len(tags))
		}
		tagSets = append(tagSets, tags)
	}
	common := release.Common(tagSets...)
	aliases := release.Aliases(common)

	var lines strings.Builder
	for _, a := range aliases {
		fmt.Fprintf(&lines, "%s %s\n", a.Tag, a.Version)
	}
	if len(images) > 1 {
		fmt.Fprintf(stdout, "%d tags published for every image, %d aliases\n%s", len(common), len(aliases), lines.String())
	} else {
		fmt.Fprintf(stdout, "%s: %d tags, %d aliases\n%s", images[0], len(common), len(aliases), lines.String())
	}
	return appendFile(*output, "aliases<<EOF\n"+lines.String()+"EOF\n")
}

// imageList collects repeated -image flags.
type imageList []string

func (l *imageList) String() string { return strings.Join(*l, ",") }

func (l *imageList) Set(v string) error {
	*l = append(*l, v)
	return nil
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
