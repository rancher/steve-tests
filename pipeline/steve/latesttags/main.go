// Command latesttags prints the newest Steve tag for each supported minor
// line, one "minor=tag" pair per line. It reads tag names or ref listings
// on stdin:
//
//	gh api --paginate 'repos/rancher/steve/tags?per_page=100' --jq '.[].name' |
//	  go run ./pipeline/steve/latesttags 0.8 0.9 0.10
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// releaseTag matches vMAJOR.MINOR.PATCH with an optional prerelease suffix.
var releaseTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-[0-9A-Za-z.-]+)?$`)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: latesttags MINOR [MINOR...]  (tag names or ref listings on stdin)")
		os.Exit(2)
	}

	latest, err := LatestTags(os.Stdin, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, minor := range sortedMinors(latest) {
		fmt.Printf("%s=%s\n", minor, latest[minor])
	}
}

// LatestTags returns the newest tag for each requested minor, such as "0.10".
// Every requested minor must have at least one tag.
func LatestTags(r io.Reader, minors []string) (map[string]string, error) {
	wanted := map[string]bool{}
	for _, minor := range minors {
		wanted[strings.TrimSpace(minor)] = true
	}

	latest := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		tag := tagFromLine(scanner.Text())
		match := releaseTag.FindStringSubmatch(tag)
		if match == nil || !semver.IsValid(tag) {
			continue
		}
		minor := match[1] + "." + match[2]
		if !wanted[minor] {
			continue
		}
		if current, ok := latest[minor]; !ok || semver.Compare(tag, current) > 0 {
			latest[minor] = tag
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for minor := range wanted {
		if _, ok := latest[minor]; !ok {
			missing = append(missing, minor)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no matching tags found for: %s", strings.Join(missing, ", "))
	}
	return latest, nil
}

// tagFromLine accepts "<sha>\trefs/tags/<tag>" lines or bare tags.
func tagFromLine(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(fields[len(fields)-1], "refs/tags/")
}

func sortedMinors(latest map[string]string) []string {
	minors := make([]string, 0, len(latest))
	for minor := range latest {
		minors = append(minors, minor)
	}
	sort.Slice(minors, func(i, j int) bool {
		return minorKey(minors[i]) < minorKey(minors[j])
	})
	return minors
}

func minorKey(minor string) int {
	major, rest, _ := strings.Cut(minor, ".")
	ma, _ := strconv.Atoi(major)
	mi, _ := strconv.Atoi(rest)
	return ma*100000 + mi
}
