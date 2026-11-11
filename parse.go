package soup

import (
	"io"
	"strings"

	"golang.org/x/net/html"
)

// parseWithTokenizer builds an html.Node tree straight from the token stream.
// html.Parse caps the open-element stack at 512 nodes and fails on deeper
// markup, so deeply nested documents fall back to this iterative builder,
// which has no recursion and no depth cap. It implements a small subset of the
// HTML5 tree construction algorithm, enough to preserve element nesting,
// attributes and text nodes.
func parseWithTokenizer(r io.Reader) (*html.Node, error) {
	tokenizer := html.NewTokenizer(r)
	doc := &html.Node{Type: html.DocumentNode}
	current := doc
	var stack []*html.Node

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			if err := tokenizer.Err(); err != io.EOF {
				return nil, err
			}
			return doc, nil
		case html.DoctypeToken:
			token := tokenizer.Token()
			doc.AppendChild(&html.Node{
				Type: html.DoctypeNode,
				Data: token.Data,
			})
		case html.CommentToken:
			token := tokenizer.Token()
			current.AppendChild(&html.Node{
				Type: html.CommentNode,
				Data: token.Data,
			})
		case html.TextToken:
			text := tokenizer.Token().Data
			if text == "" {
				continue
			}
			current.AppendChild(&html.Node{
				Type: html.TextNode,
				Data: text,
			})
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			element := &html.Node{
				Type:     html.ElementNode,
				DataAtom: token.DataAtom,
				Data:     token.Data,
				Attr:     make([]html.Attribute, 0, len(token.Attr)),
			}
			for _, attr := range token.Attr {
				element.Attr = append(element.Attr, html.Attribute{
					Namespace: attr.Namespace,
					Key:       attr.Key,
					Val:       attr.Val,
				})
			}
			current.AppendChild(element)
			if tt == html.SelfClosingTagToken || voidElements[token.Data] {
				continue
			}
			stack = append(stack, element)
			current = element
		case html.EndTagToken:
			token := tokenizer.Token()
			if voidElements[token.Data] {
				continue
			}
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].Data == token.Data {
					stack = stack[:i]
					if len(stack) > 0 {
						current = stack[len(stack)-1]
					} else {
						current = doc
					}
					break
				}
			}
		}
	}
}

// voidElements never have closing tags, so they must not be pushed onto the
// open-element stack.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true,
	"embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true,
	"track": true, "wbr": true,
}

// parseDocument parses s with the standard HTML5 parser and falls back to the
// iterative tokenizer builder when the markup exceeds the parser's nesting
// limit. Both paths return a document node.
func parseDocument(s string) (*html.Node, error) {
	doc, err := html.Parse(strings.NewReader(s))
	if err == nil {
		return doc, nil
	}
	return parseWithTokenizer(strings.NewReader(s))
}

// hasRealElementNode reports whether the document contains an element node
// beyond the html/head/body scaffold that html.Parse synthesizes for empty or
// comment-only input. A document whose elements are all scaffold nodes has no
// real content and must be treated as unparseable.
func hasRealElementNode(doc *html.Node) bool {
	if doc == nil {
		return false
	}
	scaffold := map[string]bool{
		"html": true,
		"head": true,
		"body": true,
	}
	stack := []*html.Node{doc}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.Type == html.ElementNode && !scaffold[current.Data] {
			return true
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			stack = append(stack, child)
		}
	}
	return false
}
