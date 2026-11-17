package main

// GitHub REST v3 surface, ported from bot/rest.ts.
//
// Every reply is JSON (including errors) and carries x-github-request-id.
// Response objects are assembled with an insertion-ordered JSON writer so the
// emitted bytes keep the exact field order of the TypeScript object literals.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- ordered JSON

// restJObj serializes to a JSON object whose keys appear in insertion order.
type restJObj struct {
	keys []string
	vals []any
}

// restObj builds an ordered JSON object from alternating key, value arguments.
func restObj(pairs ...any) restJObj {
	o := restJObj{}
	for i := 0; i+1 < len(pairs); i += 2 {
		o.keys = append(o.keys, pairs[i].(string))
		o.vals = append(o.vals, pairs[i+1])
	}
	return o
}

func (o restJObj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := 0; i < len(o.keys); i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(o.keys[i])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(o.vals[i])
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// restJSONBody matches sendJson: compact JSON plus a trailing newline.
func restJSONBody(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}

func restHeaders() map[string]string {
	return map[string]string{"x-github-request-id": constantRequestID}
}

func restReply(status int, v any, extra map[string]string) HTTPResult {
	h := restHeaders()
	for k, val := range extra {
		h[k] = val
	}
	return HTTPResult{Status: status, ContentType: JSONType, Body: restJSONBody(v), Headers: h}
}

func restReplyText(status int, text string, extra map[string]string) HTTPResult {
	h := restHeaders()
	for k, val := range extra {
		h[k] = val
	}
	return HTTPResult{Status: status, ContentType: JSONType, Body: []byte(text), Headers: h}
}

func restEmpty(status int) HTTPResult {
	return HTTPResult{Status: status, ContentType: JSONType, Body: []byte{}, Headers: restHeaders()}
}

func restFail(status int, message string) HTTPResult {
	return restReply(status, restObj(
		"message", message,
		"documentation_url", "https://docs.github.com/rest",
	), nil)
}

// ---------------------------------------------------------------- small helpers

func restRepoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func restDecodeSegments(parts []string) []string {
	out := make([]string, len(parts))
	for i, s := range parts {
		out[i] = safeDecode(s)
	}
	return out
}

// restQueryGet mirrors URLSearchParams.get: absent yields fallback, present
// (even with an empty value) yields the first value.
func restQueryGet(query url.Values, key, fallback string) string {
	if vals, ok := query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return fallback
}

func restFirstQuery(query url.Values, keys ...string) (string, bool) {
	for _, key := range keys {
		if vals, ok := query[key]; ok && len(vals) > 0 {
			return vals[0], true
		}
	}
	return "", false
}

// restParseInt matches Number.parseInt(text, 10): leading whitespace and sign
// are accepted, parsing stops at the first non-digit, overflow is not finite.
func restParseInt(text string) (int, bool) {
	s := strings.TrimSpace(text)
	if s == "" {
		return 0, false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		// JavaScript's parseInt returns a large but finite number here.
		if numErr, ok := err.(*strconv.NumError); ok && numErr.Err == strconv.ErrRange {
			if strings.HasPrefix(s, "-") {
				return -restMaxInt, true
			}
			return restMaxInt, true
		}
		return 0, false
	}
	return int(n), true
}

var restMaxInt = int(^uint(0) >> 1)

func restPerPage(query url.Values) int {
	raw, ok := restParseInt(restQueryGet(query, "per_page", "30"))
	if !ok || raw <= 0 {
		return 30
	}
	if raw > 100 {
		return 100
	}
	return raw
}

func restPage(query url.Values) int {
	raw, ok := restParseInt(restQueryGet(query, "page", "1"))
	if !ok || raw <= 0 {
		return 1
	}
	return raw
}

func restLinkHeader(host, path string, query url.Values, page, perPage, total int) string {
	base := url.Values{}
	for key, vals := range query {
		for _, val := range vals {
			base.Add(key, val)
		}
	}
	base.Set("per_page", strconv.Itoa(perPage))
	urlFor := func(target int) string {
		p := url.Values{}
		for key, vals := range base {
			for _, val := range vals {
				p.Add(key, val)
			}
		}
		p.Set("page", strconv.Itoa(target))
		return "http://" + host + path + "?" + p.Encode()
	}
	last := 1
	if total > 0 {
		last = (total + perPage - 1) / perPage
		if last < 1 {
			last = 1
		}
	}
	links := make([]string, 0, 4)
	if page < last {
		links = append(links, "<"+urlFor(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		links = append(links, "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
	links = append(links, "<"+urlFor(last)+">; rel=\"last\"")
	return strings.Join(links, ", ")
}

func restParseBody(bodyText string) (map[string]any, bool) {
	if strings.TrimSpace(bodyText) == "" {
		return map[string]any{}, true
	}
	var value any
	if err := json.Unmarshal([]byte(bodyText), &value); err != nil {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	if !ok || value == nil {
		return nil, false
	}
	return obj, true
}

func restStr(obj map[string]any, key, fallback string) string {
	if v, ok := obj[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func restBool(obj map[string]any, key string, fallback bool) bool {
	if v, ok := obj[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

func restNullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func restTSString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	case map[string]any:
		return "[object Object]"
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, restTSString(item))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(v)
	}
}

func restUserIDOf(login string) int {
	if strings.EqualFold(login, Viewer.Login) {
		return Viewer.ID
	}
	if strings.EqualFold(login, Teammate.Login) {
		return Teammate.ID
	}
	if strings.EqualFold(login, Stranger.Login) {
		return Stranger.ID
	}
	return 1003
}

// ---------------------------------------------------------------- response shapes

func restUserBrief(login string) restJObj {
	return restObj("login", login, "id", restUserIDOf(login), "type", "User")
}

func restLabelObj(label Label) restJObj {
	return restObj(
		"id", label.ID,
		"name", label.Name,
		"color", label.Color,
		"description", label.Description,
		"default", false,
	)
}

func restLabelObjs(labels []Label) []any {
	out := make([]any, 0, len(labels))
	for _, label := range labels {
		out = append(out, restLabelObj(label))
	}
	return out
}

func restRepoObj(repo *Repo) restJObj {
	open := 0
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			open++
		}
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return restObj(
		"id", repo.ID,
		"node_id", "R_"+strconv.Itoa(repo.ID),
		"name", repo.Name,
		"full_name", repo.Owner+"/"+repo.Name,
		"private", repo.Private,
		"owner", restUserBrief(repo.Owner),
		"description", repo.Description,
		"default_branch", repo.DefaultBranch,
		"created_at", repo.CreatedAt,
		"updated_at", repo.UpdatedAt,
		"pushed_at", repo.UpdatedAt,
		"has_issues", true,
		"has_wiki", false,
		"has_projects", false,
		"open_issues_count", open,
		"forks_count", 0,
		"stargazers_count", 0,
		"watchers_count", 0,
		"language", nil,
		"archived", false,
		"disabled", false,
		"visibility", visibility,
		"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name,
		"html_url", "http://localhost/"+repo.Owner+"/"+repo.Name,
	)
}

func restIssueObj(issue *Issue, repo *Repo) restJObj {
	closed := issue.State == "closed"
	closedAt := any(nil)
	reason := any(nil)
	if closed {
		closedAt = issue.UpdatedAt
		reason = "completed"
	}
	base := "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return restObj(
		"id", issue.ID,
		"node_id", "I_"+strconv.Itoa(issue.ID),
		"number", issue.Number,
		"title", issue.Title,
		"body", issue.Body,
		"state", issue.State,
		"state_reason", reason,
		"locked", false,
		"labels", restLabelObjs(issue.Labels),
		"user", restUserBrief(Viewer.Login),
		"comments", issue.Comments,
		"created_at", issue.CreatedAt,
		"updated_at", issue.UpdatedAt,
		"closed_at", closedAt,
		"html_url", "http://localhost/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number),
		"url", base+"/issues/"+strconv.Itoa(issue.Number),
		"repository_url", base,
	)
}

func restIssueJSONText(issue *Issue, repo *Repo) string {
	return string(restJSONBody(restIssueObj(issue, repo)))
}

func restIssueString(issue *Issue, repo *Repo) string {
	if issue.Cache != "" {
		return issue.Cache
	}
	text := restIssueJSONText(issue, repo)
	issue.Cache = text
	return text
}

func restCommentObj(comment Comment) restJObj {
	return restObj(
		"id", comment.ID,
		"node_id", "IC_"+strconv.Itoa(comment.ID),
		"body", comment.Body,
		"user", restUserBrief(comment.User.Login),
		"created_at", comment.CreatedAt,
		"updated_at", comment.UpdatedAt,
		"html_url", "",
	)
}

func restPullObj(pull *Pull, repo *Repo) restJObj {
	num := strconv.Itoa(pull.Number)
	base := "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return restObj(
		"id", pull.ID,
		"node_id", "PR_"+strconv.Itoa(pull.ID),
		"number", pull.Number,
		"title", pull.Title,
		"body", pull.Body,
		"state", pull.State,
		"merged", pull.Merged,
		"merged_at", restNullable(pull.MergedAt),
		"merge_commit_sha", restNullable(pull.MergeCommitSha),
		"head", restObj("ref", pull.HeadRef, "sha", pull.HeadSha, "label", repo.Owner+":"+pull.HeadRef),
		"base", restObj("ref", pull.BaseRef, "sha", pull.BaseSha, "label", repo.Owner+":"+pull.BaseRef),
		"user", restUserBrief(Viewer.Login),
		"created_at", pull.CreatedAt,
		"updated_at", pull.UpdatedAt,
		"draft", false,
		"html_url", "http://localhost/"+repo.Owner+"/"+repo.Name+"/pull/"+num,
		"url", base+"/pulls/"+num,
	)
}

func restCommitJSON(sha, message string) restJObj {
	ident := restObj("name", Viewer.Name, "email", Viewer.Email, "date", iso(0))
	return restObj(
		"sha", sha,
		"node_id", "C_"+sha,
		"url", "",
		"html_url", "",
		"author", ident,
		"committer", ident,
		"message", message,
		"tree", restObj("sha", "", "url", ""),
		"parents", []any{},
	)
}

// ---------------------------------------------------------------- local store mutations
// These mirror store.ts exactly; keeping them here avoids dependence on the
// in-flight store.go helper signatures and preserves deterministic counters.

func restCreateIssue(repo *Repo, title, body string, labels []Label) *Issue {
	repo.IssueSeq++
	GlobalStore.IssueIDSeq++
	now := iso(tick())
	copied := append([]Label{}, labels...)
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

func restAddComment(repo *Repo, issue *Issue, body string) Comment {
	GlobalStore.CommentSeq++
	now := iso(tick())
	comment := Comment{ID: GlobalStore.CommentSeq, Body: body, CreatedAt: now, UpdatedAt: now, User: Viewer}
	list := append(repo.Comments[issue.ID], comment)
	repo.Comments[issue.ID] = list
	issue.Comments = len(list)
	issue.UpdatedAt = now
	issue.Cache = ""
	return comment
}

func restCreatePull(repo *Repo, title, body, head, base string) (*Pull, string) {
	headOID, ok := repo.Refs["refs/heads/"+head]
	if !ok {
		return nil, "No commits between " + base + " and " + head
	}
	baseOID, ok := repo.Refs["refs/heads/"+base]
	if !ok {
		return nil, "Base branch " + base + " does not exist"
	}
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
		MergedAt:       "",
		MergeCommitSha: "",
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

func restCommitAncestors(repo *Repo, start string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := repo.Objects[oid]
		if !ok || obj.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(obj.Data).Parents {
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return seen
}

func restMergeBase(repo *Repo, a, b string) string {
	ancestors := restCommitAncestors(repo, a)
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestors[oid] {
			return oid
		}
		obj, ok := repo.Objects[oid]
		if !ok || obj.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(obj.Data).Parents {
			queue = append(queue, parent)
		}
	}
	return ""
}

type restMergeResult struct {
	OK      bool
	SHA     string
	Status  int
	Message string
}

func restMergePull(repo *Repo, pull *Pull) restMergeResult {
	notMergeable := restMergeResult{Status: 405, Message: "Pull request is not mergeable"}
	if pull.Merged {
		return notMergeable
	}
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		return notMergeable
	}
	headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		return notMergeable
	}
	baseCommit, _ := commitOf(repo, baseTip)
	headCommit, _ := commitOf(repo, headTip)
	if baseCommit == "" || headCommit == "" {
		return notMergeable
	}
	common := restMergeBase(repo, baseCommit, headCommit)
	baseFiles := filesAtCommit(repo, baseCommit)
	headFiles := filesAtCommit(repo, headCommit)
	commonFiles := filesAtCommit(repo, common)
	result := map[string]FlatEntry{}
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
			return restMergeResult{Status: 405, Message: "Pull request has merge conflicts"}
		}
		result[path] = entry
	}
	for path, commonEntry := range commonFiles {
		if _, inHead := headFiles[path]; inHead {
			continue
		}
		current, hasCurrent := baseFiles[path]
		if hasCurrent && current.OID == commonEntry.OID {
			delete(result, path)
		}
	}
	message := "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + pull.HeadRef + "\n\n" + pull.Title
	sha := makeCommit(repo, result, []string{baseTip}, message)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = iso(tick())
	pull.MergeCommitSha = sha
	pull.UpdatedAt = pull.MergedAt
	return restMergeResult{OK: true, SHA: sha}
}

// ---------------------------------------------------------------- commit helpers

type restCommitEntry struct {
	SHA  string
	View CommitView
}

func restCommitLog(repo *Repo, tip string, limit int) []restCommitEntry {
	out := []restCommitEntry{}
	if tip == "" || limit <= 0 {
		return out
	}
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := repo.Objects[oid]
		if !ok || obj.Type != "commit" {
			continue
		}
		view := parseCommit(obj.Data)
		out = append(out, restCommitEntry{SHA: oid, View: view})
		for _, parent := range view.Parents {
			if !seen[parent] {
				queue = append(queue, parent)
			}
		}
	}
	return out
}

var restIdentRE = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d+)$`)

func restIdent(line string) restJObj {
	m := restIdentRE.FindStringSubmatch(line)
	if m == nil {
		return restObj("name", line, "email", "", "date", iso(0))
	}
	seconds, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return restObj("name", line, "email", "", "date", iso(0))
	}
	return restObj("name", m[1], "email", m[2], "date", iso(seconds*1000))
}

func restParentObjs(parents []string) []any {
	out := make([]any, 0, len(parents))
	for _, parent := range parents {
		out = append(out, restObj("sha", parent, "url", ""))
	}
	return out
}

func restResolveCommit(repo *Repo, ref string) string {
	lookup := ref
	if !strings.HasPrefix(ref, "refs/") {
		lookup = "refs/heads/" + ref
	}
	if oid, ok := repo.Refs[lookup]; ok {
		commit, _ := commitOf(repo, oid)
		return commit
	}
	if _, ok := repo.Objects[ref]; ok {
		commit, _ := commitOf(repo, ref)
		return commit
	}
	return ""
}

// ---------------------------------------------------------------- top-level dispatch

// handleRest is the single entry point used by server.go.
func handleRest(method string, segments []string, query url.Values, bodyText, host string) HTTPResult {
	first := ""
	if len(segments) > 0 {
		first = segments[0]
	}

	if first == "" {
		return restReply(200, restObj(
			"current_user_url", "/api/v3/user",
			"authorizations_url", "/api/v3/authorizations",
			"emails_url", "/api/v3/user/emails",
			"feeds_url", "/api/v3/feeds",
			"followers_url", "/api/v3/user/followers",
			"following_url", "/api/v3/user/following{/target}",
			"gists_url", "/api/v3/gists{/gist_id}",
			"issues_url", "/api/v3/issues",
			"notifications_url", "/api/v3/notifications",
			"organization_url", "/api/v3/orgs/{org}",
			"organizations_url", "/api/v3/user/orgs",
			"rate_limit_url", "/api/v3/rate_limit",
			"repository_url", "/api/v3/repos/{owner}/{repo}",
			"current_user_repositories_url", "/api/v3/user/repos{?type,page,per_page,sort}",
			"user_url", "/api/v3/users/{user}",
			"user_repositories_url", "/api/v3/users/{user}/repos{?type,page,per_page,sort}",
		), map[string]string{"x-oauth-scopes": "repo, read:org, gist, workflow"})
	}

	if first == "meta" {
		return restReply(200, restObj(
			"installed_version", "3.0.0-makefun",
			"verifiable_password_authentication", true,
			"ssh_key_fingerprints", restObj(),
			"hooks", []any{"127.0.0.1/32"},
		), nil)
	}

	if first == "user" {
		if len(segments) == 1 {
			if method != "GET" {
				return restFail(404, "Not Found")
			}
			return restReply(200, restObj(
				"login", Viewer.Login,
				"id", Viewer.ID,
				"node_id", "U_"+strconv.Itoa(Viewer.ID),
				"name", Viewer.Name,
				"email", Viewer.Email,
				"type", "User",
				"site_admin", false,
				"public_repos", 0,
				"followers", 0,
				"following", 0,
				"created_at", iso(0),
				"updated_at", iso(0),
			), nil)
		}
		if segments[1] == "repos" {
			if method == "GET" {
				return restListOwnRepos(query, host)
			}
			if method == "POST" {
				return restCreateRepoRoute(bodyText, host, query)
			}
		}
		return restFail(404, "Not Found")
	}

	if first == "repos" && len(segments) >= 3 {
		return restHandleRepo(method, segments, query, bodyText, host)
	}

	return restFail(404, "Not Found")
}

func restListOwnRepos(query url.Values, host string) HTTPResult {
	perPage := restPerPage(query)
	page := restPage(query)
	all := make([]*Repo, 0)
	for _, repo := range GlobalStore.Repos {
		if strings.EqualFold(repo.Owner, Viewer.Login) {
			all = append(all, repo)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	slice := restRepoSlice(all, page, perPage)
	out := make([]any, 0, len(slice))
	for _, repo := range slice {
		out = append(out, restRepoObj(repo))
	}
	extra := map[string]string{"link": restLinkHeader(host, "/api/v3/user/repos", query, page, perPage, len(all))}
	return restReply(200, out, extra)
}

func restRepoSlice(all []*Repo, page, perPage int) []*Repo {
	start := (page - 1) * perPage
	if start < 0 || start >= len(all) {
		return []*Repo{}
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	return all[start:end]
}

func restCreateRepoRoute(bodyText, host string, _ url.Values) HTTPResult {
	parsed, ok := restParseBody(bodyText)
	if !ok {
		return restFail(400, "Problems parsing JSON")
	}
	name := strings.TrimSpace(restStr(parsed, "name", ""))
	if name == "" {
		return restFail(422, "Repository creation failed. Name is required")
	}
	if findRepo(Viewer.Login, name) != nil {
		return restFail(422, "Repository creation failed. Repository "+Viewer.Login+"/"+name+" already exists")
	}
	repo := createRepo(Viewer.Login, name, restStr(parsed, "description", ""), restBool(parsed, "private", false))
	if v, present := parsed["default_branch"]; present {
		if s, isString := v.(string); isString && s != "" {
			repo.DefaultBranch = s
		}
	}
	extra := map[string]string{"location": "http://" + host + "/api/v3/repos/" + Viewer.Login + "/" + name}
	return restReply(201, restRepoObj(repo), extra)
}

func restHandleRepo(method string, segments []string, query url.Values, bodyText, host string) HTTPResult {
	repo := findRepo(segments[1], segments[2])
	if repo == nil || !strings.EqualFold(repo.Owner, Viewer.Login) {
		return restFail(404, "Not Found")
	}

	if len(segments) == 3 {
		switch method {
		case "GET":
			return restReply(200, restRepoObj(repo), nil)
		case "PATCH":
			parsed, ok := restParseBody(bodyText)
			if !ok {
				return restFail(400, "Problems parsing JSON")
			}
			if v, present := parsed["description"]; present {
				if s, isString := v.(string); isString {
					repo.Description = s
				}
			}
			if v, present := parsed["name"]; present {
				if s, isString := v.(string); isString && s != "" && s != repo.Name {
					delete(GlobalStore.Repos, restRepoKey(repo.Owner, repo.Name))
					repo.Name = s
					GlobalStore.Repos[restRepoKey(repo.Owner, repo.Name)] = repo
				}
			}
			if v, present := parsed["private"]; present {
				if b, isBool := v.(bool); isBool {
					repo.Private = b
				}
			}
			repo.UpdatedAt = iso(tick())
			return restReply(200, restRepoObj(repo), nil)
		case "DELETE":
			deleteRepo(repo)
			return restEmpty(204)
		default:
			return restFail(404, "Not Found")
		}
	}

	switch segments[3] {
	case "labels":
		return restHandleLabels(method, segments, bodyText, repo)
	case "issues":
		return restHandleIssues(method, segments, query, bodyText, host, repo)
	case "pulls":
		return restHandlePulls(method, segments, query, bodyText, host, repo)
	case "branches":
		return restHandleBranches(segments, repo)
	case "commits":
		return restHandleCommits(query, repo)
	case "contents":
		return restHandleContents(method, segments, query, bodyText, repo)
	case "git":
		return restHandleGitData(method, segments, bodyText, repo)
	default:
		return restFail(404, "Not Found")
	}
}

// ---------------------------------------------------------------- labels

func restHandleLabels(method string, segments []string, bodyText string, repo *Repo) HTTPResult {
	if method == "GET" {
		labels := make([]Label, 0, len(repo.Labels))
		for _, label := range repo.Labels {
			labels = append(labels, label)
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
		out := make([]any, 0, len(labels))
		for _, label := range labels {
			out = append(out, restLabelObj(label))
		}
		return restReply(200, out, nil)
	}
	if method != "POST" {
		return restFail(404, "Not Found")
	}
	parsed, ok := restParseBody(bodyText)
	if !ok {
		return restFail(400, "Problems parsing JSON")
	}
	name := strings.TrimSpace(restStr(parsed, "name", ""))
	if name == "" {
		return restFail(422, "Validation Failed")
	}
	if existing, found := repo.Labels[strings.ToLower(name)]; found {
		return restReply(201, restLabelObj(existing), nil)
	}
	GlobalStore.LabelSeq++
	label := Label{
		ID:          GlobalStore.LabelSeq,
		Name:        name,
		Color:       restStr(parsed, "color", "ededed"),
		Description: restStr(parsed, "description", ""),
	}
	repo.Labels[strings.ToLower(name)] = label
	return restReply(201, restLabelObj(label), nil)
}

// ---------------------------------------------------------------- branches

func restHandleBranches(segments []string, repo *Repo) HTTPResult {
	rest := restDecodeSegments(segments[4:])
	if len(rest) == 0 {
		type branchRef struct{ ref, oid string }
		branches := make([]branchRef, 0)
		for ref, oid := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				branches = append(branches, branchRef{ref, oid})
			}
		}
		sort.Slice(branches, func(i, j int) bool { return branches[i].ref < branches[j].ref })
		out := make([]any, 0, len(branches))
		for _, b := range branches {
			out = append(out, restObj(
				"name", strings.TrimPrefix(b.ref, "refs/heads/"),
				"commit", restObj("sha", b.oid, "url", ""),
				"protected", false,
			))
		}
		return restReply(200, out, nil)
	}
	branch := strings.Join(rest, "/")
	oid, found := repo.Refs["refs/heads/"+branch]
	if !found {
		return restFail(404, "Branch not found")
	}
	return restReply(200, restObj(
		"name", branch,
		"commit", restObj("sha", oid, "url", ""),
		"protected", false,
	), nil)
}

// ---------------------------------------------------------------- commits

func restHandleCommits(query url.Values, repo *Repo) HTTPResult {
	requested, present := restFirstQuery(query, "sha", "ref")
	if !present {
		requested = repo.DefaultBranch
	}
	oid := ""
	lookup := requested
	if strings.HasPrefix(requested, "refs/") {
		lookup = requested
	} else {
		lookup = "refs/heads/" + requested
	}
	if direct, ok := repo.Refs[lookup]; ok {
		oid = direct
	} else if _, ok := repo.Objects[requested]; ok {
		oid = requested
	}
	tip := ""
	if oid != "" {
		tip, _ = commitOf(repo, oid)
	}
	perPage := restPerPage(query)
	limit := perPage
	if limit > 200 {
		limit = 200
	}
	entries := restCommitLog(repo, tip, limit)
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		message := entry.View.Message
		if strings.HasSuffix(message, "\n") {
			message = message[:len(message)-1]
		}
		out = append(out, restObj(
			"sha", entry.SHA,
			"node_id", "C_"+entry.SHA,
			"commit", restObj(
				"message", message,
				"author", restIdent(entry.View.Author),
				"committer", restIdent(entry.View.Committer),
				"tree", restObj("sha", entry.View.Tree, "url", ""),
			),
			"parents", restParentObjs(entry.View.Parents),
			"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits/"+entry.SHA,
		))
	}
	return restReply(200, out, nil)
}

// ---------------------------------------------------------------- git refs data

func restHandleGitData(method string, segments []string, bodyText string, repo *Repo) HTTPResult {
	return restHandleGitDataDepth(method, segments, bodyText, repo, 0)
}

func restHandleGitDataDepth(method string, segments []string, bodyText string, repo *Repo, depth int) HTTPResult {
	rest := restDecodeSegments(segments[4:])

	if len(rest) >= 2 && rest[0] == "refs" {
		if rest[1] == "heads" && len(rest) >= 3 {
			branch := strings.Join(rest[2:], "/")
			oid, found := repo.Refs["refs/heads/"+branch]
			if !found {
				return restFail(404, "Not Found")
			}
			return restReply(200, restRefObj("refs/heads/"+branch, oid, "commit", repo), nil)
		}
		if rest[1] == "tags" && len(rest) >= 3 {
			tag := strings.Join(rest[2:], "/")
			oid, found := repo.Refs["refs/tags/"+tag]
			if !found {
				return restFail(404, "Not Found")
			}
			typ := "tag"
			if obj, ok := repo.Objects[oid]; ok {
				typ = obj.Type
			}
			return restReply(200, restRefObj("refs/tags/"+tag, oid, typ, repo), nil)
		}
		if rest[1] == "matching-refs" && len(rest) >= 3 {
			prefix := "refs/" + strings.Join(rest[2:], "/")
			type refEntry struct{ ref, oid string }
			matches := make([]refEntry, 0)
			for ref, oid := range repo.Refs {
				if strings.HasPrefix(ref, prefix) {
					matches = append(matches, refEntry{ref, oid})
				}
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].ref < matches[j].ref })
			out := make([]any, 0, len(matches))
			for _, m := range matches {
				out = append(out, restRefObj(m.ref, m.oid, "commit", repo))
			}
			return restReply(200, out, nil)
		}
	}

	if len(rest) >= 1 && rest[0] == "refs" && method == "POST" {
		parsed, ok := restParseBody(bodyText)
		if !ok {
			return restFail(400, "Problems parsing JSON")
		}
		ref := restStr(parsed, "ref", "")
		sha := restStr(parsed, "sha", "")
		if _, exists := repo.Objects[sha]; ref == "" || !exists {
			return restFail(422, "Reference update failed")
		}
		if _, exists := repo.Refs[ref]; exists {
			return restFail(422, "Reference already exists")
		}
		repo.Refs[ref] = sha
		typ := "commit"
		if obj, ok := repo.Objects[sha]; ok {
			typ = obj.Type
		}
		return restReply(201, restRefObj(ref, sha, typ, repo), nil)
	}

	if len(rest) >= 3 && rest[0] == "refs" && (method == "PATCH" || method == "DELETE") {
		ref := "refs/" + strings.Join(rest[1:], "/")
		if method == "DELETE" {
			if _, exists := repo.Refs[ref]; !exists {
				return restFail(422, "Reference does not exist")
			}
			delete(repo.Refs, ref)
			return restEmpty(204)
		}
		delete(repo.Refs, ref)
		if depth < 2 {
			return restHandleGitDataDepth(method, segments, bodyText, repo, depth+1)
		}
		return restFail(404, "Not Found")
	}

	return restFail(404, "Not Found")
}

func restRefObj(ref, oid, typ string, _ *Repo) restJObj {
	return restObj(
		"ref", ref,
		"node_id", "R_"+oid,
		"url", "",
		"object", restObj("sha", oid, "type", typ, "url", ""),
	)
}

// ---------------------------------------------------------------- issues

func restResolveLabels(repo *Repo, value any) ([]Label, string) {
	arr, ok := value.([]any)
	if !ok {
		return []Label{}, ""
	}
	labels := make([]Label, 0, len(arr))
	for _, item := range arr {
		name := restTSString(item)
		label, found := repo.Labels[strings.ToLower(name)]
		if !found {
			return nil, "label " + name + " does not exist"
		}
		labels = append(labels, label)
	}
	return labels, ""
}

func restHandleIssues(method string, segments []string, query url.Values, bodyText, host string, repo *Repo) HTTPResult {
	rest := restDecodeSegments(segments[4:])
	if len(rest) == 0 {
		if method == "GET" {
			return restListIssues(query, host, repo)
		}
		if method == "POST" {
			parsed, ok := restParseBody(bodyText)
			if !ok {
				return restFail(400, "Problems parsing JSON")
			}
			title := restStr(parsed, "title", "")
			if strings.TrimSpace(title) == "" {
				return restFail(422, "Validation Failed")
			}
			labels, errMessage := restResolveLabels(repo, parsed["labels"])
			if errMessage != "" {
				return restFail(422, "Validation Failed: "+errMessage)
			}
			issue := restCreateIssue(repo, title, restStr(parsed, "body", ""), labels)
			extra := map[string]string{
				"location": "http://" + host + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
			}
			return restReplyText(201, restIssueJSONText(issue, repo), extra)
		}
		return restFail(404, "Not Found")
	}

	number, valid := restParseInt(rest[0])
	if !valid {
		return restFail(404, "Not Found")
	}
	var issue *Issue
	for _, entry := range repo.Issues {
		if entry.Number == number {
			issue = entry
			break
		}
	}
	if issue == nil {
		return restFail(404, "Not Found")
	}

	if len(rest) == 2 && rest[1] == "comments" {
		if method == "GET" {
			comments := repo.Comments[issue.ID]
			out := make([]any, 0, len(comments))
			for _, comment := range comments {
				out = append(out, restCommentObj(comment))
			}
			return restReply(200, out, nil)
		}
		if method == "POST" {
			parsed, ok := restParseBody(bodyText)
			if !ok {
				return restFail(400, "Problems parsing JSON")
			}
			comment := restAddComment(repo, issue, restStr(parsed, "body", ""))
			return restReply(201, restCommentObj(comment), nil)
		}
	}

	if len(rest) == 1 {
		if method == "GET" {
			text := restIssueString(issue, repo)
			result := restReplyText(200, text, nil)
			result.CacheKey = text
			return result
		}
		if method == "PATCH" {
			parsed, ok := restParseBody(bodyText)
			if !ok {
				return restFail(400, "Problems parsing JSON")
			}
			if v, present := parsed["state"]; present {
				if s, isString := v.(string); isString && (s == "open" || s == "closed") {
					issue.State = s
				}
			}
			if v, present := parsed["title"]; present {
				if s, isString := v.(string); isString {
					issue.Title = s
				}
			}
			if v, present := parsed["body"]; present {
				if s, isString := v.(string); isString {
					issue.Body = s
				}
			}
			if v, present := parsed["labels"]; present {
				labels, errMessage := restResolveLabels(repo, v)
				if errMessage != "" {
					return restFail(422, "Validation Failed: "+errMessage)
				}
				issue.Labels = labels
			}
			issue.UpdatedAt = iso(tick())
			issue.Cache = ""
			return restReplyText(200, restIssueJSONText(issue, repo), nil)
		}
	}

	return restFail(404, "Not Found")
}

func restListIssues(query url.Values, host string, repo *Repo) HTTPResult {
	state := restQueryGet(query, "state", "open")
	sortBy := restQueryGet(query, "sort", "created")
	direction := restQueryGet(query, "direction", "desc")
	perPage := restPerPage(query)
	page := restPage(query)

	labelsFilter := make([]string, 0)
	for _, raw := range query["labels"] {
		for _, name := range strings.Split(raw, ",") {
			if name != "" {
				labelsFilter = append(labelsFilter, name)
			}
		}
	}

	list := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		if state == "all" || issue.State == state {
			list = append(list, issue)
		}
	}
	if len(labelsFilter) > 0 {
		filtered := make([]*Issue, 0, len(list))
		for _, issue := range list {
			matches := true
			for _, wanted := range labelsFilter {
				found := false
				for _, label := range issue.Labels {
					if strings.EqualFold(label.Name, wanted) {
						found = true
						break
					}
				}
				if !found {
					matches = false
					break
				}
			}
			if matches {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	if sortBy == "created" || sortBy == "updated" {
		ascending := direction == "asc"
		sort.SliceStable(list, func(i, j int) bool {
			if ascending {
				return list[i].Number < list[j].Number
			}
			return list[i].Number > list[j].Number
		})
	}

	total := len(list)
	start := (page - 1) * perPage
	slice := make([]*Issue, 0)
	if start >= 0 && start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		slice = list[start:end]
	}
	out := make([]any, 0, len(slice))
	for _, issue := range slice {
		out = append(out, restIssueObj(issue, repo))
	}
	path := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues"
	extra := map[string]string{"link": restLinkHeader(host, path, query, page, perPage, total)}
	return restReply(200, out, extra)
}

// ---------------------------------------------------------------- pull requests

func restAfterColon(text string) string {
	if i := strings.LastIndex(text, ":"); i >= 0 {
		return text[i+1:]
	}
	return text
}

func restHandlePulls(method string, segments []string, query url.Values, bodyText, host string, repo *Repo) HTTPResult {
	rest := restDecodeSegments(segments[4:])
	if len(rest) == 0 {
		if method == "GET" {
			state := restQueryGet(query, "state", "open")
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				if state == "all" || pull.State == state {
					list = append(list, pull)
				}
			}
			if headFilter := restQueryGet(query, "head", ""); headFilter != "" {
				branch := restAfterColon(headFilter)
				filtered := make([]*Pull, 0, len(list))
				for _, pull := range list {
					if pull.HeadRef == branch {
						filtered = append(filtered, pull)
					}
				}
				list = filtered
			}
			total := len(list)
			start := (restPageOfQuery(query) - 1) * restPerPage(query)
			slice := make([]*Pull, 0)
			if start >= 0 && start < total {
				end := start + restPerPage(query)
				if end > total {
					end = total
				}
				slice = list[start:end]
			}
			out := make([]any, 0, len(slice))
			for _, pull := range slice {
				out = append(out, restPullObj(pull, repo))
			}
			path := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls"
			extra := map[string]string{"link": restLinkHeader(host, path, query, restPageOfQuery(query), restPerPage(query), total)}
			return restReply(200, out, extra)
		}
		if method == "POST" {
			parsed, ok := restParseBody(bodyText)
			if !ok {
				return restFail(400, "Problems parsing JSON")
			}
			head := restAfterColon(restStr(parsed, "head", ""))
			base := restAfterColon(restStr(parsed, "base", ""))
			created, errMessage := restCreatePull(repo, restStr(parsed, "title", ""), restStr(parsed, "body", ""), head, base)
			if errMessage != "" {
				return restFail(422, "Validation Failed: "+errMessage)
			}
			extra := map[string]string{
				"location": "http://" + host + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(created.Number),
			}
			return restReply(201, restPullObj(created, repo), extra)
		}
		return restFail(404, "Not Found")
	}

	number, _ := restParseInt(rest[0])
	var pull *Pull
	for _, entry := range repo.Pulls {
		if entry.Number == number {
			pull = entry
			break
		}
	}
	if pull == nil {
		return restFail(404, "Not Found")
	}

	if len(rest) == 1 {
		if method != "GET" {
			return restFail(404, "Not Found")
		}
		return restReply(200, restPullObj(pull, repo), nil)
	}

	if rest[1] == "files" {
		baseCommit, _ := commitOf(repo, pull.BaseSha)
		headCommit, _ := commitOf(repo, pull.HeadSha)
		files := restDiffFiles(repo, baseCommit, headCommit)
		out := make([]any, 0, len(files))
		for _, file := range files {
			out = append(out, restObj(
				"sha", file.SHA,
				"filename", file.Filename,
				"status", file.Status,
				"additions", file.Additions,
				"deletions", file.Deletions,
				"changes", file.Changes,
				"blob_url", "",
				"raw_url", "",
				"contents_url", "",
			))
		}
		return restReply(200, out, nil)
	}

	if rest[1] == "merge" && method == "PUT" {
		result := restMergePull(repo, pull)
		if !result.OK {
			return restFail(result.Status, result.Message)
		}
		return restReply(200, restObj(
			"sha", result.SHA,
			"merged", true,
			"message", "Pull Request successfully merged",
		), nil)
	}

	return restFail(404, "Not Found")
}

func restPageOfQuery(query url.Values) int { return restPage(query) }

type restDiff struct {
	SHA       string
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func restDiffFiles(repo *Repo, baseCommit, headCommit string) []restDiff {
	base := filesAtCommit(repo, baseCommit)
	head := filesAtCommit(repo, headCommit)
	out := []restDiff{}
	for path, entry := range head {
		before, found := base[path]
		if found && before.OID == entry.OID {
			continue
		}
		added := !found
		status := "modified"
		deletions := 1
		changes := 2
		if added {
			status = "added"
			deletions = 0
			changes = 1
		}
		out = append(out, restDiff{SHA: entry.OID, Filename: path, Status: status, Additions: 1, Deletions: deletions, Changes: changes})
	}
	for path, entry := range base {
		if _, found := head[path]; found {
			continue
		}
		out = append(out, restDiff{SHA: entry.OID, Filename: path, Status: "removed", Additions: 0, Deletions: 1, Changes: 1})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// ---------------------------------------------------------------- contents

func restHandleContents(method string, segments []string, query url.Values, bodyText string, repo *Repo) HTTPResult {
	rest := restDecodeSegments(segments[4:])
	path := strings.Join(rest, "/")
	ref := restQueryGet(query, "ref", repo.DefaultBranch)

	if method == "GET" {
		commit := restResolveCommit(repo, ref)
		if commit == "" {
			return restFail(404, "Not Found")
		}
		commitObject, ok := repo.Objects[commit]
		if !ok {
			return restFail(404, "Not Found")
		}
		treeOID := parseCommit(commitObject.Data).Tree
		if path == "" {
			rootTree, ok := repo.Objects[treeOID]
			if !ok {
				return restFail(404, "Not Found")
			}
			return restReply(200, restListingOf(repo, rootTree, ""), nil)
		}
		parts := strings.Split(path, "/")
		current, ok := repo.Objects[treeOID]
		for index, part := range parts {
			if !ok {
				return restFail(404, "Not Found")
			}
			entry := restTreeEntry(current.Data, part)
			if entry == nil {
				return restFail(404, "Not Found")
			}
			isLast := index == len(parts)-1
			if isLast {
				if entry.Mode == "40000" {
					dirTree, found := repo.Objects[entry.OID]
					if !found {
						return restFail(404, "Not Found")
					}
					return restReply(200, restListingOf(repo, dirTree, path), nil)
				}
				blob, found := repo.Objects[entry.OID]
				if !found {
					return restFail(404, "Not Found")
				}
				return restReply(200, restObj(
					"type", "file",
					"encoding", "base64",
					"size", len(blob.Data),
					"name", entry.Name,
					"path", path,
					"content", b64(blob.Data),
					"sha", entry.OID,
					"url", "",
					"git_url", "",
					"html_url", "",
					"download_url", "",
				), nil)
			}
			if entry.Mode != "40000" {
				return restFail(404, "Not Found")
			}
			current, ok = repo.Objects[entry.OID]
		}
		return restFail(404, "Not Found")
	}

	if method == "PUT" {
		parsed, ok := restParseBody(bodyText)
		if !ok {
			return restFail(400, "Problems parsing JSON")
		}
		if path == "" {
			return restFail(422, "Validation Failed: path is required")
		}
		content := restStr(parsed, "content", "")
		branch := restStr(parsed, "branch", "")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := restStr(parsed, "message", "Update file")
		branchRef := "refs/heads/" + branch
		tip := repo.Refs[branchRef]
		tipCommit := ""
		if tip != "" {
			tipCommit, _ = commitOf(repo, tip)
		}
		files := filesAtCommit(repo, tipCommit)
		before, hadBefore := files[path]
		sha := restStr(parsed, "sha", "")
		if sha != "" && (!hadBefore || before.OID != sha) {
			return restFail(409, "sha does not match the current file")
		}
		bytes := unb64(content)
		blobOID := putObject(repo, "blob", bytes)
		files[path] = FlatEntry{Mode: "100644", OID: blobOID}
		var parents []string
		if tip != "" {
			if parent, _ := commitOf(repo, tip); parent != "" {
				parents = []string{parent}
			}
		}
		commit := makeCommit(repo, files, parents, message)
		repo.Refs[branchRef] = commit
		repo.UpdatedAt = iso(tick())
		status := 201
		if hadBefore {
			status = 200
		}
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		return restReply(status, restObj(
			"content", restObj(
				"name", name,
				"path", path,
				"sha", blobOID,
				"size", len(bytes),
				"encoding", "base64",
				"content", content,
				"type", "file",
			),
			"commit", restCommitJSON(commit, message),
		), nil)
	}

	if method == "DELETE" {
		parsed, ok := restParseBody(bodyText)
		if !ok {
			return restFail(400, "Problems parsing JSON")
		}
		if path == "" {
			return restFail(422, "Validation Failed: path is required")
		}
		branch := restStr(parsed, "branch", "")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		branchRef := "refs/heads/" + branch
		tip, found := repo.Refs[branchRef]
		if !found {
			return restFail(404, "Not Found")
		}
		tipCommit, _ := commitOf(repo, tip)
		files := filesAtCommit(repo, tipCommit)
		before, found := files[path]
		if !found {
			return restFail(404, "Not Found")
		}
		sha := restStr(parsed, "sha", "")
		if sha != "" && sha != before.OID {
			return restFail(409, "sha does not match the current file")
		}
		delete(files, path)
		message := restStr(parsed, "message", "Delete file")
		var parents []string
		if parent, _ := commitOf(repo, tip); parent != "" {
			parents = []string{parent}
		}
		commit := makeCommit(repo, files, parents, message)
		repo.Refs[branchRef] = commit
		return restReply(200, restObj(
			"content", nil,
			"commit", restCommitJSON(commit, message),
		), nil)
	}

	return restFail(404, "Not Found")
}

func restTreeEntry(data []byte, name string) *TreeEntry {
	for _, entry := range parseTree(data) {
		if entry.Name == name {
			copy := entry
			return &copy
		}
	}
	return nil
}

func restListingOf(repo *Repo, tree GitObject, prefix string) []any {
	entries := parseTree(tree.Data)
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		childPath := entry.Name
		if prefix != "" {
			childPath = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			out = append(out, restObj(
				"type", "dir",
				"name", entry.Name,
				"path", childPath,
				"sha", entry.OID,
				"size", 0,
				"mode", "040000",
				"url", "",
			))
			continue
		}
		size := 0
		if blob, ok := repo.Objects[entry.OID]; ok {
			size = len(blob.Data)
		}
		mode := "100644"
		if entry.Mode == "100755" {
			mode = "100755"
		}
		out = append(out, restObj(
			"type", "file",
			"name", entry.Name,
			"path", childPath,
			"sha", entry.OID,
			"size", size,
			"mode", mode,
			"url", "",
		))
	}
	return out
}
