package mm

import "strings"

// MaxPostRunes is below Mattermost's 16383-character post limit.
const MaxPostRunes = 15000

// Split cuts text into chunks of at most max runes, preferring to break at
// a blank line, then a newline, then a space, within the last fifth of a chunk.
func Split(text string, max int) []string {
	r := []rune(strings.TrimSpace(text))
	var out []string
	for len(r) > max {
		cut := max
		window := string(r[max-max/5 : max])
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i >= 0 {
				cut = max - max/5 + len([]rune(window[:i])) + len([]rune(sep))
				break
			}
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		r = []rune(strings.TrimSpace(string(r[cut:])))
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}
