package soup

import (
	"fmt"

	"golang.org/x/net/html"
)

// traversalFrame pairs a node with its depth relative to the traversal root.
type traversalFrame struct {
	node  *html.Node
	depth int
}

// nodeMatches reports whether node satisfies the tag/attribute arguments.
// Supported forms are:
//
//	args = [tag]
//	args = [tag, attribute]                 (attribute must be present)
//	args = [tag, attribute, value]
func nodeMatches(node *html.Node, args []string, strict bool) bool {
	if node.Type != html.ElementNode || !matchElementName(node, args[0]) {
		return false
	}
	switch len(args) {
	case 1:
		return true
	case 2:
		for _, attr := range node.Attr {
			if attr.Key == args[1] {
				return true
			}
		}
		return false
	case 3:
		for _, attr := range node.Attr {
			if strict && attributeAndValueEquals(attr, args[1], args[2]) {
				return true
			}
			if !strict && attributeContainsValue(attr, args[1], args[2]) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// pushChildren appends the children of node to the stack while keeping
// document order (the first child must be popped first).
func pushChildren(stack []traversalFrame, node *html.Node, depth int) []traversalFrame {
	var children []*html.Node
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		children = append(children, child)
	}
	for i := len(children) - 1; i >= 0; i-- {
		stack = append(stack, traversalFrame{node: children[i], depth: depth})
	}
	return stack
}

// depthExceeded reports whether the traversal depth limit is reached.
func depthExceeded(depth int) bool {
	return maxTraversalDepth > 0 && depth > maxTraversalDepth
}

// findOnce performs an iterative depth-first search for the first matching
// element. The traversal root itself is never matched, mirroring the original
// recursive behavior. ErrMaxTraversalDepth is returned when the configured
// depth limit is reached before the search completes.
func findOnce(root *html.Node, args []string, strict bool) (*html.Node, bool, error) {
	if root == nil {
		return nil, false, nil
	}
	stack := make([]traversalFrame, 0, 32)
	stack = pushChildren(stack, root, 1)
	for len(stack) > 0 {
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if depthExceeded(frame.depth) {
			return nil, false, fmt.Errorf("find traversal from `%s`: %w", root.Data, ErrMaxTraversalDepth)
		}
		if nodeMatches(frame.node, args, strict) {
			return frame.node, true, nil
		}
		stack = pushChildren(stack, frame.node, frame.depth+1)
	}
	return nil, false, nil
}

// findAllofem performs an iterative depth-first search for every matching
// element in document order. Every node is visited at most once, so an element
// with several matching attributes is never returned more than once.
// ErrMaxTraversalDepth is returned alongside the partial results when the
// configured depth limit is reached.
func findAllofem(root *html.Node, args []string, strict bool) ([]*html.Node, error) {
	if root == nil {
		return nil, nil
	}
	matches := make([]*html.Node, 0, 10)
	stack := make([]traversalFrame, 0, 32)
	stack = pushChildren(stack, root, 1)
	for len(stack) > 0 {
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if depthExceeded(frame.depth) {
			return matches, fmt.Errorf("find-all traversal from `%s`: %w", root.Data, ErrMaxTraversalDepth)
		}
		if nodeMatches(frame.node, args, strict) {
			matches = append(matches, frame.node)
		}
		stack = pushChildren(stack, frame.node, frame.depth+1)
	}
	return matches, nil
}
