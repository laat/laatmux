package detect

import (
	"strconv"
	"strings"
	"unicode"
)

// Region selectors, ported from herdr src/detect/manifest.rs. Every function
// works on the joined screen text (lines joined with "\n", no trailing newline)
// and returns a byte slice of it, exactly as the Rust code slices `content`.
//
// Supported selectors: osc_title, osc_progress (always empty; Input carries no
// progress data), whole_recent, bottom_lines(n), bottom_non_empty_lines(n),
// top_non_empty_lines(n), prompt_box_body, above_prompt_box,
// last_non_empty_above_prompt_box, after_last_horizontal_rule,
// after_last_prompt_marker, before_current_prompt_marker,
// whole_recent_without_current_prompt_marker.
//
// Not ported (unused by the Claude and Codex manifests):
// current_prompt_block_marker, after_current_prompt_block_marker.

// knownRegion reports whether spec is a selector this port implements.
// osc_progress counts as known: the selector is understood, laatmux simply
// never has progress data for it, so rules using it stay quietly unmatched
// (regionText reports it as unsupported for Explain).
func knownRegion(spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "osc_progress" {
		return true
	}
	_, ok := regionText(Input{}, "", spec)
	return ok
}

// regionText returns the text a rule's region selector refers to. ok is false
// when the selector is not supported by this port (or, for osc_progress, when
// the data is never available), in which case the rule can never match.
func regionText(in Input, screen string, spec string) (text string, ok bool) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "osc_title":
		return in.Title, true
	case "osc_progress":
		return "", false
	}

	lines := splitLines(screen)
	switch spec {
	case "whole_recent":
		return screen, true
	case "after_last_prompt_marker":
		return afterLastPromptMarker(screen, lines), true
	case "before_current_prompt_marker":
		return beforeCurrentPromptMarker(screen, lines), true
	case "whole_recent_without_current_prompt_marker":
		if _, found := currentCodexPromptIndex(lines); found {
			return "", true
		}
		return screen, true
	case "prompt_box_body":
		return promptBoxBody(screen, lines), true
	case "above_prompt_box":
		return abovePromptBox(screen, lines), true
	case "last_non_empty_above_prompt_box":
		return lastNonEmptyLine(abovePromptBox(screen, lines)), true
	case "after_last_horizontal_rule":
		return afterLastHorizontalRule(screen, lines), true
	}
	if n, ok := regionCount(spec, "bottom_lines"); ok {
		return bottomLines(screen, lines, n), true
	}
	if n, ok := regionCount(spec, "bottom_non_empty_lines"); ok {
		return bottomNonEmptyLines(screen, lines, n), true
	}
	if n, ok := regionCount(spec, "top_non_empty_lines"); ok && n > 0 {
		return topNonEmptyLines(screen, lines, n), true
	}
	return "", false
}

// regionCount parses "name(123)".
func regionCount(spec, name string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, name+"(")
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutSuffix(rest, ")")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// splitLines mirrors Rust's str::lines() on text without a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lineStartOffset is the byte offset where line `index` starts, capped at
// len(content) (index == len(lines) addresses the end of the text).
func lineStartOffset(content string, lines []string, index int) int {
	if index > len(lines) {
		index = len(lines)
	}
	off := 0
	for _, l := range lines[:index] {
		off += len(l) + 1
	}
	if off > len(content) {
		off = len(content)
	}
	return off
}

func sliceFromLine(content string, lines []string, index int) string {
	return content[lineStartOffset(content, lines, index):]
}

func bottomLines(content string, lines []string, n int) string {
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	return sliceFromLine(content, lines, start)
}

func bottomNonEmptyLines(content string, lines []string, n int) string {
	if n == 0 {
		return ""
	}
	// Like Rust's rev().filter(non-empty).take(n).last(): the start is the
	// earliest of the last n non-empty lines, or the first non-empty line
	// when fewer exist.
	start := -1
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		start = i
	}
	if start < 0 {
		return ""
	}
	return sliceFromLine(content, lines, start)
}

func topNonEmptyLines(content string, lines []string, n int) string {
	// Ends after the n-th non-empty line, or after the last non-empty line
	// when fewer exist.
	end := -1
	seen := 0
	for i, l := range lines {
		if seen == n {
			break
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		seen++
		end = i
	}
	if end < 0 {
		return ""
	}
	return content[:lineStartOffset(content, lines, end+1)]
}

// Codex prompt markers.

func codexPromptLine(line string) bool {
	return line == "›" || strings.HasPrefix(line, "› ")
}

func codexBlockMarkerLine(line string) bool {
	for _, p := range []string{"•", "■", "✗", "✓"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func lastCodexPromptIndex(lines []string) (int, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if codexPromptLine(lines[i]) {
			return i, true
		}
	}
	return 0, false
}

// currentCodexPromptIndex finds the last "›" prompt line, unless a response
// block marker follows it (which makes that prompt stale).
func currentCodexPromptIndex(lines []string) (int, bool) {
	idx, ok := lastCodexPromptIndex(lines)
	if !ok {
		return 0, false
	}
	for _, l := range lines[idx+1:] {
		if codexBlockMarkerLine(l) {
			return 0, false
		}
	}
	return idx, true
}

func afterLastPromptMarker(content string, lines []string) string {
	idx, ok := lastCodexPromptIndex(lines)
	if !ok {
		return content
	}
	return sliceFromLine(content, lines, idx+1)
}

func beforeCurrentPromptMarker(content string, lines []string) string {
	idx, ok := currentCodexPromptIndex(lines)
	if !ok {
		return content
	}
	return content[:lineStartOffset(content, lines, idx)]
}

// Claude prompt box: the input box is the last two horizontal rules on screen;
// the body is whatever sits between them.

func promptBoxBody(content string, lines []string) string {
	top, ok := promptBoxTopBorderIndex(lines)
	if !ok {
		return ""
	}
	start := lineStartOffset(content, lines, top+1)
	end := len(lines)
	for i := top + 1; i < len(lines); i++ {
		if isHorizontalRule(lines[i]) {
			end = i
			break
		}
	}
	return content[start:lineStartOffset(content, lines, end)]
}

func abovePromptBox(content string, lines []string) string {
	top, ok := promptBoxTopBorderIndex(lines)
	if !ok {
		return content
	}
	return content[:lineStartOffset(content, lines, top)]
}

func promptBoxTopBorderIndex(lines []string) (int, bool) {
	borders := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if isHorizontalRule(lines[i]) {
			borders++
			if borders == 2 {
				return i, true
			}
		}
	}
	return 0, false
}

func afterLastHorizontalRule(content string, lines []string) string {
	lastRuleEnd := 0
	off := 0
	for _, l := range lines {
		next := off + len(l) + 1
		if isHorizontalRule(l) {
			lastRuleEnd = min(next, len(content))
		}
		off = next
	}
	return content[lastRuleEnd:]
}

func lastNonEmptyLine(content string) string {
	lines := splitLines(content)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// isHorizontalRule: a trimmed line starting with "─" that is either nothing
// but rule characters, or has at least three of them before a label
// ("─ Conversation recap ────").
func isHorizontalRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	ruleChars := 0
	rest := trimmed
	for strings.HasPrefix(rest, "─") {
		ruleChars++
		rest = rest[len("─"):]
	}
	if ruleChars == 0 {
		return false
	}
	suffix := strings.TrimLeftFunc(rest, unicode.IsSpace)
	return suffix == "" || ruleChars >= 3
}
