package main

// store.go — port of bot/store.ts. All surfaces read and write GlobalStore,
// which is declared (and initially seeded) by another file in this package.
// Empty strings are used for TS `undefined`/`null` where the shared structs
// use plain strings (Pull.MergedAt, Pull.MergeCommitSha, Issue.Cache, and the
// commit/ref oid parameters).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var Viewer = User{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid", Type: "User"}
var Teammate = User{ID: 1001, Login: "quinnkeller", Name: "quinn keller", Email: "quinnkeller@example.invalid", Type: "User"}
var Stranger = User{ID: 1002, Login: "olgabauer", Name: "olga bauer", Email: "olgabauer@example.invalid", Type: "User"}

// ensureForgeStore fills in the TS initializer values if the file declaring
// GlobalStore left them at their Go zero values.
func ensureForgeStore() {
	if GlobalStore.Repos == nil {
		GlobalStore.Repos = make(map[string]*Repo)
	}
	if GlobalStore.CommentSeq == 0 {
		GlobalStore.CommentSeq = 1000
	}
	if GlobalStore.RepoSeq == 0 {
		GlobalStore.RepoSeq = 5000
	}
	if GlobalStore.IssueIDSeq == 0 {
		GlobalStore.IssueIDSeq = 200000
	}
	if GlobalStore.PullIDSeq == 0 {
		GlobalStore.PullIDSeq = 300000
	}
	if GlobalStore.LabelSeq == 0 {
		GlobalStore.LabelSeq = 1
	}
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func findRepo(owner, name string) *Repo {
	ensureForgeStore()
	return GlobalStore.Repos[repoKey(owner, name)]
}

func userOf(login string) User {
	lower := strings.ToLower(login)
	if lower == Viewer.Login {
		return Viewer
	}
	if lower == Teammate.Login {
		return Teammate
	}
	if lower == Stranger.Login {
		return Stranger
	}
	return User{ID: 1003, Login: login, Name: login, Email: login + "@example.invalid", Type: "User"}
}

func createRepo(owner, name, desc string, isPrivate bool) *Repo {
	ensureForgeStore()
	GlobalStore.RepoSeq++
	now := iso(tick())
	repo := &Repo{
		ID:            GlobalStore.RepoSeq,
		Owner:         owner,
		Name:          name,
		Description:   desc,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		Objects:       make(map[string]GitObject),
		Refs:          make(map[string]string),
		Labels:        make(map[string]Label),
		Issues:        make([]*Issue, 0),
		Pulls:         make([]*Pull, 0),
		Comments:      make(map[int][]Comment),
	}
	GlobalStore.Repos[repoKey(owner, name)] = repo
	return repo
}

func deleteRepo(repo *Repo) {
	if repo == nil {
		return
	}
	ensureForgeStore()
	delete(GlobalStore.Repos, repoKey(repo.Owner, repo.Name))
}

func repoJSON(repo *Repo) map[string]interface{} {
	openIssues := 0
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			openIssues++
		}
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	owner := userOf(repo.Owner)
	return map[string]interface{}{
		"id":                repo.ID,
		"node_id":           fmt.Sprintf("R_%d", repo.ID),
		"name":              repo.Name,
		"full_name":         repo.Owner + "/" + repo.Name,
		"private":           repo.Private,
		"owner":             map[string]interface{}{"login": repo.Owner, "id": owner.ID, "type": "User"},
		"description":       repo.Description,
		"default_branch":    repo.DefaultBranch,
		"created_at":        repo.CreatedAt,
		"updated_at":        repo.UpdatedAt,
		"pushed_at":         repo.UpdatedAt,
		"has_issues":        true,
		"has_wiki":          false,
		"has_projects":      false,
		"open_issues_count": openIssues,
		"forks_count":       0,
		"stargazers_count":  0,
		"watchers_count":    0,
		"language":          nil,
		"archived":          false,
		"disabled":          false,
		"visibility":        visibility,
		"url":               fmt.Sprintf("/api/v3/repos/%s/%s", repo.Owner, repo.Name),
		"html_url":          fmt.Sprintf("http://localhost/%s/%s", repo.Owner, repo.Name),
	}
}

