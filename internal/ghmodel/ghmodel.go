// Package ghmodel holds GitHub domain types and pure helpers shared between the
// sync layer (which detects changes) and the render layer (which writes
// markdown). Keeping them here avoids an import cycle between the two and gives
// the discussion GraphQL shapes a single home that both the fetcher and the
// exporter can unmarshal into.
package ghmodel

import (
	"fmt"
	"strings"

	"github.com/mevdschee/github-export/internal/jsonutil"
)

// --- Discussion GraphQL shapes (fetched by sync, rendered by exporter) ---

type DiscussionAuthor struct {
	Login string `json:"login"`
}

type DiscussionCategory struct {
	Name         string `json:"name"`
	Emoji        string `json:"emoji"`
	IsAnswerable bool   `json:"isAnswerable"`
}

type DiscussionReply struct {
	DatabaseID int64            `json:"databaseId"`
	CreatedAt  string           `json:"createdAt"`
	Body       string           `json:"body"`
	Author     DiscussionAuthor `json:"author"`
}

type DiscussionComment struct {
	DatabaseID int64            `json:"databaseId"`
	CreatedAt  string           `json:"createdAt"`
	Body       string           `json:"body"`
	Author     DiscussionAuthor `json:"author"`
	Replies    struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []DiscussionReply `json:"nodes"`
	} `json:"replies"`
}

type DiscussionNode struct {
	Number      int64              `json:"number"`
	Title       string             `json:"title"`
	Body        string             `json:"body"`
	CreatedAt   string             `json:"createdAt"`
	UpdatedAt   string             `json:"updatedAt"`
	Closed      bool               `json:"closed"`
	ClosedAt    string             `json:"closedAt"`
	StateReason string             `json:"stateReason"`
	Locked      bool               `json:"locked"`
	URL         string             `json:"url"`
	Author      DiscussionAuthor   `json:"author"`
	Category    DiscussionCategory `json:"category"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Answer *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"answer"`
	AnswerChosenAt string            `json:"answerChosenAt"`
	AnswerChosenBy *DiscussionAuthor `json:"answerChosenBy"`
	Comments       struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []DiscussionComment `json:"nodes"`
	} `json:"comments"`
}

// DiscussionState maps the boolean closed flag to the open/closed string used
// across the export.
func DiscussionState(d DiscussionNode) string {
	if d.Closed {
		return "closed"
	}
	return "open"
}

// AnswerID returns the chosen-answer comment ID, or 0 if the discussion has no
// accepted answer.
func (d DiscussionNode) AnswerID() int64 {
	if d.Answer == nil {
		return 0
	}
	return d.Answer.DatabaseID
}

// --- Projects v2 helpers ---

// FieldValueString flattens a single Projects v2 field value node into its
// string representation, keyed off the GraphQL __typename.
func FieldValueString(fv map[string]any) string {
	switch jsonutil.Str(fv, "__typename") {
	case "ProjectV2ItemFieldTextValue":
		return jsonutil.Str(fv, "text")
	case "ProjectV2ItemFieldNumberValue":
		return jsonutil.Str(fv, "number")
	case "ProjectV2ItemFieldDateValue":
		return jsonutil.Str(fv, "date")
	case "ProjectV2ItemFieldSingleSelectValue":
		return jsonutil.Str(fv, "name")
	case "ProjectV2ItemFieldIterationValue":
		return jsonutil.Str(fv, "title")
	}
	return ""
}

// OwnerLogin returns the login of a project's owner object.
func OwnerLogin(p map[string]any) string {
	o := jsonutil.Map(p, "owner")
	if o == nil {
		return ""
	}
	return jsonutil.Str(o, "login")
}

// ExtractItemFields flattens a project item's fieldValues into name → string
// pairs (empty values dropped).
func ExtractItemFields(item map[string]any) map[string]string {
	out := map[string]string{}
	fvMap := jsonutil.Map(item, "fieldValues")
	for _, fv := range jsonutil.List(fvMap, "nodes") {
		fvm, _ := fv.(map[string]any)
		if fvm == nil {
			continue
		}
		fld := jsonutil.Map(fvm, "field")
		if fld == nil {
			continue
		}
		name := jsonutil.Str(fld, "name")
		if name == "" {
			continue
		}
		if v := FieldValueString(fvm); v != "" {
			out[name] = v
		}
	}
	return out
}

// --- Filename helpers ---

// PadWidth returns the zero-pad width for issue/PR/discussion/project filenames
// given the largest number in play.
func PadWidth(maxNumber int64) int {
	switch {
	case maxNumber >= 100000:
		return 6
	case maxNumber >= 10000:
		return 5
	default:
		return 4
	}
}

// IssueFilename renders the markdown filename for a numbered entity.
func IssueFilename(number int64, pad int) string {
	return fmt.Sprintf("%0*d.md", pad, number)
}

// SafeTag turns a release tag into a filesystem-safe base name.
func SafeTag(tag string) string {
	return strings.ReplaceAll(tag, "/", "-")
}
