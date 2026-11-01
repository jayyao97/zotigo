// Package update owns installed component updates, independently of UI lifetimes.
package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var releaseVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-dev\.(0|[1-9][0-9]*))?$`)
var commitHash = regexp.MustCompile(`^[a-f0-9]{40}$`)

type version struct {
	parts [4]uint64
	dev   bool
}

func parseVersion(value string) (version, error) {
	m := releaseVersion.FindStringSubmatch(value)
	if m == nil {
		return version{}, fmt.Errorf("unsupported release version %q", value)
	}
	v := version{dev: m[4] != ""}
	for i := 0; i < 4; i++ {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return version{}, err
		}
		v.parts[i] = n
	}
	return v, nil
}

func compare(a, b version) int {
	for i := 0; i < 3; i++ {
		if a.parts[i] < b.parts[i] {
			return -1
		}
		if a.parts[i] > b.parts[i] {
			return 1
		}
	}
	if a.dev != b.dev {
		if a.dev {
			return -1
		}
		return 1
	}
	if a.parts[3] < b.parts[3] {
		return -1
	}
	if a.parts[3] > b.parts[3] {
		return 1
	}
	return 0
}

func validChannel(channel string) bool { return channel == "stable" || channel == "development" }

// SelectRelease uses numeric prerelease ordering; the development channel also
// includes stable releases. Unpublished commits never enter this ordering.
func SelectRelease(tags, channel string) (string, string, error) {
	if !validChannel(channel) {
		return "", "", fmt.Errorf("invalid update channel")
	}
	var best version
	tag, commit := "", ""
	peeled := make(map[string]string)
	for _, line := range strings.Split(tags, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && commitHash.MatchString(fields[0]) && strings.HasSuffix(fields[1], "^{}") {
			peeled[strings.TrimSuffix(fields[1], "^{}")] = fields[0]
		}
	}
	for _, line := range strings.Split(tags, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/v") || !commitHash.MatchString(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "refs/tags/")
		v, err := parseVersion(name)
		if err != nil || (channel == "stable" && v.dev) {
			continue
		}
		if tag == "" || compare(v, best) > 0 {
			best, tag, commit = v, name, fields[0]
			if value := peeled[fields[1]]; value != "" {
				commit = value
			}
		}
	}
	if tag == "" {
		return "", "", fmt.Errorf("no release in the selected channel")
	}
	return tag, commit, nil
}