// ---------------------------------------------------------------- git objects and refs

func putObject(repo *Repo, typ string, data []byte) string {
	if repo.Objects == nil {
		repo.Objects = make(map[string]GitObject)
	}
	oid := hashObject(typ, data)
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

func objectAt(repo *Repo, oid string) (GitObject, bool) {
	obj, ok := repo.Objects[oid]
	return obj, ok
}

func refOID(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func defaultTip(repo *Repo) (string, bool) {
	return refOID(repo, "refs/heads/"+repo.DefaultBranch)
}

// commitOf peels a ref/oid to a commit, following at most eight annotated tags.
func commitOf(repo *Repo, oid string) (string, bool) {
	current := oid
	for hop := 0; hop < 8; hop++ {
		object, ok := repo.Objects[current]
		if !ok {
			return "", false
		}
		if object.Type == "commit" {
			return current, true
		}
		if object.Type == "tag" {
			current = parseTag(object.Data).Object
			continue
		}
		return "", false
	}
	return "", false
}

func commitTree(repo *Repo, commitOID string) (string, bool) {
	object, ok := repo.Objects[commitOID]
	if !ok || object.Type != "commit" {
		return "", false
	}
	return parseCommit(object.Data).Tree, true
}

// filesAtCommit returns the flattened file map at a commit. An empty commitOID
// stands in for TS `undefined` and yields an empty map.
func filesAtCommit(repo *Repo, commitOID string) map[string]FlatEntry {
	out := make(map[string]FlatEntry)
	if commitOID == "" {
		return out
	}
	tree, ok := commitTree(repo, commitOID)
	if !ok {
		return out
	}
	return flattenTree(repo.Objects, tree, "", out, 0)
}

func filesAtRef(repo *Repo, ref string) map[string]FlatEntry {
	refName := ref
	if !strings.HasPrefix(ref, "refs/") {
		refName = "refs/heads/" + ref
	}
	oid := repo.Refs[refName]
	commit := ""
	if oid != "" {
		if peeled, ok := commitOf(repo, oid); ok {
			commit = peeled
		}
	}
	return filesAtCommit(repo, commit)
}

// makeCommit signature:
//
//	func makeCommit(repo *Repo, files map[string]FlatEntry, parents []string, message string, author ...User) string
//
// The variadic author mirrors the TS default (VIEWER): omit it (or pass Viewer)
// for the standard identity. The tree is derived from files with
// buildTreeFromFlat; both the author and committer lines use the same
// gitIdent-style string built from tick(), exactly like store.ts.
func makeCommit(repo *Repo, files map[string]FlatEntry, parents []string, message string, author ...User) string {
	tree := buildTreeFromFlat(repo.Objects, files)
	who := Viewer
	if len(author) > 0 {
		who = author[0]
	}
	ident := gitIdent(who.Name, who.Email, tick())
	data := buildCommit(CommitFields{
		Tree:      tree,
		Parents:   parents,
		Author:    ident,
		Committer: ident,
		Message:   message,
	})
	return putObject(repo, "commit", data)
}

func isAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if parent == ancestor {
				return true
			}
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return false
}

// commitAncestors returns every oid reachable from start (including start);
// when stopAt contains an oid, that oid is recorded but its parents are not followed.
func commitAncestors(repo *Repo, start string, stopAt map[string]bool) map[string]bool {
	seen := make(map[string]bool)
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		if stopAt[oid] {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return seen
}

// commitLog returns []map with "sha" and "view" keys. "view" is a map with the
// lowercase CommitView keys used by TS: tree, parents, author, committer,
// message, headers. tip == "" is TS undefined; limit <= 0 yields no entries.
func commitLog(repo *Repo, tip string, limit int) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	if tip == "" {
		return out
	}
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		view := parseCommit(object.Data)
		out = append(out, map[string]interface{}{
			"sha": oid,
			"view": map[string]interface{}{
				"tree":      view.Tree,
				"parents":   view.Parents,
				"author":    view.Author,
				"committer": view.Committer,
				"message":   view.Message,
				"headers":   view.Headers,
			},
		})
		for _, parent := range view.Parents {
			if !seen[parent] {
				queue = append(queue, parent)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- issues, labels, comments

func createIssue(repo *Repo, title, body string, labels []Label) *Issue {
	ensureForgeStore()
	repo.IssueSeq++
	GlobalStore.IssueIDSeq++
	now := iso(tick())
	copied := make([]Label, len(labels))
	copy(copied, labels)
	issue := &Issue{
		ID:        GlobalStore.IssueIDSeq,
		Number:    repo.IssueSeq,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    copied,
		CreatedAt: now,
		UpdatedAt: now,
		Comments:  0,
	}
	repo.Issues = append(repo.Issues, issue)
	return issue
}

func labelJSON(label Label) map[string]interface{} {
	return map[string]interface{}{
		"id":          label.ID,
		"name":        label.Name,
		"color":       label.Color,
		"description": label.Description,
		"default":     false,
	}
}

func issueJSON(issue *Issue, repo *Repo) map[string]interface{} {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, labelJSON(label))
	}
	closed := issue.State == "closed"
	stateReason := interface{}(nil)
	closedAt := interface{}(nil)
	if closed {
		stateReason = "completed"
		closedAt = issue.UpdatedAt
	}
	return map[string]interface{}{
		"id":             issue.ID,
		"node_id":        fmt.Sprintf("I_%d", issue.ID),
		"number":         issue.Number,
		"title":          issue.Title,
		"body":           issue.Body,
		"state":          issue.State,
		"state_reason":   stateReason,
		"locked":         false,
		"labels":         labels,
		"user":           map[string]interface{}{"login": Viewer.Login, "id": Viewer.ID, "type": "User"},
		"comments":       issue.Comments,
		"created_at":     issue.CreatedAt,
		"updated_at":     issue.UpdatedAt,
		"closed_at":      closedAt,
		"html_url":       fmt.Sprintf("http://localhost/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
		"url":            fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
		"repository_url": fmt.Sprintf("/api/v3/repos/%s/%s", repo.Owner, repo.Name),
	}
}

// jsJSONQuote produces exactly what JSON.stringify produces for a JS string:
// unlike encoding/json it does not escape <, >, &, U+2028 or U+2029.
func jsJSONQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, c))
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// issueJSONText emits the exact REST JSON string from store.ts (no HTML escaping).
func issueJSONText(issue *Issue, repo *Repo) string {
	var labels strings.Builder
	labels.WriteByte('[')
	for i, label := range issue.Labels {
		if i > 0 {
			labels.WriteByte(',')
		}
		labels.WriteString(`{"id":`)
		labels.WriteString(strconv.Itoa(label.ID))
		labels.WriteString(`,"name":`)
		labels.WriteString(jsJSONQuote(label.Name))
		labels.WriteString(`,"color":`)
		labels.WriteString(jsJSONQuote(label.Color))
		labels.WriteString(`,"description":`)
		labels.WriteString(jsJSONQuote(label.Description))
		labels.WriteString(`,"default":false}`)
	}
	labels.WriteByte(']')

	closed := issue.State == "closed"
	stateReason := "null"
	closedAt := "null"
	if closed {
		stateReason = `"completed"`
		closedAt = jsJSONQuote(issue.UpdatedAt)
	}

	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(issue.ID))
	b.WriteString(`,"node_id":`)
	b.WriteString(jsJSONQuote(fmt.Sprintf("I_%d", issue.ID)))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(issue.Number))
	b.WriteString(`,"title":`)
	b.WriteString(jsJSONQuote(issue.Title))
	b.WriteString(`,"body":`)
	b.WriteString(jsJSONQuote(issue.Body))
	b.WriteString(`,"state":`)
	b.WriteString(jsJSONQuote(issue.State))
	b.WriteString(`,"state_reason":`)
	b.WriteString(stateReason)
	b.WriteString(`,"locked":false,"labels":`)
	b.WriteString(labels.String())
	b.WriteString(`,"user":{"login":`)
	b.WriteString(jsJSONQuote(Viewer.Login))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(Viewer.ID))
	b.WriteString(`,"type":"User"},"comments":`)
	b.WriteString(strconv.Itoa(issue.Comments))
	b.WriteString(`,"created_at":`)
	b.WriteString(jsJSONQuote(issue.CreatedAt))
	b.WriteString(`,"updated_at":`)
	b.WriteString(jsJSONQuote(issue.UpdatedAt))
	b.WriteString(`,"closed_at":`)
	b.WriteString(closedAt)
	b.WriteString(`,"html_url":`)
	b.WriteString(jsJSONQuote(fmt.Sprintf("http://localhost/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)))
	b.WriteString(`,"url":`)
	b.WriteString(jsJSONQuote(fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)))
	b.WriteString(`,"repository_url":`)
	b.WriteString(jsJSONQuote(fmt.Sprintf("/api/v3/repos/%s/%s", repo.Owner, repo.Name)))
	b.WriteString(`}`)
	return b.String()
}

// issueString caches issueJSONText + "\n"; an empty Cache is TS undefined.
func issueString(issue *Issue, repo *Repo) string {
	if issue.Cache != "" {
		return issue.Cache
	}
	issue.Cache = issueJSONText(issue, repo) + "\n"
	return issue.Cache
}

func addComment(repo *Repo, issue *Issue, body string) Comment {
	ensureForgeStore()
	GlobalStore.CommentSeq++
	now := iso(tick())
	comment := Comment{ID: GlobalStore.CommentSeq, Body: body, CreatedAt: now, UpdatedAt: now, User: Viewer}
	if repo.Comments == nil {
		repo.Comments = make(map[int][]Comment)
	}
	list := append(repo.Comments[issue.ID], comment)
	repo.Comments[issue.ID] = list
	issue.Comments = len(list)
	issue.UpdatedAt = now
	issue.Cache = ""
	return comment
}

func commentJSON(c Comment) map[string]interface{} {
	return map[string]interface{}{
		"id":         c.ID,
		"node_id":    fmt.Sprintf("IC_%d", c.ID),
		"body":       c.Body,
		"user":       map[string]interface{}{"login": c.User.Login, "id": c.User.ID, "type": "User"},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
		"html_url":   "",
	}
}

// ---------------------------------------------------------------- pull requests

// createPull returns (pull, "") on success, or (nil, errorMessage) for the two
// TS error branches. This replaces the TS `Pull | string` union.
func createPull(repo *Repo, title, body, head, base string) (*Pull, string) {
	headOID, ok := repo.Refs["refs/heads/"+head]
	if !ok {
		return nil, fmt.Sprintf("No commits between %s and %s", base, head)
	}
	baseOID, ok := repo.Refs["refs/heads/"+base]
	if !ok {
		return nil, fmt.Sprintf("Base branch %s does not exist", base)
	}
	ensureForgeStore()
	repo.PullSeq++
	GlobalStore.PullIDSeq++
	now := iso(tick())
	pull := &Pull{
		ID:             GlobalStore.PullIDSeq,
		Number:         repo.PullSeq,
		Title:          title,
		Body:           body,
		State:          "open",
		Merged:         false,
		MergedAt:       "", // TS null
		MergeCommitSha: "", // TS null
		HeadRef:        head,
		BaseRef:        base,
		HeadSha:        headOID,
		BaseSha:        baseOID,
		CreatedAt:      now,
		UpdatedAt:      now,
		User:           Viewer,
	}
	repo.Pulls = append(repo.Pulls, pull)
	return pull, ""
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func pullJSON(p *Pull, repo *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id":               p.ID,
		"node_id":          fmt.Sprintf("PR_%d", p.ID),
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"merged":           p.Merged,
		"merged_at":        nullableString(p.MergedAt),
		"merge_commit_sha": nullableString(p.MergeCommitSha),
		"head":             map[string]interface{}{"ref": p.HeadRef, "sha": p.HeadSha, "label": repo.Owner + ":" + p.HeadRef},
		"base":             map[string]interface{}{"ref": p.BaseRef, "sha": p.BaseSha, "label": repo.Owner + ":" + p.BaseRef},
		"user":             map[string]interface{}{"login": Viewer.Login, "id": Viewer.ID, "type": "User"},
		"created_at":       p.CreatedAt,
		"updated_at":       p.UpdatedAt,
		"draft":            false,
		"html_url":         fmt.Sprintf("http://localhost/%s/%s/pull/%d", repo.Owner, repo.Name, p.Number),
		"url":              fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", repo.Owner, repo.Name, p.Number),
	}
}

// diffFiles compares two commits ("" == TS undefined).
//
// Signature/struct note: the shared DiffFile has no `changes` or `sha` field.
// To preserve the blob id, DiffFile.Patch carries the TS `sha` value (it is
// otherwise unused); `changes` is always additions+deletions and is not stored.
func diffFiles(repo *Repo, baseCommit, headCommit string) []DiffFile {
	base := filesAtCommit(repo, baseCommit)
	head := filesAtCommit(repo, headCommit)
	out := make([]DiffFile, 0)
	for path, entry := range head {
		before, exists := base[path]
		if exists && before.OID == entry.OID {
			continue
		}
		added := !exists
		status := "modified"
		deletions := 1
		if added {
			status = "added"
			deletions = 0
		}
		out = append(out, DiffFile{Filename: path, Status: status, Additions: 1, Deletions: deletions, Patch: entry.OID})
	}
	for path, entry := range base {
		if _, ok := head[path]; ok {
			continue
		}
		out = append(out, DiffFile{Filename: path, Status: "removed", Additions: 0, Deletions: 1, Patch: entry.OID})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return jsStringLess(out[i].Filename, out[j].Filename)
	})
	return out
}

