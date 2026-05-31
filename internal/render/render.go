// Package render writes the on-demand markdown views from data already held in
// the store. It is pure presentation: every function takes rehydrated maps or
// domain structs and produces the same YAML-frontmatter markdown the tool has
// always emitted. It performs no network or database access.
package render

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/mevdschee/github-export/internal/document"
	"github.com/mevdschee/github-export/internal/ghmodel"
	"github.com/mevdschee/github-export/internal/jsonutil"
)

// IssueFile writes a single issue or PR markdown file: frontmatter, body, then
// one sub-document per timeline entry.
func IssueFile(path string, issue map[string]any, isPR bool, pr map[string]any, timeline []map[string]any, projects []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := &document.Writer{}
	d.KV("number", jsonutil.Int(issue, "number"))
	d.KV("title", jsonutil.Str(issue, "title"))
	if isPR {
		d.KV("type", "pull_request")
	}
	d.KV("state", jsonutil.Str(issue, "state"))
	if reason := jsonutil.Str(issue, "state_reason"); reason != "" {
		d.KV("state_reason", reason)
	}
	if isPR && pr != nil && jsonutil.Bool(pr, "draft") {
		d.KV("draft", true)
	}
	if jsonutil.Bool(issue, "locked") {
		d.KV("locked", true)
	}
	d.KV("created_at", jsonutil.Str(issue, "created_at"))
	d.KV("updated_at", jsonutil.Str(issue, "updated_at"))
	d.KV("closed_at", jsonutil.Str(issue, "closed_at"))
	d.KV("author", jsonutil.UserLogin(issue, "user"))
	d.List("assignees", jsonutil.Logins(issue, "assignees"))
	d.List("labels", jsonutil.LabelNames(issue, "labels"))
	if ms := jsonutil.Map(issue, "milestone"); ms != nil {
		d.KV("milestone", jsonutil.Str(ms, "title"))
	}
	if len(projects) > 0 {
		d.List("projects", projects)
	}

	if isPR && pr != nil {
		if head := jsonutil.Map(pr, "head"); head != nil {
			d.KV("source_branch", jsonutil.Str(head, "ref"))
		}
		if base := jsonutil.Map(pr, "base"); base != nil {
			d.KV("target_branch", jsonutil.Str(base, "ref"))
		}
		head := jsonutil.Map(pr, "head")
		base := jsonutil.Map(pr, "base")
		if head != nil && base != nil {
			headRepo := jsonutil.Map(head, "repo")
			baseRepo := jsonutil.Map(base, "repo")
			if headRepo != nil && baseRepo != nil {
				hf := jsonutil.Str(headRepo, "full_name")
				bf := jsonutil.Str(baseRepo, "full_name")
				if hf != "" && bf != "" && hf != bf {
					d.KV("source_repo", hf)
				}
			}
		}

		merged := jsonutil.Bool(pr, "merged")
		fmt.Fprint(d.Buf(), "merge:\n")
		d.KVIndent("  ", "merged", merged)
		if merged {
			d.KVIndent("  ", "merged_at", jsonutil.Str(pr, "merged_at"))
			mergedBy := jsonutil.UserLogin(pr, "merged_by")
			if mergedBy == "" {
				for _, ev := range timeline {
					if jsonutil.Str(ev, "event") == "merged" {
						mergedBy = jsonutil.UserLogin(ev, "actor")
						break
					}
				}
			}
			if mergedBy != "" {
				d.KVIndent("  ", "merged_by", mergedBy)
			}
			d.KVIndent("  ", "commit_sha", jsonutil.Str(pr, "merge_commit_sha"))
		}

		reviewerSet := map[string]bool{}
		for _, ev := range timeline {
			if jsonutil.Str(ev, "event") == "reviewed" {
				if login := jsonutil.UserLogin(ev, "user"); login != "" {
					reviewerSet[login] = true
				}
			}
		}
		if len(reviewerSet) > 0 {
			var reviewers []string
			for login := range reviewerSet {
				reviewers = append(reviewers, login)
			}
			sort.Strings(reviewers)
			d.List("reviewers", reviewers)
		}
		d.List("requested_reviewers", jsonutil.Logins(pr, "requested_reviewers"))
	}

	if reactions := buildReactions(issue); len(reactions) > 0 {
		fmt.Fprint(d.Buf(), "reactions:\n")
		for _, r := range reactions {
			fmt.Fprintf(d.Buf(), "  %s: %d\n", document.YamlScalar(r.key), r.count)
		}
	}

	document.WriteFirstDoc(f, d.String(), jsonutil.Str(issue, "body"))

	for _, event := range timeline {
		writeTimelineDoc(f, event)
	}
	return nil
}

