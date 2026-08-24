package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMessageFor(t *testing.T) {
	cases := []struct {
		name   string
		locale string
		key    string
		want   string
	}{
		{
			name: "greek locale gets greek", locale: "el", key: msgRefusal,
			want: messages["el"][msgRefusal],
		},
		{
			name: "region strips with dash", locale: "el-GR",
			key: msgChatDisabled, want: messages["el"][msgChatDisabled],
		},
		{
			name: "region strips with underscore", locale: "el_GR",
			key: msgTurnFailed, want: messages["el"][msgTurnFailed],
		},
		{
			name: "uppercase locale normalizes", locale: "EL",
			key: msgRateLimited, want: messages["el"][msgRateLimited],
		},
		{
			name: "english locale gets english", locale: "en-US",
			key: msgRefusal, want: messages["en"][msgRefusal],
		},
		{
			name: "unknown language falls back to english", locale: "de",
			key: msgChatDisabled, want: messages["en"][msgChatDisabled],
		},
		{
			name: "empty locale falls back to english", locale: "",
			key: msgTurnFailed, want: messages["en"][msgTurnFailed],
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, messageFor(tc.locale, tc.key))
		})
	}
}

// Every key must exist in every language table — a missing entry would
// silently serve an empty string to shoppers.
func TestMessageTablesComplete(t *testing.T) {
	keys := []string{
		msgRefusal, msgChatDisabled, msgTurnFailed, msgRateLimited,
		msgTurnIncomplete,
	}
	for lang, table := range messages {
		for _, key := range keys {
			assert.NotEmpty(t, table[key], "%s missing %s", lang, key)
		}
	}
}
