package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	ForceFull      bool
}

type RankedSource struct {
	URL             string
	Title           string
	Snippet         string
	SourceText      string
	SimilarityScore float64
	RerankPosition  int
	QueryVariant    string
	EmbeddingJSON   string
}

type SummarizeService struct {
	conversations *ConversationService
	search        *SearchService
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
	logger := loggerWithMeta(jobContext, service.logger, job.ConversationID)
	logger.Info("pipeline_job_started", "query", job.Query, "force_full", job.ForceFull)

	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "searching", "Searching the web."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}

	query := strings.TrimSpace(job.Query)
	processedAny := false

	if job.ForceFull && len(job.Results) > 0 {
		if err := service.processResults(jobContext, meta, logger, job.ConversationID, job.Results); err != nil {
			service.failPipeline(jobContext, logger, job.ConversationID, err)
			return
		}
		processedAny = true
	} else {
		response, err := service.search.Search(jobContext, query)
		if err != nil {
			service.failPipeline(jobContext, logger, job.ConversationID, err)
			return
		}

		inserted, err := service.conversations.AppendSearchResults(jobContext, job.ConversationID, response.Results, response.EngineStatus)
		if err != nil {
			service.failPipeline(jobContext, logger, job.ConversationID, err)
			return
		}

		if len(inserted) > 0 {
			if err := service.processResults(jobContext, meta, logger, job.ConversationID, inserted); err != nil {
				service.failPipeline(jobContext, logger, job.ConversationID, err)
				return
			}
			processedAny = true
		}
	}

	if !processedAny {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("no search results could be processed"))
		return
	}

	if err := service.conversations.StoreRewrittenQuery(jobContext, job.ConversationID, query); err != nil {
		logger.Error("persisting query failed", "error", err)
	}

	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "ranking", "Ranking sources with query and document embeddings."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}

	queryEmbedding, err := service.llm.EmbedText(jobContext, meta, query)
	if err != nil {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("query embedding failed: %w", err))
		return
	}

	rankedSources, err := service.rankSources(jobContext, logger, job.ConversationID, queryEmbedding)
	if err != nil {
		service.failPipeline(jobContext, logger, job.ConversationID, err)
		return
	}

	if len(rankedSources) == 0 {
		service.failPipeline(jobContext, logger, job.ConversationID, fmt.Errorf("no extracted sources were eligible for ranking"))
		return
	}

	readyCount := service.urlLimit
	if readyCount <= 0 {
		readyCount = 3
	}
	if len(rankedSources) < readyCount {
		readyCount = len(rankedSources)
	}

	detail := fmt.Sprintf("Ready to stream an answer from the top %d ranked sources.", readyCount)
	if err := service.conversations.UpdateAnswerStatus(jobContext, job.ConversationID, "ready", detail); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}

	logger.Info("pipeline_job_finished", "ranked_sources", len(rankedSources), "query", query)
}

func (service *SummarizeService) processResults(ctx context.Context, meta RequestMeta, logger *slog.Logger, conversationID int64, results []SearchResult) error {
	if len(results) == 0 {
		return nil
	}

	if err := service.conversations.UpdateAnswerStatus(ctx, conversationID, "extracting", "Extracting source text from the collected results."); err != nil {
		logger.Error("updating answer status failed", "error", err)
	}

	docCh := service.fetch.FetchAndExtractChan(ctx, meta, results, func(url, status, detail string) {
		service.updateSummaryStatus(ctx, logger, conversationID, url, status, detail)
	})

	// Track which URLs were fetched to detect failures afterwards.
	fetched := make(map[string]bool)

	// Process each document as soon as it arrives: store source text immediately
	// (making hover-card content visible) then compute and store the embedding.
	for document := range docCh {
		fetched[document.URL] = true

		if strings.TrimSpace(document.Text) == "" {
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "skipped", "Ignored because extracted content was empty.")
			continue
		}
		if len([]rune(strings.TrimSpace(document.Text))) < minSummarySourceChars {
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "skipped", fmt.Sprintf("Ignored because the extracted text was too short (%d characters).", len([]rune(strings.TrimSpace(document.Text)))))
			continue
		}

		// Store source text immediately so the hover card can show it while
		// we wait for the embedding to be computed.
		if err := service.conversations.StoreSourceText(ctx, conversationID, document.URL, document.Text); err != nil {
			logger.Error("storing source text failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "error", err.Error())
			continue
		}

		service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "embedding", "Generating document embeddings.")
		embedding, err := service.llm.EmbedText(ctx, meta, document.Text)
		if err != nil {
			logger.Error("document embedding failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "error", err.Error())
			continue
		}

		embeddingJSON, err := json.Marshal(embedding)
		if err != nil {
			logger.Error("embedding serialization failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "error", err.Error())
			continue
		}

		if err := service.conversations.UpdateDocumentEmbedding(ctx, conversationID, document.URL, string(embeddingJSON)); err != nil {
			logger.Error("storing document embedding failed", "url", document.URL, "error", err)
			service.updateSummaryStatus(ctx, logger, conversationID, document.URL, "error", err.Error())
			continue
		}
	}

	for _, result := range results {
		if !fetched[result.URL] {
			service.updateSummaryStatus(ctx, logger, conversationID, result.URL, "error", "Failed to extract source text.")
		}
	}

	return nil
}

func (service *SummarizeService) rankSources(ctx context.Context, logger *slog.Logger, conversationID int64, queryEmbedding []float64) ([]RankedSource, error) {
	return service.conversations.RerankAllSources(ctx, logger, conversationID, queryEmbedding)
}

func (service *SummarizeService) failPipeline(ctx context.Context, logger *slog.Logger, conversationID int64, err error) {
	logger.Error("pipeline_failed", "error", err)
	if statusErr := service.conversations.UpdateAnswerStatus(ctx, conversationID, "error", err.Error()); statusErr != nil {
		logger.Error("updating failed pipeline status", "error", statusErr)
	}
}

func (service *SummarizeService) updateSummaryStatus(ctx context.Context, logger *slog.Logger, conversationID int64, url, status, detail string) {
	if err := service.conversations.UpdateSummaryStatus(ctx, conversationID, url, status, detail); err != nil {
		logger.Error("updating summary status failed", "url", url, "status", status, "error", err)
	}
}


func containsDocument(documents []PageDocument, url string) bool {
	for _, document := range documents {
		if document.URL == url {
			return true
		}
	}
	return false
}

func cosineSimilarity(left, right []float64) float64 {
	if len(left) == 0 || len(right) == 0 || len(left) != len(right) {
		return 0
	}

	dot := 0.0
	leftNorm := 0.0
	rightNorm := 0.0
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (sqrt(leftNorm) * sqrt(rightNorm))
}

func sqrt(value float64) float64 {
	if value <= 0 {
		return 0
	}
	z := value
	for iteration := 0; iteration < 8; iteration++ {
		z -= (z*z - value) / (2 * z)
	}
	return z
}
