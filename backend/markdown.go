package main

import (
	"bytes"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var (
	markdownOnce           sync.Once
	markdowner             goldmark.Markdown
	markdownSafe           *bluemonday.Policy
	wikiDisplayMathPattern = regexp.MustCompile(`\{\\displaystyle\s+([^{}]+)\}`)
	wikiTextMathPattern    = regexp.MustCompile(`\{\\textstyle\s+([^{}]+)\}`)
	citationPattern        = regexp.MustCompile(`\[(\d+)\]`)
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

func renderMarkdownWithSources(input string, conversation ConversationView) template.HTML {
	return renderMarkdown(linkCitationReferences(input, conversation))
}

func normalizeMathMarkup(input string) string {
	input = strings.ReplaceAll(input, "{\\displaystyle", "{\\displaystyle ")
	input = strings.ReplaceAll(input, "{\\textstyle", "{\\textstyle ")
	input = wikiDisplayMathPattern.ReplaceAllString(input, `$$$1$$`)
	input = wikiTextMathPattern.ReplaceAllString(input, `$$1$`)
	return input
}

func linkCitationReferences(input string, conversation ConversationView) string {
	lookup := citationSourceLookup(conversation)
	if len(lookup) == 0 || strings.TrimSpace(input) == "" {
		return input
	}

	matches := citationPattern.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input
	}

	var builder strings.Builder
	lastIndex := 0
	for _, match := range matches {
		start := match[0]
		end := match[1]
		digitStart := match[2]
		digitEnd := match[3]

		builder.WriteString(input[lastIndex:start])
		if end < len(input) && input[end] == '(' {
			builder.WriteString(input[start:end])
			lastIndex = end
			continue
		}

		key := input[digitStart:digitEnd]
		url := strings.TrimSpace(lookup[key])
		if url == "" {
			builder.WriteString(input[start:end])
			lastIndex = end
			continue
		}

		builder.WriteString("[")
		builder.WriteString(key)
		builder.WriteString("](")
		builder.WriteString(url)
		builder.WriteString(")")
		lastIndex = end
	}

	builder.WriteString(input[lastIndex:])
	return builder.String()
}

func citationSourceLookup(conversation ConversationView) map[string]string {
	lookup := make(map[string]string)
	for _, summary := range conversation.Summaries {
		if summary.RerankPosition <= 0 {
			continue
		}
		url := strings.TrimSpace(summary.URL)
		if url == "" {
			continue
		}
		key := strconv.Itoa(summary.RerankPosition)
		if _, exists := lookup[key]; exists {
			continue
		}
		lookup[key] = url
	}

	return lookup
}
