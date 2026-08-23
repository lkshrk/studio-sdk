package signal

import (
	"regexp"
	"strings"
)

// Signal has no markdown renderer. With text_mode "styled" it understands
// *italic*, **bold**, `monospace`, ~strikethrough~ and ||spoiler|| markers;
// everything else in the Telegram-flavoured markdown that agent output and
// model answers carry would reach the group as literal characters. styledText
// translates into that syntax, plainText strips it for contexts such as poll
// questions, which carry no styles at all.

var (
	mdHeader     = regexp.MustCompile(`^#{1,6}\s+`)
	mdRule       = regexp.MustCompile(`^\s*(?:[-*_][ \t]*){3,}$`)
	mdBullet     = regexp.MustCompile(`^(\s*)[*+-]\s+`)
	mdQuote      = regexp.MustCompile(`^\s*>\s?`)
	mdBoldStars  = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdBoldUnder  = regexp.MustCompile(`__([^_]+)__`)
	mdEmStars    = regexp.MustCompile(`\*([^*\s][^*]*)\*`)
	mdInlineCode = regexp.MustCompile("`([^`]*)`")
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdStrike     = regexp.MustCompile(`~~([^~]+)~~`)
)

var styleEscaper = strings.NewReplacer("*", `\*`, "`", "\\`", "~", `\~`, "|", `\|`)

// escapeStyleChars backslash-escapes the characters Signal's styled-text parser
// treats as markers, so content renders verbatim instead of toggling styles.
func escapeStyleChars(s string) string { return styleEscaper.Replace(s) }

// Placeholders stand in for markers this pass has already claimed, so the
// blanket escape below cannot escape them along with the unpaired ones.
const (
	phStrike = "\x00"
	phBold   = "\x01"
	phItalic = "\x02"
)

// styledInline converts one line of markdown into Signal styled syntax. Inline
// code spans keep their backticks — Signal renders them monospace — but their
// content is escaped.
func styledInline(line string) string {
	parts := mdInlineCode.Split(line, -1)
	codes := mdInlineCode.FindAllStringSubmatch(line, -1)
	var b strings.Builder
	for i, part := range parts {
		part = mdImage.ReplaceAllString(part, "$1 ($2)")
		part = mdLink.ReplaceAllString(part, "$1 ($2)")
		part = mdStrike.ReplaceAllString(part, phStrike+"$1"+phStrike)
		part = mdBoldUnder.ReplaceAllString(part, phBold+"$1"+phBold+phBold)
		part = mdBoldStars.ReplaceAllString(part, phBold+"$1"+phBold+phBold)
		part = mdEmStars.ReplaceAllString(part, phItalic+"$1"+phItalic)
		// Anything still carrying a style character is unpaired; the parser
		// would silently swallow it, so escape rather than lose it.
		part = escapeStyleChars(part)
		part = strings.ReplaceAll(part, phBold+phBold, "**")
		part = strings.ReplaceAll(part, phBold, "**")
		part = strings.ReplaceAll(part, phItalic, "*")
		part = strings.ReplaceAll(part, phStrike, "~")
		b.WriteString(part)
		if i < len(codes) {
			b.WriteString("`" + escapeStyleChars(codes[i][1]) + "`")
		}
	}
	return b.String()
}

// styledText renders markdown content in Signal's styled-text syntax: bold,
// italic and monospace survive as real styles, headers become bold lines,
// fenced code becomes a monospace block, links render as "label (url)" (Signal
// linkifies the raw URL), bullets become •, rules and quote markers go away.
func styledText(s string) string {
	if s == "" {
		return s
	}
	var out []string
	var fence []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				out = append(out, "`"+strings.Join(fence, "\n")+"`")
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, escapeStyleChars(line))
			continue
		}
		if mdRule.MatchString(line) {
			continue
		}
		if m := mdHeader.FindString(line); m != "" {
			out = append(out, "**"+styledInline(line[len(m):])+"**")
			continue
		}
		line = mdQuote.ReplaceAllString(line, "")
		line = mdBullet.ReplaceAllString(line, "$1• ")
		out = append(out, styledInline(line))
	}
	if inFence && len(fence) > 0 {
		out = append(out, "`"+strings.Join(fence, "\n")+"`")
	}
	return strings.Join(out, "\n")
}

// plainText renders markdown-formatted content as readable prose, for the
// contexts Signal gives no styles at all.
func plainText(s string) string {
	if s == "" {
		return s
	}
	var out []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if mdRule.MatchString(line) && !mdBullet.MatchString(line) {
			continue
		}
		line = mdHeader.ReplaceAllString(line, "")
		line = mdQuote.ReplaceAllString(line, "")
		line = mdBullet.ReplaceAllString(line, "$1• ")
		line = mdImage.ReplaceAllString(line, "$1 ($2)")
		line = mdLink.ReplaceAllString(line, "$1 ($2)")
		line = mdBoldStars.ReplaceAllString(line, "$1")
		line = mdBoldUnder.ReplaceAllString(line, "$1")
		line = mdEmStars.ReplaceAllString(line, "$1")
		line = mdInlineCode.ReplaceAllString(line, "$1")
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