func mergeBase(repo *Repo, a, b string) (string, bool) {
	ancestors := commitAncestors(repo, a, nil)
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestors[oid] {
			return oid, true
		}
		object, ok := repo.Objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(object.Data).Parents...)
	}
	return "", false
}

// mergePull returns (ok, status, message, sha). A successful merge returns
// status 200 with an empty message; failures mirror the TS MergeResult errors
// (405 + the exact TS message). The returned sha is the merge commit oid.
func mergePull(repo *Repo, p *Pull) (bool, int, string, string) {
	if p.Merged {
		return false, 405, "Pull request is not mergeable", ""
	}
	baseTip, ok := repo.Refs["refs/heads/"+p.BaseRef]
	if !ok {
		return false, 405, "Pull request is not mergeable", ""
	}
	headTip, ok := repo.Refs["refs/heads/"+p.HeadRef]
	if !ok {
		return false, 405, "Pull request is not mergeable", ""
	}
	baseCommit, ok := commitOf(repo, baseTip)
	if !ok {
		return false, 405, "Pull request is not mergeable", ""
	}
	headCommit, ok := commitOf(repo, headTip)
	if !ok {
		return false, 405, "Pull request is not mergeable", ""
	}
	common, _ := mergeBase(repo, baseCommit, headCommit)
	baseFiles := filesAtCommit(repo, baseCommit)
	headFiles := filesAtCommit(repo, headCommit)
	commonFiles := filesAtCommit(repo, common)
	result := make(map[string]FlatEntry, len(baseFiles))
	for path, entry := range baseFiles {
		result[path] = entry
	}
	for path, entry := range headFiles {
		before, hasBefore := commonFiles[path]
		if hasBefore && before.OID == entry.OID {
			continue
		}
		current, hasCurrent := baseFiles[path]
		if hasCurrent && current.OID != entry.OID && hasBefore && before.OID != current.OID {
			return false, 405, "Pull request has merge conflicts", ""
		}
		result[path] = entry
	}
	for path, commonEntry := range commonFiles {
		if _, inHead := headFiles[path]; inHead {
			continue
		}
		if current, inBase := baseFiles[path]; inBase && current.OID == commonEntry.OID {
			delete(result, path)
		}
	}
	sha := makeCommit(repo, result, []string{baseTip}, fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.HeadRef, p.Title))
	repo.Refs["refs/heads/"+p.BaseRef] = sha
	p.State = "closed"
	p.Merged = true
	p.MergedAt = iso(tick())
	p.MergeCommitSha = sha
	p.UpdatedAt = p.MergedAt
	return true, 200, "", sha
}
