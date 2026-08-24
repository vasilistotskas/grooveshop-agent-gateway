package chat

import "strings"

// Shopper-facing message keys. Everything the /chat surface says outside
// the model's own output goes through messageFor so it follows the
// tenant's language.
const (
	msgRefusal        = "refusal"
	msgChatDisabled   = "chat_disabled"
	msgTurnFailed     = "turn_failed"
	msgRateLimited    = "rate_limited"
	msgTurnIncomplete = "turn_incomplete"
)

// messages maps language → key → text. Deliberately not an i18n library:
// four strings in two languages. New tenant languages add one entry here.
var messages = map[string]map[string]string{
	"el": {
		msgRefusal: "Λυπάμαι, δεν μπορώ να βοηθήσω με αυτό το αίτημα. " +
			"Μπορώ όμως να σε βοηθήσω να βρεις προϊόντα, να δεις " +
			"διαθεσιμότητα και να ολοκληρώσεις την παραγγελία σου!",
		msgChatDisabled: "Ο βοηθός δεν είναι διαθέσιμος για αυτό το " +
			"κατάστημα.",
		msgTurnFailed: "Η συνομιλία διακόπηκε προσωρινά — δοκίμασε ξανά.",
		msgRateLimited: "Ο βοηθός δέχεται πολλές ερωτήσεις αυτή τη " +
			"στιγμή — δοκίμασε ξανά σε λίγο.",
		msgTurnIncomplete: "Χρειάστηκα περισσότερα βήματα από όσα " +
			"επιτρέπονται για αυτό. Μπορείς να το διατυπώσεις πιο απλά ή " +
			"να ρωτήσεις ξανά;",
	},
	"en": {
		msgRefusal: "Sorry, I can't help with that request. I can help " +
			"you find products, check availability and complete your " +
			"order, though!",
		msgChatDisabled: "The assistant is not available for this store.",
		msgTurnFailed: "The conversation was interrupted for a moment — " +
			"please try again.",
		msgRateLimited: "The assistant is handling a lot of questions " +
			"right now — please try again in a little while.",
		msgTurnIncomplete: "That took more steps than I'm allowed in one " +
			"go. Could you rephrase or ask again?",
	},
}

// messageFor returns the shopper-facing text for a tenant locale.
// Locales normalize by lowercasing and stripping the region ("el-GR" and
// "el_GR" → "el"); languages without a table fall back to English.
func messageFor(locale, key string) string {
	lang := strings.ToLower(locale)
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	if text, ok := messages[lang][key]; ok {
		return text
	}
	return messages["en"][key]
}
