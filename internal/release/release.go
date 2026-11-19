// Package release classifies a workflow run's ref into a release line,
// version and change type.
//
// Conventions handled:
//
//	tag     v1.20.0          → line v1.20, version v1.20.0, release
//	branch  release/v1.20.0  → line v1.20, version "",      hotfix-merge
//	tag     v1.20.1          → line v1.20, version v1.20.1, hotfix-tag
//
// For tag pushes GitHub puts the tag name in workflow_run.head_branch, so the
// classifier tells tags from branches by the branch prefix.
package release

import (
	"fmt"
	"regexp"
	"strings"
)

type ChangeType string

const (
	Release     ChangeType = "release"
	HotfixMerge ChangeType = "hotfix-merge"
	HotfixTag   ChangeType = "hotfix-tag"
)

// Info describes where a run sits in the release model.
type Info struct {
	Line    string     // e.g. "v1.20"
	Version string     // e.g. "v1.20.1"; empty for hotfix-merge runs
	Type    ChangeType // release | hotfix-merge | hotfix-tag
}

const (
	DefaultTagPattern   = `^v(\d+)\.(\d+)\.(\d+)$`
	DefaultBranchPrefix = "release/"
)

// Classifier maps head_branch values to release Info.
type Classifier struct {
	tag          *regexp.Regexp
	branchPrefix string
}

// New builds a Classifier. tagPattern must have exactly three capture groups:
// major, minor, patch.
func New(tagPattern, branchPrefix string) (*Classifier, error) {
	re, err := regexp.Compile(tagPattern)
	if err != nil {
		return nil, fmt.Errorf("release: bad tag pattern: %w", err)
	}
	if re.NumSubexp() != 3 {
		return nil, fmt.Errorf("release: tag pattern %q needs 3 capture groups (major, minor, patch), has %d", tagPattern, re.NumSubexp())
	}
	if branchPrefix == "" {
		return nil, fmt.Errorf("release: branch prefix must not be empty")
	}
	return &Classifier{tag: re, branchPrefix: branchPrefix}, nil
}

// MustDefault returns a Classifier with the default conventions.
func MustDefault() *Classifier {
	c, err := New(DefaultTagPattern, DefaultBranchPrefix)
	if err != nil {
		panic(err)
	}
	return c
}

// Classify returns the release Info for a head_branch value, and false when
// the ref is not part of the release flow (e.g. main, feature branches).
func (c *Classifier) Classify(headBranch string) (Info, bool) {
	if rest, ok := strings.CutPrefix(headBranch, c.branchPrefix); ok {
		m := c.tag.FindStringSubmatch(rest)
		if m == nil {
			return Info{}, false
		}
		return Info{Line: line(m), Type: HotfixMerge}, true
	}

	m := c.tag.FindStringSubmatch(headBranch)
	if m == nil {
		return Info{}, false
	}
	t := HotfixTag
	if strings.TrimLeft(m[3], "0") == "" { // patch == 0
		t = Release
	}
	return Info{Line: line(m), Version: headBranch, Type: t}, true
}

func line(m []string) string {
	return "v" + m[1] + "." + m[2]
}
