// Command rancherminors prints the Steve minor lines used by the newest
// Rancher release branches and main, separated by spaces.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	steveModule  = "github.com/rancher/steve"
	pageSize     = 100
	maxPages     = 100
	maxBodyBytes = 4 << 20
)

var (
	releaseBranch = regexp.MustCompile(`^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitSHA     = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	fullVersion   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+].*)?$`)
)

type branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type dependency struct {
	Branch  string
	Version string
}

type githubClient struct {
	client *http.Client
	token  string
}

func main() {
	releaseLines := flag.Int("release-lines", 5, "number of newest Rancher release branches to include, plus main")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: rancherminors [-release-lines N]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gh := githubClient{
		client: &http.Client{Timeout: 20 * time.Second},
		token:  os.Getenv("GH_TOKEN"),
	}
	if err := run(ctx, gh, *releaseLines, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, gh githubClient, releaseLines int, stdout, stderr io.Writer) error {
	if releaseLines < 1 {
		return fmt.Errorf("release-lines must be positive")
	}
	branches, err := gh.branches(ctx)
	if err != nil {
		return err
	}
	selected, err := selectBranches(branches, releaseLines)
	if err != nil {
		return err
	}

	var dependencies []dependency
	for _, selectedBranch := range selected {
		// Fetch the listed commit, so moving branch tips cannot change this
		// branch's source between discovery and dependency resolution.
		url := "https://raw.githubusercontent.com/rancher/rancher/" + selectedBranch.Commit.SHA + "/go.mod"
		body, err := gh.get(ctx, url)
		if err != nil {
			return fmt.Errorf("read Rancher %s go.mod: %w", selectedBranch.Name, err)
		}
		version, err := steveVersion(body)
		if err != nil {
			return fmt.Errorf("Rancher %s: %w", selectedBranch.Name, err)
		}
		dependencies = append(dependencies, dependency{Branch: selectedBranch.Name, Version: version})
	}

	// Do not publish a partial minor list when any branch fails to resolve.
	for _, dep := range dependencies {
		if _, err := fmt.Fprintf(stderr, "Rancher %s -> Steve %s\n", dep.Branch, dep.Version); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(stdout, strings.Join(steveMinors(dependencies), " "))
	return err
}

func (gh githubClient) branches(ctx context.Context) ([]branch, error) {
	var branches []branch
	for page := 1; page <= maxPages; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/rancher/rancher/branches?per_page=%d&page=%d", pageSize, page)
		body, err := gh.get(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("list Rancher branches (page %d): %w", page, err)
		}
		var batch []branch
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("decode Rancher branches (page %d): %w", page, err)
		}
		branches = append(branches, batch...)
		if len(batch) < pageSize {
			return branches, nil
		}
	}
	return nil, fmt.Errorf("Rancher branch listing exceeded %d pages", maxPages)
}

func (gh githubClient) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "steve-tests-rancherminors")
	if req.URL.Host == "api.github.com" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if gh.token != "" {
			req.Header.Set("Authorization", "Bearer "+gh.token)
		}
	}
	resp, err := gh.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("response from %s exceeds %d bytes", req.URL.Host, maxBodyBytes)
	}
	return body, nil
}

func selectBranches(branches []branch, count int) ([]branch, error) {
	if count < 1 {
		return nil, fmt.Errorf("release-lines must be positive")
	}
	var releases []branch
	var mainBranch *branch
	seen := map[string]bool{}
	for _, candidate := range branches {
		if seen[candidate.Name] {
			continue
		}
		seen[candidate.Name] = true
		if candidate.Name == "main" {
			mainBranch = &candidate
		} else if releaseBranch.MatchString(candidate.Name) {
			releases = append(releases, candidate)
		}
	}
	if mainBranch == nil {
		return nil, fmt.Errorf("Rancher main branch was not found")
	}
	if len(releases) < count {
		return nil, fmt.Errorf("found %d Rancher release branches, need %d", len(releases), count)
	}
	sort.Slice(releases, func(i, j int) bool {
		a := strings.TrimPrefix(releases[i].Name, "release/") + ".0"
		b := strings.TrimPrefix(releases[j].Name, "release/") + ".0"
		return semver.Compare(a, b) > 0
	})
	selected := append(releases[:count:count], *mainBranch)
	for _, selectedBranch := range selected {
		if !commitSHA.MatchString(selectedBranch.Commit.SHA) {
			return nil, fmt.Errorf("Rancher %s has an invalid commit SHA", selectedBranch.Name)
		}
	}
	return selected, nil
}

func steveVersion(source []byte) (string, error) {
	// Preserve the written version so an incomplete dependency such as v0.9
	// cannot be silently canonicalized to v0.9.0 before validation.
	file, err := modfile.Parse("go.mod", source, func(_, version string) (string, error) {
		return version, nil
	})
	if err != nil {
		return "", fmt.Errorf("parse go.mod: %w", err)
	}
	version := ""
	for _, require := range file.Require {
		if require.Mod.Path == steveModule {
			version = require.Mod.Version
			break
		}
	}
	if version == "" {
		return "", fmt.Errorf("go.mod does not require %s", steveModule)
	}

	var replacement, exactReplacement *modfile.Replace
	for _, replace := range file.Replace {
		if replace.Old.Path != steveModule {
			continue
		}
		if replace.Old.Version == "" {
			replacement = replace
		} else if replace.Old.Version == version {
			exactReplacement = replace
		}
	}
	if exactReplacement != nil {
		replacement = exactReplacement
	}
	if replacement != nil {
		if replacement.New.Version == "" {
			return "", fmt.Errorf("Steve uses local replacement %q; cannot resolve rancher/steve tags", replacement.New.Path)
		}
		if replacement.New.Path != steveModule {
			return "", fmt.Errorf("Steve uses external replacement %q; cannot resolve rancher/steve tags", replacement.New.Path)
		}
		version = replacement.New.Version
	}
	if !fullVersion.MatchString(version) || !semver.IsValid(version) {
		return "", fmt.Errorf("Steve dependency %q is not a full semantic version", version)
	}
	return version, nil
}

func steveMinors(dependencies []dependency) []string {
	seen := map[string]bool{}
	var minors []string
	for _, dep := range dependencies {
		minor := strings.TrimPrefix(semver.MajorMinor(dep.Version), "v")
		if !seen[minor] {
			seen[minor] = true
			minors = append(minors, minor)
		}
	}
	sort.Slice(minors, func(i, j int) bool {
		return semver.Compare("v"+minors[i]+".0", "v"+minors[j]+".0") < 0
	})
	return minors
}