type reaction struct {
	key   string
	count int
}

func buildReactions(issue map[string]any) []reaction {
	r := jsonutil.Map(issue, "reactions")
	if r == nil {
		return nil
	}
	keys := []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}
	var out []reaction
	for _, k := range keys {
		if count := int(jsonutil.Int(r, k)); count > 0 {
			out = append(out, reaction{k, count})
		}
	}
	return out
}

func writeTimelineDoc(w io.Writer, event map[string]any) {
	eventType := jsonutil.Str(event, "event")

	switch eventType {
	case "commented":
		d := &document.Writer{}
		d.KV("document", "comment")
		d.KV("id", jsonutil.Int(event, "id"))
		d.KV("author", jsonutil.UserLogin(event, "user"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		document.WriteSubDoc(w, d.String(), jsonutil.Str(event, "body"))

	case "reviewed":
		d := &document.Writer{}
		d.KV("document", "review")
		d.KV("id", jsonutil.Int(event, "id"))
		d.KV("author", jsonutil.UserLogin(event, "user"))
		d.KV("state", strings.ToLower(jsonutil.Str(event, "state")))
		d.KV("commit_sha", jsonutil.Str(event, "commit_id"))
		d.KV("submitted_at", jsonutil.Str(event, "submitted_at"))
		document.WriteSubDoc(w, d.String(), jsonutil.Str(event, "body"))

	case "line-commented":
		comments := jsonutil.List(event, "comments")
		for _, c := range comments {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			d := &document.Writer{}
			d.KV("document", "review_comment")
			d.KV("id", jsonutil.Int(cm, "id"))
			if rid := jsonutil.Int(cm, "pull_request_review_id"); rid > 0 {
				d.KV("review_id", rid)
			}
			d.KV("author", jsonutil.UserLogin(cm, "user"))
			d.KV("created_at", jsonutil.Str(cm, "created_at"))
			d.KV("path", jsonutil.Str(cm, "path"))
			if line := jsonutil.Int(cm, "line"); line > 0 {
				d.KV("line", line)
			} else if origLine := jsonutil.Int(cm, "original_line"); origLine > 0 {
				d.KV("line", origLine)
			}
			if side := jsonutil.Str(cm, "side"); side != "" {
				d.KV("side", side)
			}
			d.KV("commit_sha", jsonutil.Str(cm, "commit_id"))
			document.WriteSubDoc(w, d.String(), jsonutil.Str(cm, "body"))
		}

	case "labeled", "unlabeled":
		label := jsonutil.Map(event, "label")
		if label == nil {
			return
		}
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		d.KV("label", jsonutil.Str(label, "name"))
		document.WriteSubDoc(w, d.String(), "")

	case "assigned", "unassigned":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		d.KV("assignee", jsonutil.UserLogin(event, "assignee"))
		document.WriteSubDoc(w, d.String(), "")

	case "milestoned", "demilestoned":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if ms := jsonutil.Map(event, "milestone"); ms != nil {
			d.KV("milestone", jsonutil.Str(ms, "title"))
		}
		document.WriteSubDoc(w, d.String(), "")

	case "renamed":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if rename := jsonutil.Map(event, "rename"); rename != nil {
			d.KV("from", jsonutil.Str(rename, "from"))
			d.KV("to", jsonutil.Str(rename, "to"))
		}
		document.WriteSubDoc(w, d.String(), "")

	case "cross-referenced":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if source := jsonutil.Map(event, "source"); source != nil {
			if si := jsonutil.Map(source, "issue"); si != nil {
				if n := jsonutil.Int(si, "number"); n > 0 {
					d.KV("source_number", n)
				}
				if sr := jsonutil.Map(si, "repository"); sr != nil {
					d.KV("source_repo", jsonutil.Str(sr, "full_name"))
				}
			}
		}
		document.WriteSubDoc(w, d.String(), "")

	case "committed":
		return

	case "closed", "reopened", "merged", "referenced",
		"locked", "unlocked",
		"head_ref_force_pushed", "head_ref_deleted", "base_ref_changed",
		"converted_to_draft", "ready_for_review",
		"review_requested", "review_request_removed",
		"review_dismissed",
		"pinned", "unpinned", "transferred",
		"connected", "disconnected",
		"marked_as_duplicate", "unmarked_as_duplicate":
		d := &document.Writer{}
		d.KV("document", "event")
		d.KV("event", eventType)
		d.KV("actor", jsonutil.UserLogin(event, "actor"))
		d.KV("created_at", jsonutil.Str(event, "created_at"))
		if sha := jsonutil.Str(event, "commit_id"); sha != "" {
			d.KV("commit_sha", sha)
		}
		if reviewer := jsonutil.UserLogin(event, "requested_reviewer"); reviewer != "" {
			d.KV("reviewer", reviewer)
		}
		if reason := jsonutil.Str(event, "lock_reason"); reason != "" {
			d.KV("lock_reason", reason)
		}
		if dismissal := jsonutil.Str(event, "dismissal_message"); dismissal != "" {
			d.KV("dismissal_message", dismissal)
		}
		document.WriteSubDoc(w, d.String(), "")

	default:
		if eventType != "" {
			d := &document.Writer{}
			d.KV("document", "event")
			d.KV("event", eventType)
			d.KV("actor", jsonutil.UserLogin(event, "actor"))
			d.KV("created_at", jsonutil.Str(event, "created_at"))
			document.WriteSubDoc(w, d.String(), "")
		}
	}
}

// ReleaseFile writes a single release markdown file.
func ReleaseFile(path string, m map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := &document.Writer{}
	d.KV("tag", jsonutil.Str(m, "tag_name"))
	d.KV("name", jsonutil.Str(m, "name"))
	d.KV("draft", jsonutil.Bool(m, "draft"))
	d.KV("prerelease", jsonutil.Bool(m, "prerelease"))
	d.KV("author", jsonutil.UserLogin(m, "author"))
	d.KV("created_at", jsonutil.Str(m, "created_at"))
	d.KV("published_at", jsonutil.Str(m, "published_at"))
	d.KV("target_commitish", jsonutil.Str(m, "target_commitish"))

	assets := jsonutil.List(m, "assets")
	if len(assets) > 0 {
		fmt.Fprint(d.Buf(), "assets:\n")
		for _, a := range assets {
			asset, _ := a.(map[string]any)
			if asset == nil {
				continue
			}
			fmt.Fprintf(d.Buf(), "  - name: %s\n", document.YamlScalar(jsonutil.Str(asset, "name")))
			if ct := jsonutil.Str(asset, "content_type"); ct != "" {
				fmt.Fprintf(d.Buf(), "    content_type: %s\n", document.YamlScalar(ct))
			}
			if sz := jsonutil.Int(asset, "size"); sz > 0 {
				fmt.Fprintf(d.Buf(), "    size_bytes: %d\n", sz)
			}
			if dc := jsonutil.Int(asset, "download_count"); dc > 0 {
				fmt.Fprintf(d.Buf(), "    download_count: %d\n", dc)
			}
		}
	}

	document.WriteFirstDoc(f, d.String(), jsonutil.Str(m, "body"))
	return nil
}

// ProjectFile writes a project markdown file with its item sub-documents.
func ProjectFile(path string, p map[string]any, items []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	d := &document.Writer{}
	d.KV("number", jsonutil.Int(p, "number"))
	d.KV("title", jsonutil.Str(p, "title"))
	d.KV("state", "open")
	if jsonutil.Bool(p, "public") {
		d.KV("public", true)
	}
	d.KV("url", jsonutil.Str(p, "url"))
	d.KV("owner", ghmodel.OwnerLogin(p))
	if desc := jsonutil.Str(p, "shortDescription"); desc != "" {
		d.KV("description", desc)
	}
	d.KV("created_at", jsonutil.Str(p, "createdAt"))
	d.KV("updated_at", jsonutil.Str(p, "updatedAt"))

	fieldsMap := jsonutil.Map(p, "fields")
	fieldNodes := jsonutil.List(fieldsMap, "nodes")
	if len(fieldNodes) > 0 {
		fmt.Fprint(d.Buf(), "fields:\n")
		for _, fn := range fieldNodes {
			fm, _ := fn.(map[string]any)
			if fm == nil {
				continue
			}
			name := jsonutil.Str(fm, "name")
			if name == "" {
				continue
			}
			fmt.Fprintf(d.Buf(), "  - name: %s\n", document.YamlScalar(name))
			if dt := jsonutil.Str(fm, "dataType"); dt != "" {
				fmt.Fprintf(d.Buf(), "    type: %s\n", document.YamlScalar(dt))
			}
			if opts := jsonutil.List(fm, "options"); len(opts) > 0 {
				fmt.Fprint(d.Buf(), "    options:\n")
				for _, o := range opts {
					om, _ := o.(map[string]any)
					if om == nil {
						continue
					}
					if optName := jsonutil.Str(om, "name"); optName != "" {
						fmt.Fprintf(d.Buf(), "      - %s\n", document.YamlScalar(optName))
					}
				}
			}
		}
	}

	document.WriteFirstDoc(f, d.String(), jsonutil.Str(p, "readme"))

	for _, item := range items {
		writeProjectItem(f, item)
	}
	return nil
}

func writeProjectItem(f io.Writer, item map[string]any) {
	content := jsonutil.Map(item, "content")
	if content == nil {
		return
	}
	var itemType string
	switch jsonutil.Str(content, "__typename") {
	case "Issue":
		itemType = "issue"
	case "PullRequest":
		itemType = "pull_request"
	default:
		return
	}

	d := &document.Writer{}
	d.KV("document", "item")
	d.KV("type", itemType)
	d.KV("number", jsonutil.Int(content, "number"))
	d.KV("title", jsonutil.Str(content, "title"))
	if r := jsonutil.Map(content, "repository"); r != nil {
		d.KV("repo", jsonutil.Str(r, "nameWithOwner"))
	}

	fvMap := jsonutil.Map(item, "fieldValues")
	fvNodes := jsonutil.List(fvMap, "nodes")
	var pairs [][2]string
	for _, fv := range fvNodes {
		fvm, _ := fv.(map[string]any)
		if fvm == nil {
			continue
		}
		fieldName := ""
		if fld := jsonutil.Map(fvm, "field"); fld != nil {
			fieldName = jsonutil.Str(fld, "name")
		}
		if fieldName == "" {
			continue
		}
		value := ghmodel.FieldValueString(fvm)
		if value == "" {
			continue
		}
		pairs = append(pairs, [2]string{fieldName, value})
	}
	if len(pairs) > 0 {
		sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
		fmt.Fprint(d.Buf(), "fields:\n")
		for _, kv := range pairs {
			fmt.Fprintf(d.Buf(), "  %s: %s\n", document.YamlScalar(kv[0]), document.YamlScalar(kv[1]))
		}
	}

	document.WriteSubDoc(f, d.String(), "")
}

// DiscussionFile writes a discussion markdown file with comment and reply
// sub-documents.
func DiscussionFile(path string, d ghmodel.DiscussionNode) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := &document.Writer{}
	w.KV("number", d.Number)
	w.KV("title", d.Title)
	w.KV("type", "discussion")
	w.KV("state", ghmodel.DiscussionState(d))
	if d.StateReason != "" {
		w.KV("state_reason", strings.ToLower(d.StateReason))
	}
	if d.Locked {
		w.KV("locked", true)
	}
	w.KV("created_at", d.CreatedAt)
	w.KV("updated_at", d.UpdatedAt)
	if d.ClosedAt != "" {
		w.KV("closed_at", d.ClosedAt)
	}
	w.KV("author", d.Author.Login)
	if d.Category.Name != "" {
		w.KV("category", d.Category.Name)
	}
	var labels []string
	for _, l := range d.Labels.Nodes {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	w.List("labels", labels)
	if d.Answer != nil {
		w.KV("answer_id", d.Answer.DatabaseID)
		if d.AnswerChosenAt != "" {
			w.KV("answer_chosen_at", d.AnswerChosenAt)
		}
		if d.AnswerChosenBy != nil && d.AnswerChosenBy.Login != "" {
			w.KV("answer_chosen_by", d.AnswerChosenBy.Login)
		}
	}

	document.WriteFirstDoc(f, w.String(), d.Body)

	answerID := d.AnswerID()
	for _, c := range d.Comments.Nodes {
		cw := &document.Writer{}
		cw.KV("document", "comment")
		cw.KV("id", c.DatabaseID)
		cw.KV("author", c.Author.Login)
		cw.KV("created_at", c.CreatedAt)
		if answerID != 0 && c.DatabaseID == answerID {
			cw.KV("is_answer", true)
		}
		document.WriteSubDoc(f, cw.String(), c.Body)

		for _, r := range c.Replies.Nodes {
			rw := &document.Writer{}
			rw.KV("document", "reply")
			rw.KV("id", r.DatabaseID)
			rw.KV("parent_id", c.DatabaseID)
			rw.KV("author", r.Author.Login)
			rw.KV("created_at", r.CreatedAt)
			document.WriteSubDoc(f, rw.String(), r.Body)
		}
	}
	return nil
}
