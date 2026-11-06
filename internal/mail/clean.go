package mail

import (
	"regexp"
	"strings"
)

// Draft text is cleaned in code because a prompt is not a control: these marks
// give machine writing away, whatever the model was told.
var (
	dashBetweenDigits = regexp.MustCompile(`(\d)\s?[–—]\s?(\d)`)
	dashAsPunctuation = regexp.MustCompile(`[ \t]*(?:[–—]|--+)[ \t]*`)
	boldItalic        = regexp.MustCompile(`(\*\*|__)(.+?)(\*\*|__)`)
	heading           = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	starBullet        = regexp.MustCompile(`(?m)^([ \t]*)[*•][ \t]+`)
	mdLink            = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	backtick          = regexp.MustCompile("`+")
)

// CleanDraftText removes em dashes, en dashes, double dashes and markdown.
func CleanDraftText(s string) string {
	s = dashBetweenDigits.ReplaceAllString(s, "$1-$2")
	s = dashAsPunctuation.ReplaceAllStringFunc(s, func(m string) string {
		return " - "
	})
	s = boldItalic.ReplaceAllString(s, "$2")
	s = heading.ReplaceAllString(s, "")
	s = starBullet.ReplaceAllString(s, "$1- ")
	s = mdLink.ReplaceAllString(s, "$1 ($2)")
	s = backtick.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, " - ") // a dash that opened a line
		if strings.HasPrefix(l, " - ") {
			lines[i] = "- " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
