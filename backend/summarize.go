package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

const (
	summaryCandidateMultiplier = 4
	minSummarySourceChars      = 500
	minSummaryOutputChars      = 80
)

type SummaryJob struct {
	ConversationID int64
	UserID         string
	Query          string
	Results        []SearchResult
}

type SummarizeService struct {
	conversations *ConversationService
	fetch         *FetchService
	llm           *LLMService
	memory        *MemoryService
	logger        *slog.Logger
	urlLimit      int
}

func (service *SummarizeService) StartWorkers(jobs <-chan SummaryJob, workerCount int) {
	for index := 0; index < workerCount; index++ {
		go func() {
			for job := range jobs {
				service.runJob(job)
			}
		}()
	}
}

func (service *SummarizeService) runJob(job SummaryJob) {
	ctx := context.Background()
	jobContext := context.WithValue(ctx, requestMetaKey, RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID})
	meta := RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID}
	logger := loggerWithMeta(ctx, service.logger, job.ConversationID)
	logger.Info("summary_job_started", "query", job.Query)

	pickedResults := pickRelevantResults(job.Results, service.urlLimit*summaryCandidateMultiplier)
	pickedURLs := make(map[string]struct{}, len(pickedResults))
	for _, result := range pickedResults {
		pickedURLs[result.URL] = struct{}{}
	}
	for _, result := range job.Results {
		if _, ok := pickedURLs[result.URL]; ok {
			continue
		}
		service.updateSummaryStatus(jobContext, logger, job.ConversationID, result.URL, "skipped", "Ignored because higher-ranked sources were selected first.")
	}

	documents := service.fetch.FetchAndExtract(jobContext, meta, pickedResults, func(url, status, detail string) {
		service.updateSummaryStatus(jobContext, logger, job.ConversationID, url, status, detail)
	})
	if len(documents) == 0 {
		logger.Info("summary_job_finished", "documents", 0)
		return
	}

	summaries := make([]SummaryRecord, 0, len(documents))
	for index, document := range documents {
		if strings.TrimSpace(document.Text) == "" {
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "skipped", "Ignored because extracted content was empty.")
			continue
		}

		if err := service.conversations.UpdateSummarySource(jobContext, job.ConversationID, document.URL, document.Text); err != nil {
			logger.Error("updating summary source failed", "url", document.URL, "error", err)
		}

		if len([]rune(strings.TrimSpace(document.Text))) < minSummarySourceChars {
			logger.Info("summary_source_too_short", "url", document.URL, "chars", len([]rune(strings.TrimSpace(document.Text))))
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "skipped", fmt.Sprintf("Ignored because the extracted text was too short (%d characters).", len([]rune(strings.TrimSpace(document.Text)))))
			continue
		}

		service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "summarizing", "LLM summary generation in progress.")

		summary, err := service.llm.SummarizePage(jobContext, meta, job.Query, "", document.URL, document.Text)
		if err != nil {
			logger.Error("page summarization failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "error", err.Error())
			continue
		}

		if !isUsefulSummary(summary) {
			logger.Info("summary_output_rejected", "url", document.URL, "summary", strings.TrimSpace(summary))
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "skipped", "Ignored because the model returned an incomplete summary.")
			continue
		}

		if err := service.conversations.StoreSummary(jobContext, job.ConversationID, document.URL, summary, document.Text); err != nil {
			logger.Error("storing summary failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(jobContext, logger, job.ConversationID, document.URL, "error", err.Error())
			continue
		}

		summaries = append(summaries, SummaryRecord{URL: document.URL, Summary: summary, SourceText: document.Text, Status: "ready"})
		if len(summaries) == service.urlLimit {
			for _, leftover := range documents[index+1:] {
				service.updateSummaryStatus(jobContext, logger, job.ConversationID, leftover.URL, "skipped", "Ignored because enough validated summaries were already generated.")
			}
			break
		}
	}

	if len(summaries) >= service.urlLimit {
		overview, err := service.llm.SynthesizeSearchAnswer(jobContext, meta, job.Query, summaries)
		if err != nil || !isUsefulOverview(overview) {
			if err != nil {
				logger.Error("summary synthesis failed", "error", err)
			}
			overview = buildAssistantOverview(job.Query, summaries)
		}
		if err := service.conversations.AddMessage(jobContext, job.ConversationID, "assistant", overview); err != nil {
			logger.Error("storing assistant overview failed", "error", err)
		}
	} else {
		logger.Info("summary_synthesis_waiting", "ready_summaries", len(summaries), "target", service.urlLimit)
	}

	logger.Info("summary_job_finished", "documents", len(summaries))
}

func (service *SummarizeService) updateSummaryStatus(ctx context.Context, logger *slog.Logger, conversationID int64, url, status, detail string) {
	if err := service.conversations.UpdateSummaryStatus(ctx, conversationID, url, status, detail); err != nil {
		logger.Error("updating summary status failed", "url", url, "status", status, "error", err)
	}
}

func pickRelevantResults(results []SearchResult, limit int) []SearchResult {
	if limit <= 0 {
		limit = 3
	}

	picked := make([]SearchResult, 0, limit)
	seenHosts := map[string]bool{}

	for _, result := range results {
		parsed, err := url.Parse(result.URL)
		host := result.URL
		if err == nil {
			host = parsed.Hostname()
		}

		if seenHosts[host] {
			continue
		}
		seenHosts[host] = true
		picked = append(picked, result)
		if len(picked) == limit {
			break
		}
	}

	return picked
}

func isUsefulSummary(summary string) bool {
	trimmed := strings.TrimSpace(summary)
	if len([]rune(trimmed)) < minSummaryOutputChars {
		return false
	}

	normalized := strings.ToLower(trimmed)
	return normalized != "## summary" && normalized != "summary"
}

func isUsefulOverview(summary string) bool {
	return len([]rune(strings.TrimSpace(summary))) >= 140
}

func buildAssistantOverview(query string, summaries []SummaryRecord) string {
	parts := make([]string, 0, len(summaries)+2)
	parts = append(parts, fmt.Sprintf("I synthesized the validated search summaries for: %s", query))
	for _, summary := range summaries {
		firstLine := strings.TrimSpace(summary.Summary)
		if firstLine == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("- %s", firstLine))
	}
	parts = append(parts, "Limits: some sources were skipped because extraction failed or the content was too thin to summarize reliably.")
	return strings.Join(parts, "\n")
}
