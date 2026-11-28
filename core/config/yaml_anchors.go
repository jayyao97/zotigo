package config

import (
	"errors"
	"regexp"

	"gopkg.in/yaml.v3"
)

var generatedAnchor = regexp.MustCompile("^id[0-9]+$")

// Inline machine-generated YAML anchors before editing. Named anchors belong
// to the author. Keep a generated wrapper too if copying it would redefine a
// nested named anchor. Aliases point backwards in a valid YAML document.
func expandGeneratedAnchors(doc *yaml.Node) error {
	budget := 100000
	expandable := map[*yaml.Node]bool{}
	var copyTree func(*yaml.Node) (*yaml.Node, error)
	copyTree = func(n *yaml.Node) (*yaml.Node, error) {
		budget--
		if budget < 0 {
			return nil, errors.New("expanded configuration is too large")
		}
		clone := *n
		clone.Content = nil
		for _, child := range n.Content {
			c, err := copyTree(child)
			if err != nil {
				return nil, err
			}
			clone.Content = append(clone.Content, c)
		}
		return &clone, nil
	}
	var visit func(*yaml.Node) (bool, error)
	visit = func(n *yaml.Node) (bool, error) {
		if n.Kind == yaml.AliasNode && expandable[n.Alias] {
			c, err := copyTree(n.Alias)
			if err != nil {
				return false, err
			}
			if n.LineComment != "" {
				c.LineComment = n.LineComment
			}
			*n = *c
			return false, nil
		}
		nested := false
		for _, child := range n.Content {
			named, err := visit(child)
			if err != nil {
				return false, err
			}
			nested = nested || named
		}
		if !nested && generatedAnchor.MatchString(n.Anchor) {
			expandable[n] = true
			n.Anchor = ""
		}
		return nested || n.Anchor != "", nil
	}
	_, err := visit(doc)
	return err
}
