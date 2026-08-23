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
		{name: "snake_case kept, underscore emphasis converted", in: "set max_retries_count in _config_", want: "set max_retries_count in *config*"},
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
		{name: "word-internal asterisks escaped", in: "2*3 and 4*5", want: `2\*3 and 4\*5`},
		{name: "glob pattern asterisks escaped", in: "build/*.go and src/*.ts", want: `build/\*.go and src/\*.ts`},
		{name: "equation asterisks escaped", in: "a*b and c*d", want: `a\*b and c\*d`},
		{name: "italic at line edges kept", in: "*italic*", want: "*italic*"},
		{name: "italic mid sentence kept", in: "say *hi* now", want: "say *hi* now"},
		{
			name: "empty fence renders nothing",
			in:   "before\n```\n```\nafter",
			want: "before\nafter",
		},
		{
			name: "blank-line fence renders nothing",
			in:   "before\n```\n\n```\nafter",
			want: "before\nafter",
		},
		{
			name: "whitespace-only fence renders nothing",
			in:   "before\n```\n   \n```\nafter",
			want: "before\nafter",
		},
		{
			name: "unterminated blank fence renders nothing",
			in:   "before\n```\n\n",
			want: "before",
		},
		{
			name: "non-empty fence still wraps",
			in:   "before\n```\ncode\n```\nafter",
			want: "before\n`code`\nafter",
		},
		{name: "asterisks after a colon escaped", in: "Note:*important*", want: `Note:\*important\*`},
		{name: "underscore emphasis becomes stars", in: "_config_ and *star*", want: "*config* and *star*"},
		{
			name: "fence keeps interior blank lines",
			in:   "before\n```\ncode\n\nmore\n```\nafter",
			want: "before\n`code\n\nmore`\nafter",
		},
		{
			name: "fence keeps indentation",
			in:   "```\n    indented\n```",
			want: "`    indented`",
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
		{name: "underscore emphasis stripped", in: "see _project_ now", want: "see project now"},
		{name: "snake_case underscores preserved", in: "set max_retries_count and _config_value", want: "set max_retries_count and _config_value"},
		{name: "word-internal asterisks kept", in: "2*3 and build/*.go", want: "2*3 and build/*.go"},
		{
			name: "empty fence renders nothing",
			in:   "before\n```\n```\nafter",
			want: "before\nafter",
		},
		{
			name: "whitespace-only fence renders nothing",
			in:   "before\n```\n   \n```\nafter",
			want: "before\nafter",
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
