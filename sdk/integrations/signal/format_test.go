package signal

import "testing"

func TestStyledText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain passes through", in: "all done, nothing to do", want: "all done, nothing to do"},
		{name: "bold kept", in: "the **build** passed", want: "the **build** passed"},
		{name: "underscore bold becomes stars", in: "__urgent__ fix", want: "**urgent** fix"},
		{name: "italic kept", in: "a *subtle* hint", want: "a *subtle* hint"},
		{name: "single underscores left alone", in: "set max_retries_count in _config_", want: "set max_retries_count in _config_"},
		{name: "inline code kept, content escaped", in: "run `make build && a*b` now", want: "run `make build && a\\*b` now"},
		{name: "header becomes bold", in: "## Summary\ntext", want: "**Summary**\ntext"},
		{name: "double strike narrows", in: "~~gone~~ stays", want: "~gone~ stays"},
		{name: "stray tilde escaped", in: "path ~/x and a~b", want: `path \~/x and a\~b`},
		{name: "double pipe escaped", in: "a || b", want: `a \|\| b`},
		{name: "stray asterisk escaped not eaten", in: "5 * 3 = 15", want: `5 \* 3 = 15`},
		{name: "stray backtick escaped not eaten", in: "odd ` tick", want: "odd \\` tick"},
		{name: "glob escaped", in: "rm -rf build/*", want: `rm -rf build/\*`},
		{name: "link becomes label and url", in: "see [the PR](https://github.com/x/y/pull/1)", want: "see the PR (https://github.com/x/y/pull/1)"},
		{name: "image becomes label and url", in: "![diagram](https://x/y.png)", want: "diagram (https://x/y.png)"},
		{name: "bullets normalised", in: "* first\n- second", want: "• first\n• second"},
		{name: "indented bullet keeps indent", in: "  - nested", want: "  • nested"},
		{name: "rule dropped", in: "above\n---\nbelow", want: "above\nbelow"},
		{name: "blockquote marker stripped", in: "> quoted line", want: "quoted line"},
		{
			name: "fence becomes monospace block with escaped content",
			in:   "before\n```go\nx := a * b\n```\nafter",
			want: "before\n`x := a \\* b`\nafter",
		},
		{
			name: "unterminated fence still closes",
			in:   "before\n```\nx := 1",
			want: "before\n`x := 1`",
		},
		{
			name: "mixed answer",
			in:   "## Result\nThe `average()` helper is **done**:\n* [PR #8](https://github.com/x/y/pull/8)",
			want: "**Result**\nThe `average()` helper is **done**:\n• PR #8 (https://github.com/x/y/pull/8)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := styledText(tt.in); got != tt.want {
				t.Errorf("styledText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPlainText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain passes through", in: "all done, nothing to do", want: "all done, nothing to do"},
		{name: "bold stripped", in: "the **build** passed", want: "the build passed"},
		{name: "underscore bold stripped", in: "__urgent__ fix", want: "urgent fix"},
		{name: "italic stripped", in: "a *subtle* hint", want: "a subtle hint"},
		{name: "inline code stripped", in: "run `make build` first", want: "run make build first"},
		{name: "header stripped", in: "## Summary\ntext", want: "Summary\ntext"},
		{name: "link becomes label and url", in: "see [the PR](https://github.com/x/y/pull/1) now", want: "see the PR (https://github.com/x/y/pull/1) now"},
		{name: "image becomes label and url", in: "![diagram](https://x/y.png)", want: "diagram (https://x/y.png)"},
		{name: "bullets normalised", in: "* first\n- second\n+ third", want: "• first\n• second\n• third"},
		{name: "indented bullet keeps indent", in: "  - nested", want: "  • nested"},
		{name: "horizontal rule dropped", in: "above\n---\nbelow", want: "above\nbelow"},
		{name: "blockquote marker stripped", in: "> quoted line", want: "quoted line"},
		{
			name: "fence markers removed, code kept verbatim",
			in:   "before\n```go\nx := \"**not bold**\"\n```\nafter",
			want: "before\nx := \"**not bold**\"\nafter",
		},
		{
			name: "mixed answer",
			in:   "## Result\nThe `average()` helper is **done**:\n* tests pass\n* [PR #8](https://github.com/x/y/pull/8)",
			want: "Result\nThe average() helper is done:\n• tests pass\n• PR #8 (https://github.com/x/y/pull/8)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plainText(tt.in); got != tt.want {
				t.Errorf("plainText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
