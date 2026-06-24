package steering

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"flow/internal/productdb"
)

type TaskImpactInput struct {
	Source string
	People []string
	Text   string
}

type TaskImpactHint struct {
	TaskSlug    string `json:"task_slug"`
	TaskName    string `json:"task_name"`
	ProjectSlug string `json:"project_slug,omitempty"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	Strength    string `json:"strength"`
	Reason      string `json:"reason"`
	Evidence    string `json:"evidence"`
}

func BuildTaskImpactHints(db *sql.DB, in TaskImpactInput) ([]TaskImpactHint, error) {
	if db == nil {
		return nil, fmt.Errorf("steering: nil db")
	}

	people := normalizeImpactPeople(in.People)
	if len(people) == 0 {
		return nil, nil
	}

	tasks, err := productdb.ListTasks(db, productdb.TaskFilter{Kind: "", IncludeArchived: true})
	if err != nil {
		return nil, fmt.Errorf("steering: list tasks for impact hints: %w", err)
	}
	slugs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if impactTaskActive(task) {
			slugs = append(slugs, task.Slug)
		}
	}
	tagsByTask, err := productdb.GetTaskTagsBatch(db, slugs)
	if err != nil {
		return nil, fmt.Errorf("steering: task tags for impact hints: %w", err)
	}

	var hints []TaskImpactHint
	for _, task := range tasks {
		if !impactTaskActive(task) {
			continue
		}
		if hint, ok := impactHintForTask(task, people, tagsByTask[task.Slug]); ok {
			hints = append(hints, hint)
		}
	}

	sort.Slice(hints, func(i, j int) bool {
		if a, b := impactStrengthRank(hints[i].Strength), impactStrengthRank(hints[j].Strength); a != b {
			return a > b
		}
		if a, b := impactPriorityRank(hints[i].Priority), impactPriorityRank(hints[j].Priority); a != b {
			return a > b
		}
		return hints[i].TaskSlug < hints[j].TaskSlug
	})
	if len(hints) > 3 {
		hints = hints[:3]
	}
	return hints, nil
}

func impactTaskActive(task *productdb.Task) bool {
	return task != nil && !task.DeletedAt.Valid && task.Status != "done"
}

func impactHintForTask(task *productdb.Task, people, tags []string) (TaskImpactHint, bool) {
	if person, evidence, ok := impactMatchPeopleField(people, task.WaitingOn); ok {
		return impactHint(task, "strong", "waiting_on mentions "+person, evidence), true
	}
	if person, evidence, ok := impactMatchPeopleField(people, task.Assignee); ok {
		return impactHint(task, "strong", "assignee matches "+person, evidence), true
	}
	if person, ok := impactMatchPeopleText(people, task.Name); ok {
		return impactHint(task, "medium", "task name mentions "+person, strings.TrimSpace(task.Name)), true
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if person, ok := impactMatchPeopleText(people, tag); ok {
			return impactHint(task, "medium", "task tag mentions "+person, tag), true
		}
	}
	return TaskImpactHint{}, false
}

func impactHint(task *productdb.Task, strength, reason, evidence string) TaskImpactHint {
	hint := TaskImpactHint{
		TaskSlug: task.Slug,
		TaskName: task.Name,
		Status:   task.Status,
		Priority: task.Priority,
		Strength: strength,
		Reason:   reason,
		Evidence: evidence,
	}
	if task.ProjectSlug.Valid {
		hint.ProjectSlug = strings.TrimSpace(task.ProjectSlug.String)
	}
	return hint
}

func impactMatchPeopleField(people []string, field sql.NullString) (string, string, bool) {
	if !field.Valid {
		return "", "", false
	}
	text := strings.TrimSpace(field.String)
	if text == "" {
		return "", "", false
	}
	person, ok := impactMatchPeopleText(people, text)
	return person, text, ok
}

func impactMatchPeopleText(people []string, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	for _, person := range people {
		if impactPersonMatchesText(person, text) {
			return person, true
		}
	}
	return "", false
}

func normalizeImpactPeople(names []string) []string {
	seen := make(map[string]bool, len(names))
	var out []string
	for _, name := range names {
		normalized := strings.Join(strings.Fields(name), " ")
		if normalized == "" {
			continue
		}
		key := strings.ToLower(normalized)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, normalized)
	}
	return out
}

func impactPersonMatchesText(person, text string) bool {
	personPhraseTokens := impactTokens(person)
	textPhraseTokens := impactTokens(text)
	if len(personPhraseTokens) >= 2 && impactContainsTokenPhrase(textPhraseTokens, personPhraseTokens) {
		return true
	}

	personTokens := impactUniqueTokens(impactMeaningfulTokens(person))
	if len(personTokens) == 0 {
		return false
	}
	waitingTokens := make(map[string]bool)
	for _, token := range impactMeaningfulTokens(text) {
		waitingTokens[token] = true
	}
	if len(waitingTokens) == 0 {
		return false
	}

	matches := 0
	distinctiveMatch := false
	for _, token := range personTokens {
		if !waitingTokens[token] {
			continue
		}
		matches++
		if impactDistinctiveToken(token) {
			distinctiveMatch = true
		}
	}
	if matches >= 2 {
		return true
	}
	return matches == 1 && distinctiveMatch
}

func impactTokens(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func impactContainsTokenPhrase(tokens, phrase []string) bool {
	if len(phrase) == 0 || len(phrase) > len(tokens) {
		return false
	}
	for i := 0; i <= len(tokens)-len(phrase); i++ {
		matched := true
		for j, token := range phrase {
			if tokens[i+j] != token {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func impactUniqueTokens(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	var out []string
	for _, token := range tokens {
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func impactMeaningfulTokens(s string) []string {
	var out []string
	for _, part := range impactTokens(s) {
		if utf8.RuneCountInString(part) < 3 || weakImpactToken(part) {
			continue
		}
		out = append(out, part)
	}
	return out
}

func impactDistinctiveToken(token string) bool {
	return utf8.RuneCountInString(token) >= 5 && !weakImpactToken(token)
}

func weakImpactToken(token string) bool {
	switch token {
	case "review", "approval", "task", "work", "leave", "tomorrow", "after",
		"the", "and", "for", "will", "may", "can", "could", "would", "should",
		"shall", "might", "must", "need", "needs", "next", "this", "that",
		"with", "from", "into", "onto", "over", "under",
		"jan", "january", "feb", "february", "mar", "march", "apr", "april",
		"jun", "june", "jul", "july", "aug", "august", "sep", "sept",
		"september", "oct", "october", "nov", "november", "dec", "december":
		return true
	default:
		return false
	}
}

func impactStrengthRank(strength string) int {
	switch strings.ToLower(strings.TrimSpace(strength)) {
	case "strong":
		return 3
	case "medium":
		return 2
	case "weak":
		return 1
	default:
		return 0
	}
}

func impactPriorityRank(priority string) int {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}
