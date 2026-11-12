package soup

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/net/html"
)

const attributePresenceHTML = `
<html>
	<body>
		<a href="/one" class="link">one</a>
		<a name="anchor">two</a>
		<a href="/three" id="third" class="link primary">three</a>
		<div data-track="x" id="d1" class="box">multi-attr</div>
	</body>

</html>
`

func TestFindByAttributePresence(t *testing.T) {
	doc := HTMLParse(attributePresenceHTML)

	link := doc.Find("a", "href")
	assert.Nil(t, link.Error)
	assert.Equal(t, "/one", link.Attrs()["href"])

	anchors := doc.FindAll("a", "href")
	assert.Equal(t, 2, len(anchors))
	assert.Equal(t, "/one", anchors[0].Attrs()["href"])
	assert.Equal(t, "/three", anchors[1].Attrs()["href"])

	// Two-argument strict lookup has the same presence semantics.
	strictAnchors := doc.FindAllStrict("a", "href")
	assert.Equal(t, 2, len(strictAnchors))

	// A present tag with a missing attribute is simply not found.
	missing := doc.Find("a", "nope")
	assert.Nil(t, missing.Pointer)
	assert.IsType(t, Error{}, missing.Error)
	assert.Equal(t, ErrElementNotFound, missing.Error.(Error).Type)
}

func TestFindStrictTwoArgAttributePresence(t *testing.T) {
	doc := HTMLParse(attributePresenceHTML)

	found := doc.FindStrict("a", "name")
	assert.Nil(t, found.Error)
	assert.Equal(t, "two", found.Text())

	all := doc.FindAllStrict("div", "data-track")
	assert.Equal(t, 1, len(all))
	assert.Equal(t, "d1", all[0].Attrs()["id"])
}

func TestDefaultTraversalDepthIsUnlimited(t *testing.T) {
	assert.Equal(t, 0, maxTraversalDepth)
	doc := HTMLParse(`<html><body><div><div><div><span>deep</span></div></div></div></body></html>`)
	assert.Nil(t, doc.Find("span").Error)
	assert.Len(t, doc.FindAll("div"), 3)
}

func TestFindAllDeduplicatesElements(t *testing.T) {
	doc := HTMLParse(attributePresenceHTML)

	// The div carries several attributes; the matching node must appear once.
	divs := doc.FindAll("div", "class", "box")
	assert.Equal(t, 1, len(divs))
	assert.Equal(t, "d1", divs[0].Attrs()["id"])

	links := doc.FindAll("a", "class", "link")
	assert.Equal(t, 2, len(links))
	assert.Equal(t, "/one", links[0].Attrs()["href"])
	assert.Equal(t, "/three", links[1].Attrs()["href"])
}

func TestChildrenReturnsElementsOnly(t *testing.T) {
	doc := HTMLParse(`<html><body><div id="parent">
		text node
		<!-- a comment -->
		<p>first</p>
		<span>second</span>
	</div></body></html>`)
	parent := doc.Find("div", "id", "parent")
	children := parent.Children()
	assert.Equal(t, 2, len(children))
	for _, child := range children {
		assert.Equal(t, html.ElementNode, child.Pointer.Type)
	}
	assert.Equal(t, "p", children[0].NodeValue)
	assert.Equal(t, "span", children[1].NodeValue)
}

func TestDeeplyNestedHTMLDoesNotOverflow(t *testing.T) {
	builder := strings.Builder{}
	builder.WriteString("<html><body>")
	const depth = 3000
	for i := 0; i < depth; i++ {
		builder.WriteString("<div class=\"nested\">")
	}
	for i := 0; i < depth; i++ {
		builder.WriteString("</div>")
	}
	builder.WriteString("</body></html>")

	doc := HTMLParse(builder.String())
	assert.Nil(t, doc.Error)

	divs := doc.FindAll("div")
	assert.Equal(t, depth, len(divs))

	first := doc.Find("div", "class", "nested")
	assert.Nil(t, first.Error)
	assert.Equal(t, "div", first.NodeValue)
}

func TestMaxTraversalDepth(t *testing.T) {
	SetMaxTraversalDepth(10)
	t.Cleanup(func() { SetMaxTraversalDepth(0) })

	builder := strings.Builder{}
	builder.WriteString("<html><body>")
	const nestedDivs = 100
	for i := 0; i < nestedDivs; i++ {
		builder.WriteString("<div>")
	}
	builder.WriteString("<span>deep</span>")
	for i := 0; i < nestedDivs; i++ {
		builder.WriteString("</div>")
	}
	builder.WriteString("</body></html>")
	doc := HTMLParse(builder.String())

	found := doc.Find("span")
	assert.NotNil(t, found.Error)
	assert.True(t, errors.Is(found.Error, ErrMaxTraversalDepth))
	assert.Nil(t, found.Pointer)

	// FindAll keeps the partial results and never panics.
	partial := doc.FindAll("div")
	assert.NotEmpty(t, partial)
	for _, node := range partial {
		assert.Equal(t, html.ElementNode, node.Pointer.Type)
	}
}

func TestHTMLParseWithoutElements(t *testing.T) {
	for _, input := range []string{"", "   ", "<!-- only a comment -->", "<!-- one --><!-- two -->"} {
		t.Run(input, func(t *testing.T) {
			root := HTMLParse(input)
			assert.Nil(t, root.Pointer)
			if assert.IsType(t, Error{}, root.Error) {
				assert.Equal(t, ErrUnableToParse, root.Error.(Error).Type)
			}
		})
	}
}
