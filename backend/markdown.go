package main

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var (
	markdownOnce sync.Once
	markdowner   goldmark.Markdown
	markdownSafe *bluemonday.Policy
	wikiDisplayMathPattern = regexp.MustCompile(`\{\\displaystyle\s+([^{}]+)\}`)
	wikiTextMathPattern    = regexp.MustCompile(`\{\\textstyle\s+([^{}]+)\}`)
)

func renderMarkdown(input string) template.HTML {
	markdownOnce.Do(func() {
		markdowner = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM,
				extension.Strikethrough,
				extension.Linkify,
			),
			goldmark.WithRendererOptions(
				gmhtml.WithHardWraps(),
				gmhtml.WithUnsafe(),
			),
		)

		markdownSafe = bluemonday.UGCPolicy()
		markdownSafe.AllowAttrs("class").OnElements("code", "pre")
	})

	if input == "" {
		return ""
	}

	input = normalizeMathMarkup(input)

	var buffer bytes.Buffer
	if err := markdowner.Convert([]byte(input), &buffer); err != nil {
		return template.HTML(template.HTMLEscapeString(input))
	}

	return template.HTML(markdownSafe.Sanitize(buffer.String()))
}

func normalizeMathMarkup(input string) string {
	input = strings.ReplaceAll(input, "{\\displaystyle", "{\\displaystyle ")
	input = strings.ReplaceAll(input, "{\\textstyle", "{\\textstyle ")
	input = wikiDisplayMathPattern.ReplaceAllString(input, `$$$1$$`)
	input = wikiTextMathPattern.ReplaceAllString(input, `$$1$`)
	return input
}