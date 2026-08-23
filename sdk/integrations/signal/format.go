package signal

import (
	"regexp"
	"strings"
	"unicode"
)

// Signal styled mode knows only *italic*, **bold**, `mono`, ~strike~, ||spoiler||; other markdown must be translated or stripped.

var (
	mdHeader     = regexp.MustCompile(`^#{1,6}\s+`)
	mdRule       = regexp.MustCompile(`^\s*(?:[-*_][ \t]*){3,}$`)
	mdBullet     = regexp.MustCompile(`^(\s*)[*+-]\s+`)
	mdQuote      = regexp.MustCompile(`^\s*>\s?`)
	mdBoldStars  = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdBoldUnder  = regexp.MustCompile(`__([^_]+)__`)
	mdInlineCode = regexp.MustCompile("`([^`]*)`")
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdStrike     = regexp.MustCompile(`~~([^~]+)~~`)
)

var styleEscaper = strings.NewReplacer("*", `\*`, "`", "\\`", "~", `\~`, "|", `\|`)

func escapeStyleChars(s string) string { return styleEscaper.Replace(s) }

// Placeholders shield already-paired markers from the blanket escape.
const (
	phStrike = "\x00"
	phBold   = "\x01"
	phItalic = "\x02"
)

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func emphasisCanOpen(runes []rune, i int) bool {
	if i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) || runes[i+1] == runes[i] {
		return false
	}
	if i == 0 {
		return true
	}
	prev := runes[i-1]
	return unicode.IsSpace(prev) || strings.ContainsRune("([{<\"'", prev)
}

// Emphasis pairs only at word boundaries; word-internal markers stay literal.
func replaceEmphasis(s string, marker rune, wrap string) string {
	if !strings.ContainsRune(s, marker) {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		if runes[i] != marker || !emphasisCanOpen(runes, i) {
			b.WriteRune(runes[i])
			i++
			continue
		}
		end := -1
		for k := i + 2; k < len(runes); k++ {
			if runes[k] != marker {
				continue
			}
			if !unicode.IsSpace(runes[k-1]) && (k+1 == len(runes) || !isWordRune(runes[k+1])) {
				end = k
			}
			break
		}
		if end < 0 {
			b.WriteRune(runes[i])
			i++
			continue
		}
		b.WriteString(wrap)
		b.WriteString(string(runes[i+1 : end]))
		b.WriteString(wrap)
		i = end + 1
	}
	return b.String()
}

// styledInline converts one markdown line to Signal styled syntax; inline code keeps its backticks.
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
		part = replaceEmphasis(part, '*', phItalic)
		part = replaceEmphasis(part, '_', phItalic)
		// Remaining style characters are unpaired; escape them so the parser cannot swallow them.
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

// Reports false for a whitespace-only block, which would render as bare backticks.
func fenceBlock(fence []string) (string, bool) {
	joined := strings.Join(fence, "\n")
	return joined, strings.TrimSpace(joined) != ""
}

// styledText renders markdown as Signal styled text: real styles, bold headers, "label (url)" links, • bullets.
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
				if block, ok := fenceBlock(fence); ok {
					out = append(out, "`"+block+"`")
				}
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
	if inFence {
		if block, ok := fenceBlock(fence); ok {
			out = append(out, "`"+block+"`")
		}
	}
	return strings.Join(out, "\n")
}

func plainText(s string) string {
	if s == "" {
		return s
	}
	var out []string
	var fence []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				if block, ok := fenceBlock(fence); ok {
					out = append(out, block)
				}
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, line)
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
		line = replaceEmphasis(line, '*', "")
		line = replaceEmphasis(line, '_', "")
		line = mdInlineCode.ReplaceAllString(line, "$1")
		out = append(out, line)
	}
	if inFence {
		if block, ok := fenceBlock(fence); ok {
			out = append(out, block)
		}
	}
	return strings.Join(out, "\n")
}
