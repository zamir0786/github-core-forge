package main

// A small GraphQL v4 executor over the same store: the documents gh and github-mcp-server send
// (viewer, repository reads, __type probes, issue and pull-request reads and mutations, node(id:),
// cursor pagination and operation batching). Ported from bot/graphql.ts.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- ordered JSON object

// orderedObject keeps insertion order so GraphQL selection order and JSON-RPC key order
// are byte-stable, the way JavaScript's JSON.stringify over an object literal is.
type orderedObject struct {
	order  []string
	values map[string]interface{}
}

func newOrderedObject() *orderedObject {
	return &orderedObject{values: make(map[string]interface{})}
}

func (o *orderedObject) set(key string, value interface{}) {
	if _, exists := o.values[key]; !exists {
		o.order = append(o.order, key)
	}
	o.values[key] = value
}

func (o *orderedObject) get(key string) (interface{}, bool) {
	v, ok := o.values[key]
	return v, ok
}

func (o *orderedObject) del(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, k := range o.order {
		if k == key {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.order {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := jsonMarshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := jsonMarshal(o.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// ---------------------------------------------------------------- store adapters

// These wrappers are the only places that bind to the store API; keeping them here makes
// any signature differences with the in-progress core/store.go a one-line fix.
func gqlUserOf(login string) User { return userOf(login) }

func gqlCreateIssue(repo *Repo, title, body string) *Issue {
	return createIssue(repo, title, body, nil)
}

func gqlCreatePull(repo *Repo, title, body, head, base string) (*Pull, string) {
	return createPull(repo, title, body, head, base)
}

func gqlAddComment(repo *Repo, issue *Issue, body string) *Comment {
	comment := addComment(repo, issue, body)
	return &comment
}

func gqlCommitOf(repo *Repo, oid string) string {
	commit, _ := commitOf(repo, oid)
	return commit
}

func gqlDiffFiles(repo *Repo, baseCommit, headCommit string) []DiffFile {
	return diffFiles(repo, baseCommit, headCommit)
}

// ---------------------------------------------------------------- lexer and parser

type gqlTokenKind int

const (
	gqlName gqlTokenKind = iota
	gqlInt
	gqlString
	gqlPunct
	gqlEOF
)

type gqlToken struct {
	kind  gqlTokenKind
	value string
}

func gqlIsNameStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func gqlIsNameCont(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlLex(source string) []gqlToken {
	tokens := make([]gqlToken, 0, len(source)/2+1)
	i := 0
	for i < len(source) {
		ch := source[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if strings.HasPrefix(source[i:], "\uFEFF") {
			i += len("\uFEFF")
			continue
		}
		if ch == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' {
			i++
			var text strings.Builder
			for i < len(source) && source[i] != '"' {
				if source[i] == '\\' {
					i++
					escape := ""
					if i < len(source) {
						escape = string(source[i])
					}
					switch escape {
					case "n":
						text.WriteByte('\n')
					case "t":
						text.WriteByte('\t')
					case "r":
						text.WriteByte('\r')
					case "u":
						if i+4 < len(source) {
							if n, err := strconv.ParseUint(source[i+1:i+5], 16, 16); err == nil {
								text.WriteRune(rune(n))
							}
							i += 4
						}
					default:
						text.WriteString(escape)
					}
					i++
				} else {
					text.WriteByte(source[i])
					i++
				}
			}
			i++
			tokens = append(tokens, gqlToken{kind: gqlString, value: text.String()})
			continue
		}
		if ch == '-' || (ch >= '0' && ch <= '9') {
			start := i
			i++
			for i < len(source) {
				c := source[i]
				if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
					i++
					continue
				}
				break
			}
			tokens = append(tokens, gqlToken{kind: gqlInt, value: source[start:i]})
			continue
		}
		if gqlIsNameStart(ch) {
			start := i
			i++
			for i < len(source) && gqlIsNameCont(source[i]) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: gqlName, value: source[start:i]})
			continue
		}
		if ch == '.' && strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: gqlPunct, value: "..."})
			i += 3
			continue
		}
		tokens = append(tokens, gqlToken{kind: gqlPunct, value: string(ch)})
		i++
	}
	tokens = append(tokens, gqlToken{kind: gqlEOF, value: ""})
	return tokens
}

