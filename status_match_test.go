package caddyconsul

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStatusMatcher_Empty(t *testing.T) {
	sm, err := ParseStatusMatcher("")
	require.NoError(t, err)
	assert.Nil(t, sm, "empty spec should return nil matcher")
}

func TestParseStatusMatcher_Whitespace(t *testing.T) {
	sm, err := ParseStatusMatcher("   ")
	require.NoError(t, err)
	assert.Nil(t, sm, "whitespace-only spec should return nil matcher")
}

func TestParseStatusMatcher_ClassWildcards_Lowercase(t *testing.T) {
	sm, err := ParseStatusMatcher("3xx")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(300))
	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(399))
	assert.False(t, sm.Matches(200))
	assert.False(t, sm.Matches(400))
}

func TestParseStatusMatcher_ClassWildcards_Uppercase(t *testing.T) {
	sm, err := ParseStatusMatcher("3XX")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(300))
	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(399))
	assert.False(t, sm.Matches(200))
}

func TestParseStatusMatcher_ClassWildcards_MixedCase(t *testing.T) {
	sm, err := ParseStatusMatcher("3Xx")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(302))
}

func TestParseStatusMatcher_IndividualCodes(t *testing.T) {
	sm, err := ParseStatusMatcher("301,404,502")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(404))
	assert.True(t, sm.Matches(502))
	assert.False(t, sm.Matches(302))
	assert.False(t, sm.Matches(403))
	assert.False(t, sm.Matches(200))
}

func TestParseStatusMatcher_Mixed(t *testing.T) {
	sm, err := ParseStatusMatcher("3xx,502,503")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(300))
	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(399))
	assert.True(t, sm.Matches(502))
	assert.True(t, sm.Matches(503))
	assert.False(t, sm.Matches(200))
	assert.False(t, sm.Matches(400))
	assert.False(t, sm.Matches(501))
}

func TestParseStatusMatcher_WhitespaceInTokens(t *testing.T) {
	sm, err := ParseStatusMatcher(" 3xx , 502 ")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(502))
}

func TestParseStatusMatcher_AllClasses(t *testing.T) {
	sm, err := ParseStatusMatcher("1xx,2xx,3xx,4xx,5xx")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(100))
	assert.True(t, sm.Matches(200))
	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(404))
	assert.True(t, sm.Matches(503))
}

func TestParseStatusMatcher_Overlapping(t *testing.T) {
	// 301 is redundant with 3xx but should still parse successfully
	sm, err := ParseStatusMatcher("3xx,301")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(301))
	assert.True(t, sm.Matches(302))
}

func TestParseStatusMatcher_Invalid_Text(t *testing.T) {
	_, err := ParseStatusMatcher("abc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status code")
}

func TestParseStatusMatcher_Invalid_ClassOutOfRange(t *testing.T) {
	_, err := ParseStatusMatcher("6xx")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status class")
}

func TestParseStatusMatcher_Invalid_ZeroClass(t *testing.T) {
	_, err := ParseStatusMatcher("0xx")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status class")
}

func TestParseStatusMatcher_Invalid_TooSmall(t *testing.T) {
	_, err := ParseStatusMatcher("99")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status code")
}

func TestParseStatusMatcher_Invalid_TooLarge(t *testing.T) {
	_, err := ParseStatusMatcher("600")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status code")
}

func TestStatusMatcher_Matches_NilReceiver(t *testing.T) {
	var sm *StatusMatcher
	assert.False(t, sm.Matches(502), "nil matcher should never match")
}

func TestStatusMatcher_Matches_BoundaryValues(t *testing.T) {
	sm, err := ParseStatusMatcher("5xx")
	require.NoError(t, err)

	assert.False(t, sm.Matches(499))
	assert.True(t, sm.Matches(500))
	assert.True(t, sm.Matches(599))
}

func TestParseStatusMatcher_TrailingComma(t *testing.T) {
	sm, err := ParseStatusMatcher("502,")
	require.NoError(t, err)
	require.NotNil(t, sm)

	assert.True(t, sm.Matches(502))
}
