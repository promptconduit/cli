package enrich

import (
	"regexp"
	"strings"
)

// Task kinds carried as prompt.task_kind. The set and the keyword rules below
// are mirrored by every reader of the field; keep them in sync.
const (
	TaskKindShip     = "ship"
	TaskKindDebug    = "debug"
	TaskKindBuild    = "build"
	TaskKindRefactor = "refactor"
	TaskKindTest     = "test"
	TaskKindDocs     = "docs"
	TaskKindReview   = "review"
	TaskKindExplore  = "explore"
	TaskKindChore    = "chore"
	TaskKindOther    = "other"
)

// TaskKinds lists every task_kind value.
var TaskKinds = []string{
	TaskKindShip, TaskKindDebug, TaskKindBuild, TaskKindRefactor, TaskKindTest,
	TaskKindDocs, TaskKindReview, TaskKindExplore, TaskKindChore, TaskKindOther,
}

// shortFollowUpWords is the word count at or below which an unclassified
// prompt ("yes please", "do it") inherits the previous prompt's kind.
const shortFollowUpWords = 3

type taskKindRule struct {
	kind string
	re   *regexp.Regexp
}

// taskKindRules are tried against the lower-cased prompt; the match that
// starts EARLIEST in the text wins ("publish the blog post" is ship, not
// docs), and on a tie the rule listed first wins. The patterns are a
// byte-for-byte mirror of the editor extension's classifier (some match word
// prefixes on purpose: "merge" also catches "merged"); keep them identical.
var taskKindRules = []taskKindRule{
	{TaskKindShip, regexp.MustCompile(`\b(merge|ship|publish|deploy|release|cut v\d|tag v?\d|land (it|this|the)|push it|open (a|the) pr|submit (it|the))`)},
	{TaskKindDebug, regexp.MustCompile(`\b(fix|bug|error|broken|failing|fails|why (is|does|did)|doesn'?t work|crash|regression)`)},
	{TaskKindTest, regexp.MustCompile(`\b(tests?|coverage|e2e|spec)\b`)},
	{TaskKindRefactor, regexp.MustCompile(`\b(refactor|rename|clean ?up|simplify|extract)`)},
	{TaskKindDocs, regexp.MustCompile(`\b(docs?|readme|changelog|document|write up|blog post|draft)\b`)},
	{TaskKindReview, regexp.MustCompile(`\b(review|audit|look over|check|verify|ensure|confirm)\b`)},
	{TaskKindExplore, regexp.MustCompile(`\b(how (does|do|can)|where (is|are)|explain|what (is|are)|find|investigate|research|plan)\b`)},
	{TaskKindChore, regexp.MustCompile(`\b(bump|upgrade|update deps|dependencies|config|rotate|lint|format)\b`)},
	{TaskKindBuild, regexp.MustCompile(`\b(add|build|implement|create|make|support|wire up|feature)\b`)},
}

// ClassifyTaskKind maps prompt text to a task kind with local keyword rules.
// The text never leaves the machine; only the returned kind does.
func ClassifyTaskKind(prompt string) string {
	text := strings.ToLower(prompt)
	best, bestAt := TaskKindOther, -1
	for _, r := range taskKindRules {
		loc := r.re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		if bestAt == -1 || loc[0] < bestAt {
			best, bestAt = r.kind, loc[0]
		}
	}
	return best
}

// taskKindFor classifies prompt, letting a short unclassified follow-up
// inherit previous (the session's last kind, "" when none).
func taskKindFor(prompt, previous string) string {
	if strings.TrimSpace(prompt) == "" && previous != "" {
		return previous
	}
	kind := ClassifyTaskKind(prompt)
	if kind == TaskKindOther && previous != "" && len(strings.Fields(prompt)) <= shortFollowUpWords {
		return previous
	}
	return kind
}
