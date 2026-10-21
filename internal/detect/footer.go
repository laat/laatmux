package detect

import (
	"regexp"
	"strconv"
	"strings"
)

// What Claude Code says under the prompt box, read for the record: the
// footer line counts its background shells and its monitors, and the
// agents panel under the footer lists its subagents.

// shellsRe is Claude Code's count of its background shells, an item of
// its footer line: `⏵⏵ auto mode on · 2 shells · ← for agents`, or
// `1 shell`. The item is bounded by the footer's separators or the
// line's ends, so a prompt that mentions shells does not count.
var shellsRe = regexp.MustCompile(`(?:^|·)\s*([1-9]\d*) shells?\s*(?:·|$)`)

// monitorsRe is its count of the monitors it runs, the Monitor tool's
// background watches whose events start a turn of their own, an item
// of the same line: `⏵⏵ bypass permissions on · PR #460 · 1 monitor`.
// The turn's closing line above the prompt box, `· 1 monitor still
// running`, is not the footer's.
var monitorsRe = regexp.MustCompile(`(?:^|·)\s*([1-9]\d*) monitors?\s*(?:·|$)`)

// The agents panel: its header `⏺ main`, a line per subagent, `◯ name
// what it does  7m 6s · ↓ 235.5k tokens` while it runs and `◯ name
// what it does  idle` after, the cursor's row with `❯` before the
// mark, `↓ 1 more` for the lines it folds away, and `◯ 4 idle agents`
// for the finished ones it folds into one row.
var (
	panelHeadRe = regexp.MustCompile(`^\s*(?:❯\s*)?⏺ main\s*$`)
	panelRowRe  = regexp.MustCompile(`^\s*(?:❯\s*)?◯\s+\S`)
	panelIdleRe = regexp.MustCompile(`^\s*(?:❯\s*)?◯ [1-9]\d* idle agents?\s*$`)
	panelMoreRe = regexp.MustCompile(`^\s*↓ ([1-9]\d*) more\s*$`)
)

// footerLines are the lines under the prompt box: after the last
// horizontal rule, the bottom three non-empty lines on a screen with
// none, the prompt line skipped. Only Claude Code's screen is read.
func footerLines(in Input) []string {
	if in.Agent != "claude" {
		return nil
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
	var out []string
	for _, line := range splitLines(region) {
		if strings.HasPrefix(strings.TrimSpace(line), "❯ ") || strings.TrimSpace(line) == "❯" {
			// The prompt line, when the box has no rule under it;
			// the panel's cursor row has a mark after the ❯.
			continue
		}
		out = append(out, line)
	}
	return out
}

// Shells is how many background shells the agent's screen says it is
// running: Claude Code counts them in the footer. 0 for none, and for
// an agent that does not count them.
func Shells(in Input) int { return footerCount(in, shellsRe) }

// Monitors is how many monitors the agent's screen says it is running:
// Claude Code counts them in the footer too. 0 for none, and for an
// agent that does not count them.
func Monitors(in Input) int { return footerCount(in, monitorsRe) }

// footerCount is the count the footer item re matches, 0 for none.
func footerCount(in Input, re *regexp.Regexp) int {
	for _, line := range footerLines(in) {
		if m := re.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// Subagents is how many subagents the agent's screen says are running:
// the rows of Claude Code's agents panel that do not read idle, the
// folded ones counted as running since the panel cannot say. The panel
// is read from the bottom of the screen, where it is drawn under the
// footer and under a dialog alike, so a screen without a rule under
// the prompt box still has it. 0 for none, and for an agent without
// the panel.
func Subagents(in Input) int {
	if in.Agent != "claude" {
		return 0
	}
	screen := strings.Join(in.Screen, "\n")
	lines := splitLines(screen)
	n, inPanel := 0, false
	for _, line := range splitLines(bottomNonEmptyLines(screen, lines, 24)) {
		switch {
		case panelHeadRe.MatchString(line):
			n, inPanel = 0, true
		case !inPanel:
		case panelIdleRe.MatchString(line):
		case panelRowRe.MatchString(line):
			if !strings.HasSuffix(strings.TrimRight(line, " "), " idle") {
				n++
			}
		case panelMoreRe.MatchString(line):
			m, _ := strconv.Atoi(panelMoreRe.FindStringSubmatch(line)[1])
			n += m
		}
	}
	return n
}
