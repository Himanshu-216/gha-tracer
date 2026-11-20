// Package tags decides whether a workflow run was triggered by a release tag.
//
// For tag pushes GitHub puts the tag name in workflow_run.head_branch, so a
// run is a tag run when its event is "push" and head_branch matches the
// pattern (default: v1.20.0, v1.20.1, ...).
package tags

import (
	"fmt"
	"regexp"
)

const DefaultPattern = `^v\d+\.\d+\.\d+$`

type Matcher struct{ re *regexp.Regexp }

func New(pattern string) (*Matcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("tags: bad pattern %q: %w", pattern, err)
	}
	return &Matcher{re: re}, nil
}

func MustDefault() *Matcher {
	m, err := New(DefaultPattern)
	if err != nil {
		panic(err)
	}
	return m
}

func (m *Matcher) Match(ref string) bool { return ref != "" && m.re.MatchString(ref) }
