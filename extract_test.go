package soup

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extractHTML = `
<html>
	<body>
		<h1 class="title">  Headline  </h1>
		<a href="/relative/one" class="link primary">One</a>
		<a href="/relative/one" class="link primary">Duplicate One</a>
		<a href="https://cdn.test/two.js" class="link">Two</a>
		<a name="no-href" class="link">No URL</a>
		<div class="note"><span>Inner</span></div>
	</body>

</html>
`

func TestExtractorAllValueModes(t *testing.T) {
	root := HTMLParse(extractHTML)
	fields := []Field{
		{Name: "title", Tag: "h1", Mode: ValueText, Required: true},
		{Name: "noteHTML", Tag: "div", Attrs: map[string]string{"class": "note"}, Mode: ValueHTML},
		{
			Name:        "links",
			Tag:         "a",
			Attrs:       map[string]string{"class": "link primary"},
			Mode:        ValueAttr,
			Attribute:   "href",
			Multiple:    true,
			AbsoluteURL: true,
		},
	}
	extractor := NewExtractor(fields)
	result, err := extractor.Extract(root, "http://example.com/dir/page.html")
	require.NoError(t, err)

	assert.Equal(t, "Headline", result["title"])
	assert.Contains(t, result["noteHTML"], "<span>Inner</span>")

	links, ok := result["links"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{
		"http://example.com/relative/one", // duplicate href collapsed, order kept
	}, links)
}

func TestExtractorSingleValueAndDedupe(t *testing.T) {
	root := HTMLParse(extractHTML)
	fields := []Field{
		{Name: "firstLink", Tag: "a", Mode: ValueAttr, Attribute: "href", AbsoluteURL: true},
		{Name: "texts", Tag: "a", Mode: ValueText, Multiple: true},
	}
	extractor := NewExtractor(fields)
	result, err := extractor.Extract(root, "http://example.com/")
	require.NoError(t, err)
	assert.Equal(t, "http://example.com/relative/one", result["firstLink"])
	assert.Equal(t, []string{"One", "Duplicate One", "Two", "No URL"}, result["texts"])
}

func TestExtractorRequiredFieldMissing(t *testing.T) {
	root := HTMLParse(extractHTML)
	fields := []Field{
		{Name: "missing", Tag: "article", Mode: ValueText, Required: true},
	}
	extractor := NewExtractor(fields)
	_, err := extractor.Extract(root, "http://example.com/page")
	require.Error(t, err)
	var missingErr *MissingFieldError
	require.True(t, errors.As(err, &missingErr))
	assert.Equal(t, "missing", missingErr.Field)
	assert.Contains(t, err.Error(), "missing")
	assert.Contains(t, err.Error(), "http://example.com/page")
}

func TestExtractorOptionalFieldOmitted(t *testing.T) {
	root := HTMLParse(extractHTML)
	fields := []Field{
		{Name: "maybe", Tag: "article", Mode: ValueText},
		{Name: "title", Tag: "h1", Mode: ValueText},
	}
	extractor := NewExtractor(fields)
	result, err := extractor.Extract(root, "http://example.com/")
	require.NoError(t, err)
	_, present := result["maybe"]
	assert.False(t, present)
	assert.Equal(t, "Headline", result["title"])
}

func TestExtractorAttributeConditionsAllMustMatch(t *testing.T) {
	root := HTMLParse(extractHTML)
	fields := []Field{
		{
			Name:     "primaryLinks",
			Tag:      "a",
			Attrs:    map[string]string{"class": "link primary"},
			Mode:     ValueText,
			Multiple: true,
		},
	}
	extractor := NewExtractor(fields)
	result, err := extractor.Extract(root, "http://example.com/")
	require.NoError(t, err)
	assert.Equal(t, []string{"One", "Duplicate One"}, result["primaryLinks"])
}
