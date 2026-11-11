package soup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeURLAbsolute(t *testing.T) {
	got, err := NormalizeURL("HTTP://Example.COM:80/a/./b/../c/#frag", "")
	require.NoError(t, err)
	assert.Equal(t, "http://example.com/a/c/", got)
}

func TestNormalizeURLHTTPSDefaultPort(t *testing.T) {
	got, err := NormalizeURL("https://Example.com:443/path", "")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/path", got)
}

func TestNormalizeURLRelative(t *testing.T) {
	base := "http://example.com/dir/page.html"
	cases := map[string]string{
		"sub/other.html":  "http://example.com/dir/sub/other.html",
		"/root/x?y=1":     "http://example.com/root/x?y=1",
		"../up.html#frag": "http://example.com/up.html",
		"//cdn.test/a.js": "http://cdn.test/a.js",
	}
	for raw, expected := range cases {
		got, err := NormalizeURL(raw, base)
		require.NoError(t, err, raw)
		assert.Equal(t, expected, got, raw)
	}
}

func TestNormalizeURLDropsNonHTTP(t *testing.T) {
	base := "http://example.com/index.html"
	for _, raw := range []string{
		"mailto:a@b.com",
		"javascript:alert(1)",
		"data:text/plain,hello",
		"tel:+123456",
		"ftp://example.com/file",
	} {
		_, err := NormalizeURL(raw, base)
		assert.Error(t, err, raw)
	}
}

func TestNormalizeURLTrailingSlashPreserved(t *testing.T) {
	got, err := NormalizeURL("http://example.com/a/b/../", "")
	require.NoError(t, err)
	assert.Equal(t, "http://example.com/a/", got)
}

func TestNormalizeURLRequiresBaseForRelative(t *testing.T) {
	_, err := NormalizeURL("relative/page.html", "")
	assert.Error(t, err)
}
