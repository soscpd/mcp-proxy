package main

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 parameters.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// searchResult holds a tool info with a relevance score.
type searchResult struct {
	ToolInfo
	Score float64
}

// searchTools performs BM25 ranking over tool name + description fields.
// Returns up to maxResults, ranked by relevance.
func searchTools(tools []ToolInfo, query string, maxResults int) []searchResult {
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	// Build corpus: each document is the concatenation of name + description.
	type doc struct {
		info   ToolInfo
		tokens []string
	}
	docs := make([]doc, len(tools))
	totalLen := 0
	for i, t := range tools {
		// Replace dots/underscores/hyphens with spaces for tokenization.
		text := strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(t.Name) + " " + t.Description
		tokens := tokenize(text)
		docs[i] = doc{info: t, tokens: tokens}
		totalLen += len(tokens)
	}
	if len(docs) == 0 {
		return nil
	}
	avgDL := float64(totalLen) / float64(len(docs))

	// Compute IDF for each query term.
	df := make(map[string]int) // document frequency
	for _, d := range docs {
		seen := make(map[string]bool)
		for _, tok := range d.tokens {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}

	n := float64(len(docs))
	idf := make(map[string]float64)
	for _, qt := range queryTerms {
		docFreq := float64(df[qt])
		// Standard BM25 IDF: log((N - df + 0.5) / (df + 0.5) + 1)
		idf[qt] = math.Log((n-docFreq+0.5)/(docFreq+0.5) + 1)
	}

	// Score each document.
	results := make([]searchResult, 0)
	for _, d := range docs {
		tf := make(map[string]int)
		for _, tok := range d.tokens {
			tf[tok]++
		}

		score := 0.0
		dl := float64(len(d.tokens))
		for _, qt := range queryTerms {
			f := float64(tf[qt])
			if f == 0 {
				continue
			}
			num := f * (bm25K1 + 1)
			denom := f + bm25K1*(1-bm25B+bm25B*dl/avgDL)
			score += idf[qt] * num / denom
		}

		if score > 0 {
			results = append(results, searchResult{
				ToolInfo: d.info,
				Score:    score,
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}

// tokenize splits text into lowercase alpha-numeric tokens.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	// Remove very short tokens.
	var result []string
	for _, w := range words {
		if len(w) >= 2 {
			result = append(result, w)
		}
	}
	return result
}
