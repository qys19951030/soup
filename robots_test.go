package soup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRobotsDisallowAndAllowLongestMatch(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /private\n" +
		"Allow: /private/public\n" +
		"Disallow: /admin\n"
	data := parseRobots(contents)

	assert.False(t, data.allowed("any-agent", "/private/secret"))
	assert.True(t, data.allowed("any-agent", "/private/public/page"))
	assert.False(t, data.allowed("any-agent", "/admin"))
	assert.True(t, data.allowed("any-agent", "/open"))
}

func TestRobotsSpecificAgentBeatsWildcard(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /\n" +
		"User-agent: soup-bot\n" +
		"Allow: /\n" +
		"Disallow: /secret\n"
	data := parseRobots(contents)

	assert.True(t, data.allowed("soup-bot", "/home"))
	assert.False(t, data.allowed("soup-bot", "/secret"))
	assert.False(t, data.allowed("other-bot", "/home"))
}

func TestRobotsEqualLengthAllowWins(t *testing.T) {
	contents := "User-agent: *\n" +
		"Disallow: /same\n" +
		"Allow: /same\n"
	data := parseRobots(contents)
	assert.True(t, data.allowed("bot", "/same"))
}

func TestRobotsCommentsAndEmptyDisallow(t *testing.T) {
	contents := "# a comment\n" +
		"User-agent: *  # trailing comment\n" +
		"Disallow:\n" +
		"Disallow: /x # inline\n"
	data := parseRobots(contents)
	assert.True(t, data.allowed("bot", "/anything"))
	assert.False(t, data.allowed("bot", "/x/y"))
}

func TestRobotsMultipleAgentsShareGroup(t *testing.T) {
	contents := "User-agent: bot-a\n" +
		"User-agent: bot-b\n" +
		"Disallow: /shared\n"
	data := parseRobots(contents)
	assert.False(t, data.allowed("bot-a", "/shared"))
	assert.False(t, data.allowed("bot-b", "/shared"))
	assert.True(t, data.allowed("bot-c", "/shared"))
}

func TestRobotsEmptyFileAllowsEverything(t *testing.T) {
	data := parseRobots("")
	assert.True(t, data.allowed("bot", "/anything"))
}
