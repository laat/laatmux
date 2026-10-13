package detect

import (
	"regexp"
	"strconv"
	"strings"
)

// shellsRe is Claude Code's count of its background shells, an item of
// its footer line: `⏵⏵ auto mode on · 2 shells · ← for agents`, or
// `1 shell`. The item is bounded by the footer's separators or the
// line's ends, so a prompt that mentions shells does not count.
var shellsRe = regexp.MustCompile(`(?:^|·)\s*([1-9]\d*) shells?\s*(?:·|$)`)

// Shells is how many background shells the agent's screen says it is
// running: Claude Code counts them in the footer under the prompt box,
// so the lines after the last horizontal rule are read, the bottom
// three non-empty lines when the screen has no rule. 0 for none, and
// for an agent that does not count them.
func Shells(in Input) int {
	if in.Agent != "claude" {
		return 0
	}
	screen := strings.Join(in.Screen, "\n")
	lines := splitLines(screen)
	region := bottomNonEmptyLines(screen, lines, 3)
	for _, l := range lines {
		if isHorizontalRule(l) {
			region = afterLastHorizontalRule(screen, lines)
			break
		}
	}
	for _, line := range splitLines(region) {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			// The prompt line, when the box has no rule under it.
			continue
		}
		if m := shellsRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}
