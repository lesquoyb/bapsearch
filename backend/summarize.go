package main

import (
    "context"
    "fmt"
    "log/slog"
    "net/url"
    "strings"
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
    meta := RequestMeta{RequestID: newRequestID(), UserID: job.UserID, ConversationID: job.ConversationID}
    logger := loggerWithMeta(ctx, service.logger, job.ConversationID)
    logger.Info("summary_job_started", "query", job.Query)

    pickedResults := pickRelevantResults(job.Results, service.urlLimit)
    documents := service.fetch.FetchAndExtract(context.WithValue(ctx, requestMetaKey, meta), meta, pickedResults)
    if len(documents) == 0 {
        logger.Info("summary_job_finished", "documents", 0)
        return
    }

    userMemory, _ := service.memory.GetUserMemory(context.WithValue(ctx, requestMetaKey, meta), job.UserID)
    summaries := make([]SummaryRecord, 0, len(documents))

    for _, document := range documents {
        if strings.TrimSpace(document.Text) == "" {
            continue
        }

        summary, err := service.llm.SummarizePage(context.WithValue(ctx, requestMetaKey, meta), meta, job.Query, userMemory, document.URL, document.Text)
        if err != nil {
            logger.Error("page summarization failed", "url", document.URL, "error", err)
            continue
        }

        if err := service.conversations.StoreSummary(context.WithValue(ctx, requestMetaKey, meta), job.ConversationID, document.URL, summary, document.Text); err != nil {
            logger.Error("storing summary failed", "url", document.URL, "error", err)
            continue
        }

        summaries = append(summaries, SummaryRecord{URL: document.URL, Summary: summary, SourceText: document.Text, Status: "ready"})
    }

    if len(summaries) > 0 {
        overview := buildAssistantOverview(summaries)
        if err := service.conversations.AddMessage(context.WithValue(ctx, requestMetaKey, meta), job.ConversationID, "assistant", overview); err != nil {
            logger.Error("storing assistant overview failed", "error", err)
        }
    }

    logger.Info("summary_job_finished", "documents", len(summaries))
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

func buildAssistantOverview(summaries []SummaryRecord) string {
    parts := make([]string, 0, len(summaries)+1)
    parts = append(parts, "I processed the top pages for this search. Open any summary below and continue in chat mode for follow-up questions.")
    for _, summary := range summaries {
        firstLine := strings.TrimSpace(summary.Summary)
        if firstLine == "" {
            continue
        }
        parts = append(parts, fmt.Sprintf("- %s", firstLine))
    }
    return strings.Join(parts, "\n")
}