type gqlValue struct {
	kind   string // var | int | string | bool | null | enum | list | object
	text   string
	num    float64
	b      bool
	list   []*gqlValue
	fields map[string]*gqlValue
}

type gqlSelection struct {
	kind       string // field | fragment
	alias      string
	name       string
	typeName   string
	args       map[string]*gqlValue
	selections []gqlSelection
}

type gqlOperation struct {
	kind       string // query | mutation
	selections []gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	index  int
}

func (p *gqlParser) peek() gqlToken {
	if p.index >= len(p.tokens) {
		return gqlToken{kind: gqlEOF, value: ""}
	}
	return p.tokens[p.index]
}

func (p *gqlParser) next() gqlToken {
	t := p.peek()
	if p.index < len(p.tokens) {
		p.index++
	}
	return t
}

func (p *gqlParser) expect(value string) error {
	token := p.next()
	if token.value != value {
		return fmt.Errorf("expected %s but saw %s", value, token.value)
	}
	return nil
}

func (p *gqlParser) skipDirectives() {
	for p.peek().value == "@" {
		p.next()
		p.next()
	}
}

func (p *gqlParser) parseDocument() ([]gqlOperation, error) {
	operations := make([]gqlOperation, 0, 4)
	for p.peek().kind != gqlEOF {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return operations, nil
}

func (p *gqlParser) parseOperation() (gqlOperation, error) {
	token := p.peek()
	if token.kind == gqlPunct && token.value == "{" {
		selections, err := p.parseSelectionSet()
		return gqlOperation{kind: "query", selections: selections}, err
	}
	keyword := p.next().value
	if keyword != "query" && keyword != "mutation" && keyword != "subscription" {
		return gqlOperation{}, fmt.Errorf("unsupported operation %s", keyword)
	}
	if p.peek().kind == gqlName {
		p.next()
	}
	if p.peek().value == "(" {
		p.next()
		depth := 1
		for depth > 0 && p.peek().kind != gqlEOF {
			current := p.next()
			if current.value == "(" {
				depth++
			} else if current.value == ")" {
				depth--
			}
		}
	}
	p.skipDirectives()
	selections, err := p.parseSelectionSet()
	kind := "query"
	if keyword == "mutation" {
		kind = "mutation"
	}
	return gqlOperation{kind: kind, selections: selections}, err
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	selections := make([]gqlSelection, 0, 8)
	for p.peek().value != "}" && p.peek().kind != gqlEOF {
		selection, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, selection)
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek().value == "..." {
		p.next()
		typeName := ""
		if p.peek().value == "on" {
			p.next()
			typeName = p.next().value
		} else if p.peek().kind == gqlName {
			p.next()
		}
		p.skipDirectives()
		if p.peek().value == "{" {
			selections, err := p.parseSelectionSet()
			return gqlSelection{kind: "fragment", typeName: typeName, selections: selections}, err
		}
		return gqlSelection{kind: "fragment", typeName: typeName, selections: []gqlSelection{}}, nil
	}
	name := p.next().value
	alias := ""
	if p.peek().value == ":" {
		p.next()
		alias = name
		name = p.next().value
	}
	args := map[string]*gqlValue{}
	var err error
	if p.peek().value == "(" {
		args, err = p.parseArgs()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	p.skipDirectives()
	selections := []gqlSelection{}
	if p.peek().value == "{" {
		selections, err = p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	return gqlSelection{kind: "field", alias: alias, name: name, args: args, selections: selections}, nil
}

func (p *gqlParser) parseArgs() (map[string]*gqlValue, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	args := make(map[string]*gqlValue)
	for p.peek().value != ")" && p.peek().kind != gqlEOF {
		name := p.next().value
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = value
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	token := p.next()
	if token.value == "$" {
		return &gqlValue{kind: "var", text: p.next().value}, nil
	}
	if token.value == "[" {
		values := make([]*gqlValue, 0, 4)
		for p.peek().value != "]" && p.peek().kind != gqlEOF {
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return &gqlValue{kind: "list", list: values}, nil
	}
	if token.value == "{" {
		fields := make(map[string]*gqlValue)
		for p.peek().value != "}" && p.peek().kind != gqlEOF {
			name := p.next().value
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields[name] = value
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return &gqlValue{kind: "object", fields: fields}, nil
	}
	if token.kind == gqlString {
		return &gqlValue{kind: "string", text: token.value}, nil
	}
	if token.kind == gqlInt {
		num, _ := strconv.ParseFloat(token.value, 64)
		return &gqlValue{kind: "int", num: num}, nil
	}
	if token.value == "true" {
		return &gqlValue{kind: "bool", b: true}, nil
	}
	if token.value == "false" {
		return &gqlValue{kind: "bool", b: false}, nil
	}
	if token.value == "null" {
		return &gqlValue{kind: "null"}, nil
	}
	return &gqlValue{kind: "enum", text: token.value}, nil
}

func gqlParse(query string) ([]gqlOperation, error) {
	parser := &gqlParser{tokens: gqlLex(query)}
	return parser.parseDocument()
}

func gqlEvalValue(value *gqlValue, vars map[string]interface{}) interface{} {
	if value == nil {
		return nil
	}
	switch value.kind {
	case "var":
		return vars[value.text]
	case "int":
		return value.num
	case "string":
		return value.text
	case "bool":
		return value.b
	case "null":
		return nil
	case "enum":
		return value.text
	case "list":
		out := make([]interface{}, 0, len(value.list))
		for _, entry := range value.list {
			out = append(out, gqlEvalValue(entry, vars))
		}
		return out
	case "object":
		out := make(map[string]interface{}, len(value.fields))
		for key, entry := range value.fields {
			out[key] = gqlEvalValue(entry, vars)
		}
		return out
	}
	return nil
}

func gqlEvalArgs(args map[string]*gqlValue, vars map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		out[key] = gqlEvalValue(value, vars)
	}
	return out
}

// ---------------------------------------------------------------- global ids and wrappers

func gqlGlobalID(typeName string, id int) string {
	return b64([]byte(typeName + ":" + strconv.Itoa(id)))
}

func gqlParseIntPrefix(text string) (int, bool) {
	text = strings.TrimLeft(text, " \t\n\r\f\v")
	if text == "" {
		return 0, false
	}
	sign := 1
	if text[0] == '+' || text[0] == '-' {
		if text[0] == '-' {
			sign = -1
		}
		text = text[1:]
	}
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(text[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	return int(n) * sign, true
}

func gqlDecodeID(value interface{}) (string, int, bool) {
	text, ok := value.(string)
	if !ok {
		return "", 0, false
	}
	decoded := string(unb64(text))
	colon := strings.IndexByte(decoded, ':')
	if colon < 0 {
		return "", 0, false
	}
	id, ok := gqlParseIntPrefix(decoded[colon+1:])
	if !ok {
		return "", 0, false
	}
	return decoded[:colon], id, true
}

type gqlWrapped struct {
	Type string
	Ref  interface{}
}

func gqlWrap(typeName string, ref interface{}) *gqlWrapped {
	return &gqlWrapped{Type: typeName, Ref: ref}
}

func gqlAsWrapped(value interface{}) (*gqlWrapped, bool) {
	w, ok := value.(*gqlWrapped)
	return w, ok
}

type gqlIssueRef struct {
	Repo  *Repo
	Issue *Issue
}

type gqlPullRef struct {
	Repo *Repo
	Pull *Pull
}

type gqlConnRef struct {
	Nodes     []interface{}
	HasNext   bool
	EndCursor string
}

func gqlFindRepoByID(id int) *Repo {
	for _, repo := range GlobalStore.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func gqlFindIssueByID(id int) (*Repo, *Issue) {
	for _, repo := range GlobalStore.Repos {
		for _, issue := range repo.Issues {
			if issue.ID == id {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func gqlFindPullByID(id int) (*Repo, *Pull) {
	for _, repo := range GlobalStore.Repos {
		for _, pull := range repo.Pulls {
			if pull.ID == id {
				return repo, pull
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- schema description

var gqlSchemaFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "comments", "labels", "author", "createdAt", "updatedAt", "closed", "repository"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "comments", "author", "createdAt", "updatedAt"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "description", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "isPrivate", "issue", "issues", "pullRequests", "pullRequest"},
}

var gqlSchemaEnums = map[string][]string{
	"SearchType":       {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

// ---------------------------------------------------------------- execution

type gqlExecContext struct {
	vars   map[string]interface{}
	errors []interface{}
}

func gqlConnPage(values []interface{}, args map[string]interface{}) ([]interface{}, bool, string) {
	first := 100
	if n, ok := args["first"].(float64); ok {
		first = int(n)
	}
	if first < 1 {
		first = 1
	}
	if first > 100 {
		first = 100
	}
	offset := 0
	if after, ok := args["after"].(string); ok && after != "" {
		decoded := string(unb64(after))
		colon := strings.IndexByte(decoded, ':')
		if parsed, ok := gqlParseIntPrefix(decoded[colon+1:]); ok && parsed >= 0 {
			offset = parsed
		}
	}
	items := make([]interface{}, 0, first)
	if offset < len(values) {
		end := offset + first
		if end > len(values) {
			end = len(values)
		}
		items = append(items, values[offset:end]...)
	}
	end := offset + len(items)
	hasNext := end < len(values)
	endCursor := b64([]byte("cursor:" + strconv.Itoa(end)))
	return items, hasNext, endCursor
}

func gqlIssueConnection(repo *Repo, args map[string]interface{}) *gqlWrapped {
	list := make([]*Issue, 0, len(repo.Issues))
	list = append(list, repo.Issues...)
	if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
		wanted := make(map[string]bool, len(states))
		for _, state := range states {
			wanted[strings.ToUpper(gqlToString(state))] = true
		}
		filtered := list[:0]
		for _, issue := range list {
			if wanted[strings.ToUpper(issue.State)] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	direction := "ASC"
	if orderBy, ok := args["orderBy"].(map[string]interface{}); ok {
		if dir, ok := orderBy["direction"].(string); ok {
			direction = strings.ToUpper(dir)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if direction == "DESC" {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	values := make([]interface{}, 0, len(list))
	for _, issue := range list {
		values = append(values, gqlWrap("Issue", &gqlIssueRef{Repo: repo, Issue: issue}))
	}
	items, hasNext, endCursor := gqlConnPage(values, args)
	return gqlWrap("IssueConnection", &gqlConnRef{Nodes: items, HasNext: hasNext, EndCursor: endCursor})
}

func gqlPullConnection(repo *Repo, args map[string]interface{}) *gqlWrapped {
	list := make([]*Pull, 0, len(repo.Pulls))
	list = append(list, repo.Pulls...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	values := make([]interface{}, 0, len(list))
	for _, pull := range list {
		values = append(values, gqlWrap("PullRequest", &gqlPullRef{Repo: repo, Pull: pull}))
	}
	items, hasNext, endCursor := gqlConnPage(values, args)
	return gqlWrap("PullRequestConnection", &gqlConnRef{Nodes: items, HasNext: hasNext, EndCursor: endCursor})
}

func gqlToString(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	default:
		return fmt.Sprint(v)
	}
}

func gqlNumberArg(value interface{}) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		n, _ := gqlParseIntPrefix(gqlToString(value))
		return n
	}
}

func gqlFindIssue(repo *Repo, number interface{}) *Issue {
	parsed := gqlNumberArg(number)
	for _, issue := range repo.Issues {
		if issue.Number == parsed {
			return issue
		}
	}
	return nil
}

func gqlFindPull(repo *Repo, number interface{}) *Pull {
	parsed := gqlNumberArg(number)
	for _, pull := range repo.Pulls {
		if pull.Number == parsed {
			return pull
		}
	}
	return nil
}

func gqlIssueState(issue *Issue) string { return strings.ToUpper(issue.State) }

func gqlPullState(pull *Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	return strings.ToUpper(pull.State)
}

func gqlResolve(typeName string, ref interface{}, field string, args map[string]interface{}, ctx *gqlExecContext) interface{} {
	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return gqlWrap("Viewer", Viewer)
		case "repository":
			repo := findRepo(gqlToString(args["owner"]), gqlToString(args["name"]))
			if repo == nil {
				return nil
			}
			return gqlWrap("Repository", repo)
		case "node":
			decodedType, id, ok := gqlDecodeID(args["id"])
			if !ok {
				return nil
			}
			switch decodedType {
			case "Repository":
				repo := gqlFindRepoByID(id)
				if repo == nil {
					return nil
				}
				return gqlWrap("Repository", repo)
			case "Issue":
				repo, issue := gqlFindIssueByID(id)
				if issue == nil {
					return nil
				}
				return gqlWrap("Issue", &gqlIssueRef{Repo: repo, Issue: issue})
			case "PullRequest":
				repo, pull := gqlFindPullByID(id)
				if pull == nil {
					return nil
				}
				return gqlWrap("PullRequest", &gqlPullRef{Repo: repo, Pull: pull})
			}
			return nil
		case "__type":
			name := gqlToString(args["name"])
			_, inFields := gqlSchemaFields[name]
			_, inEnums := gqlSchemaEnums[name]
			if !inFields && !inEnums {
				return nil
			}
			return gqlWrap("__Type", name)
		case "__schema":
			return gqlWrap("__Schema", nil)
		}
	case "Mutation":
		return gqlResolveMutation(field, args)
	case "Viewer", "User":
		user, ok := gqlAsUser(ref)
		if !ok {
			break
		}
		switch field {
		case "login":
			return user.Login
		case "name":
			return user.Name
		case "id":
			return gqlGlobalID("User", user.ID)
		case "databaseId":
			return user.ID
		}
	case "Repository":
		repo, ok := ref.(*Repo)
		if !ok {
			break
		}
		switch field {
		case "id":
			return gqlGlobalID("Repository", repo.ID)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name
		case "description":
			return repo.Description
		case "owner":
			return gqlWrap("User", gqlUserOf(repo.Owner))
		case "defaultBranchRef":
			if _, has := repo.Refs["refs/heads/"+repo.DefaultBranch]; has {
				return gqlWrap("Ref", map[string]interface{}{"name": repo.DefaultBranch})
			}
			return nil
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "isPrivate":
			return repo.Private
		case "issue":
			issue := gqlFindIssue(repo, args["number"])
			if issue == nil {
				return nil
			}
			return gqlWrap("Issue", &gqlIssueRef{Repo: repo, Issue: issue})
		case "issues":
			return gqlIssueConnection(repo, args)
		case "pullRequests":
			return gqlPullConnection(repo, args)
		case "pullRequest":
			pull := gqlFindPull(repo, args["number"])
			if pull == nil {
				return nil
			}
			return gqlWrap("PullRequest", &gqlPullRef{Repo: repo, Pull: pull})
		}
	case "Ref":
		value, ok := ref.(map[string]interface{})
		if !ok {
			break
		}
		if field == "name" {
			return value["name"]
		}
		if field == "target" {
			return nil
		}
	case "Issue":
		issueRef, ok := ref.(*gqlIssueRef)
		if !ok {
			break
		}
		issue := issueRef.Issue
		switch field {
		case "id":
			return gqlGlobalID("Issue", issue.ID)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return gqlIssueState(issue)
		case "comments":
			return gqlWrap("IssueCommentConnection", &gqlConnRef{Nodes: []interface{}{}})
		case "labels":
			labels := make([]interface{}, 0, len(issue.Labels))
			for _, label := range issue.Labels {
				labels = append(labels, label)
			}
			return gqlWrap("LabelConnection", &gqlConnRef{Nodes: labels})
		case "author":
			return gqlWrap("User", Viewer)
		case "createdAt":
			return issue.CreatedAt
		case "updatedAt":
			return issue.UpdatedAt
		case "closed":
			return issue.State == "closed"
		}
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		conn, ok := ref.(*gqlConnRef)
		if !ok {
			break
		}
		switch field {
		case "nodes":
			return conn.Nodes
		case "totalCount":
			return len(conn.Nodes)
		case "pageInfo":
			return gqlWrap("PageInfo", conn)
		}
	case "PageInfo":
		conn, ok := ref.(*gqlConnRef)
		if !ok {
			break
		}
		switch field {
		case "hasNextPage":
			return conn.HasNext
		case "hasPreviousPage":
			return false
		case "endCursor":
			return conn.EndCursor
		case "startCursor":
			return conn.EndCursor
		}
	case "PullRequest":
		pullRef, ok := ref.(*gqlPullRef)
		if !ok {
			break
		}
		pull := pullRef.Pull
		switch field {
		case "id":
			return gqlGlobalID("PullRequest", pull.ID)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			return gqlPullState(pull)
		case "merged":
			return pull.Merged
		case "mergeable":
			return "MERGEABLE"
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "files":
			baseCommit := gqlCommitOf(pullRef.Repo, pull.BaseSha)
			headCommit := gqlCommitOf(pullRef.Repo, pull.HeadSha)
			files := gqlDiffFiles(pullRef.Repo, baseCommit, headCommit)
			values := make([]interface{}, 0, len(files))
			for i := range files {
				values = append(values, gqlWrap("PullRequestFile", &files[i]))
			}
			items, hasNext, endCursor := gqlConnPage(values, args)
			return gqlWrap("PullRequestFileConnection", &gqlConnRef{Nodes: items, HasNext: hasNext, EndCursor: endCursor})
		case "author":
			return gqlWrap("User", Viewer)
		case "createdAt":
			return pull.CreatedAt
		case "updatedAt":
			return pull.UpdatedAt
		}
	case "PullRequestFile":
		file, ok := ref.(*DiffFile)
		if !ok {
			break
		}
		switch field {
		case "path":
			return file.Filename
		case "additions":
			return 1
		case "deletions":
			return 0
		}
	case "__Type":
		name, ok := ref.(string)
		if !ok {
			break
		}
		switch field {
		case "name":
			return name
		case "kind":
			if _, isEnum := gqlSchemaEnums[name]; isEnum {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			names := gqlSchemaFields[name]
			out := make([]interface{}, 0, len(names))
			for _, value := range names {
				out = append(out, gqlWrap("__Field", value))
			}
			return out
		case "enumValues":
			names := gqlSchemaEnums[name]
			out := make([]interface{}, 0, len(names))
			for _, value := range names {
				out = append(out, gqlWrap("__EnumValue", value))
			}
			return out
		}
	case "__Field", "__EnumValue":
		name, ok := ref.(string)
		if !ok {
			break
		}
		switch field {
		case "name":
			return name
		case "isDeprecated":
			return false
		case "deprecationReason":
			return nil
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		payload, ok := ref.(*gqlIssueRef)
		if !ok {
			break
		}
		if field == "issue" {
			return gqlWrap("Issue", payload)
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		payload, ok := ref.(*gqlPullRef)
		if !ok {
			break
		}
		if field == "pullRequest" {
			return gqlWrap("PullRequest", payload)
		}
	case "AddCommentPayload":
		comment, ok := ref.(*Comment)
		if !ok {
			break
		}
		if field == "commentEdge" {
			return gqlWrap("CommentEdge", comment)
		}
		if field == "comment" {
			return gqlWrap("Comment", comment)
		}
	case "CommentEdge":
		comment, ok := ref.(*Comment)
		if !ok {
			break
		}
		if field == "node" {
			return gqlWrap("Comment", comment)
		}
	case "Comment":
		comment, ok := ref.(*Comment)
		if !ok {
			break
		}
		switch field {
		case "id":
			return gqlGlobalID("Comment", comment.ID)
		case "body":
			return comment.Body
		case "author":
			return gqlWrap("User", comment.User)
		case "createdAt":
			return comment.CreatedAt
		}
	case "IssueCommentConnection", "LabelConnection":
		// No resolvable fields: every selection produces the error below, as in the TS port.
	}
	ctx.errors = append(ctx.errors, map[string]interface{}{
		"message": fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field, typeName),
	})
	return nil
}

func gqlAsUser(ref interface{}) (User, bool) {
	switch v := ref.(type) {
	case User:
		return v, true
	case *User:
		return *v, true
	}
	return User{}, false
}

func gqlResolveMutation(field string, args map[string]interface{}) interface{} {
	input := map[string]interface{}{}
	if value, ok := args["input"].(map[string]interface{}); ok {
		input = value
	}
	if field == "createIssue" {
		_, id, ok := gqlDecodeID(input["repositoryId"])
		if !ok {
			return nil
		}
		repo := gqlFindRepoByID(id)
		if repo == nil {
			return nil
		}
		issue := gqlCreateIssue(repo, gqlToString(input["title"]), gqlToString(input["body"]))
		return gqlWrap("CreateIssuePayload", &gqlIssueRef{Repo: repo, Issue: issue})
	}
	if field == "closeIssue" {
		_, id, ok := gqlDecodeID(input["issueId"])
		if !ok {
			return nil
		}
		repo, issue := gqlFindIssueByID(id)
		if issue == nil {
			return nil
		}
		issue.State = "closed"
		issue.UpdatedAt = iso(tick())
		invalidateIssue(issue)
		return gqlWrap("CloseIssuePayload", &gqlIssueRef{Repo: repo, Issue: issue})
	}
	if field == "createPullRequest" {
		_, id, ok := gqlDecodeID(input["repositoryId"])
		if !ok {
			return nil
		}
		repo := gqlFindRepoByID(id)
		if repo == nil {
			return nil
		}
		created, errText := gqlCreatePull(repo, gqlToString(input["title"]), gqlToString(input["body"]), gqlToString(input["headRefName"]), gqlToString(input["baseRefName"]))
		if errText != "" {
			return nil
		}
		return gqlWrap("CreatePullRequestPayload", &gqlPullRef{Repo: repo, Pull: created})
	}
	if field == "mergePullRequest" {
		_, id, ok := gqlDecodeID(input["pullRequestId"])
		if !ok {
			return nil
		}
		repo, pull := gqlFindPullByID(id)
		if pull == nil {
			return nil
		}
		okMerge, _ := gqlMergePull(repo, pull)
		if !okMerge {
			return nil
		}
		return gqlWrap("MergePullRequestPayload", &gqlPullRef{Repo: repo, Pull: pull})
	}
	if field == "addComment" {
		_, id, ok := gqlDecodeID(input["subjectId"])
		if !ok {
			return nil
		}
		repo, issue := gqlFindIssueByID(id)
		if issue == nil {
			return nil
		}
		comment := gqlAddComment(repo, issue, gqlToString(input["body"]))
		return gqlWrap("AddCommentPayload", comment)
	}
	return nil
}

func gqlMergePull(repo *Repo, pull *Pull) (bool, string) {
	ok, _, message, _ := mergePull(repo, pull)
	return ok, message
}

func gqlExecuteSelections(parent *gqlWrapped, selections []gqlSelection, ctx *gqlExecContext, out *orderedObject) {
	for _, selection := range selections {
		if selection.kind == "fragment" {
			if selection.typeName != "" && selection.typeName != parent.Type {
				continue
			}
			gqlExecuteSelections(parent, selection.selections, ctx, out)
			continue
		}
		if selection.name == "__typename" {
			key := selection.name
			if selection.alias != "" {
				key = selection.alias
			}
			out.set(key, parent.Type)
			continue
		}
		args := gqlEvalArgs(selection.args, ctx.vars)
		value := gqlResolve(parent.Type, parent.Ref, selection.name, args, ctx)
		key := selection.name
		if selection.alias != "" {
			key = selection.alias
		}
		if len(selection.selections) == 0 {
			out.set(key, value)
			continue
		}
		if value == nil {
			out.set(key, nil)
			continue
		}
		if list, ok := value.([]interface{}); ok {
			items := make([]interface{}, 0, len(list))
			for _, item := range list {
				if wrapped, ok := gqlAsWrapped(item); ok {
					child := newOrderedObject()
					gqlExecuteSelections(wrapped, selection.selections, ctx, child)
					items = append(items, child)
				} else {
					items = append(items, item)
				}
			}
			out.set(key, items)
			continue
		}
		if wrapped, ok := gqlAsWrapped(value); ok {
			child := newOrderedObject()
			gqlExecuteSelections(wrapped, selection.selections, ctx, child)
			out.set(key, child)
			continue
		}
		out.set(key, value)
	}
}

func gqlExecuteOperation(operation gqlOperation, vars map[string]interface{}) *orderedObject {
	ctx := &gqlExecContext{vars: vars, errors: []interface{}{}}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	data := newOrderedObject()
	gqlExecuteSelections(gqlWrap(rootType, nil), operation.selections, ctx, data)
	if len(ctx.errors) > 0 {
		data.set("__errors", ctx.errors)
	}
	return data
}

func gqlErrorResult(message string) map[string]interface{} {
	return map[string]interface{}{
		"errors": []interface{}{map[string]interface{}{"message": message}},
	}
}

/** Run one GraphQL document; answers `{data, errors?}` exactly as GitHub v4 does. */
func runGraphql(query string, variables map[string]interface{}) map[string]interface{} {
	operations, err := gqlParse(query)
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	if len(operations) == 0 {
		return gqlErrorResult("no operation")
	}
	vars := variables
	if vars == nil {
		vars = map[string]interface{}{}
	}
	data := gqlExecuteOperation(operations[0], vars)
	result := map[string]interface{}{"data": data}
	if raw, ok := data.get("__errors"); ok {
		if list, ok := raw.([]interface{}); ok && len(list) > 0 {
			result["errors"] = list
		}
		data.del("__errors")
	}
	return result
}

func gqlBodyMap(value interface{}) map[string]interface{} {
	if m, ok := value.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func gqlStringField(m map[string]interface{}, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func gqlVariablesField(m map[string]interface{}) map[string]interface{} {
	if v, ok := m["variables"].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

func gqlJSONResult(status int, body interface{}) HTTPResult {
	b, _ := jsonMarshal(body)
	b = append(b, '\n')
	return HTTPResult{Status: status, ContentType: JSONType, Body: b, Headers: map[string]string{}}
}

/** POST /api/graphql: one document, or a batch array of them. */
func handleGraphql(bodyText string) HTTPResult {
	var parsed interface{}
	if err := json.Unmarshal([]byte(bodyText), &parsed); err != nil {
		return gqlJSONResult(200, gqlErrorResult("Problems parsing JSON"))
	}
	if list, ok := parsed.([]interface{}); ok {
		answers := make([]interface{}, 0, len(list))
		for _, entry := range list {
			document := gqlBodyMap(entry)
			query := gqlStringField(document, "query")
			variables := gqlVariablesField(document)
			answers = append(answers, runGraphql(query, variables))
		}
		return gqlJSONResult(200, answers)
	}
	document, ok := parsed.(map[string]interface{})
	if !ok || parsed == nil {
		return gqlJSONResult(400, gqlErrorResult("Problems parsing JSON"))
	}
	query := gqlStringField(document, "query")
	variables := gqlVariablesField(document)
	return gqlJSONResult(200, runGraphql(query, variables))
}
