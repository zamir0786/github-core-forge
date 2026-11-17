package main

// The MCP endpoint: the modern 2026-07-28 revision (server/discover + stateless requests), a tools
// surface over the same store, and the JSON-RPC error behaviour the checks require. Ported from
// bot/mcp.ts.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const mcpProtocolVersion = "2026-07-28"

var mcpServerInfo = map[string]interface{}{"name": "makefun-forge", "version": "1.0.0"}

const mcpServerDescription = "A GitHub-compatible forge: repositories, issues and pull requests over one shared store."

// ---------------------------------------------------------------- tool schemas

func mcpSchemaString(description string) *orderedObject {
	schema := newOrderedObject()
	schema.set("type", "string")
	schema.set("description", description)
	return schema
}

func mcpSchemaNumber(description string) *orderedObject {
	schema := newOrderedObject()
	schema.set("type", "number")
	schema.set("description", description)
	return schema
}

func mcpSchemaBool(description string) *orderedObject {
	schema := newOrderedObject()
	schema.set("type", "boolean")
	schema.set("description", description)
	return schema
}

type mcpToolSpec struct {
	name     string
	required []string
	def      *orderedObject
}

func mcpToolSpecOf(name, description string, properties *orderedObject, required []string) mcpToolSpec {
	inputSchema := newOrderedObject()
	inputSchema.set("type", "object")
	inputSchema.set("properties", properties)
	inputSchema.set("required", required)
	inputSchema.set("additionalProperties", false)

	def := newOrderedObject()
	def.set("name", name)
	def.set("description", description)
	def.set("inputSchema", inputSchema)
	return mcpToolSpec{name: name, required: required, def: def}
}

var mcpTools = []mcpToolSpec{
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login; defaults to the authenticated user."))
		properties.set("name", mcpSchemaString("The repository name."))
		properties.set("description", mcpSchemaString("A short description."))
		properties.set("private", mcpSchemaBool("Whether the repository is private."))
		return mcpToolSpecOf("create_repo", "Create a repository owned by the authenticated user.", properties, []string{"name"})
	}(),
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login."))
		properties.set("repo", mcpSchemaString("The repository name."))
		return mcpToolSpecOf("get_repo", "Read one repository by owner and name.", properties, []string{"owner", "repo"})
	}(),
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login."))
		properties.set("repo", mcpSchemaString("The repository name."))
		properties.set("title", mcpSchemaString("The issue title."))
		properties.set("body", mcpSchemaString("The issue body."))
		return mcpToolSpecOf("create_issue", "Create an issue in a repository.", properties, []string{"owner", "repo", "title"})
	}(),
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login."))
		properties.set("repo", mcpSchemaString("The repository name."))
		properties.set("state", mcpSchemaString("open, closed or all."))
		return mcpToolSpecOf("list_issues", "List the issues of a repository, optionally filtered by state.", properties, []string{"owner", "repo"})
	}(),
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login."))
		properties.set("repo", mcpSchemaString("The repository name."))
		properties.set("title", mcpSchemaString("The pull request title."))
		properties.set("body", mcpSchemaString("The pull request body."))
		properties.set("head", mcpSchemaString("The source branch."))
		properties.set("base", mcpSchemaString("The target branch."))
		return mcpToolSpecOf("create_pull_request", "Open a pull request from a head branch into a base branch.", properties, []string{"owner", "repo", "head", "base"})
	}(),
	func() mcpToolSpec {
		properties := newOrderedObject()
		properties.set("owner", mcpSchemaString("The owner login."))
		properties.set("repo", mcpSchemaString("The repository name."))
		properties.set("pull_number", mcpSchemaNumber("The pull request number."))
		return mcpToolSpecOf("merge_pull_request", "Merge a pull request into its base branch.", properties, []string{"owner", "repo", "pull_number"})
	}(),
}

// Serialized once so two tools/list answers carry the same bytes.
func mcpBuildToolsResult() string {
	defs := make([]interface{}, 0, len(mcpTools))
	for _, tool := range mcpTools {
		defs = append(defs, tool.def)
	}
	body, _ := jsonMarshal(map[string]interface{}{"tools": defs})
	return string(body)
}

var mcpToolsResult = mcpBuildToolsResult()

// ---------------------------------------------------------------- helpers

func mcpBodyMap(value interface{}) map[string]interface{} { return gqlBodyMap(value) }

func mcpArgString(args map[string]interface{}, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func mcpJSString(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return "undefined"
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
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		b, err := jsonMarshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}

func mcpResolveRepo(args map[string]interface{}) *Repo {
	owner := mcpArgString(args, "owner")
	if owner == "" {
		owner = Viewer.Login
	}
	name := mcpArgString(args, "repo")
	if name == "" {
		name = mcpArgString(args, "repository")
	}
	if name == "" {
		name = mcpArgString(args, "name")
	}
	return findRepo(owner, name)
}

func mcpToolResult(value interface{}) *orderedObject {
	text, _ := jsonMarshal(value)
	out := newOrderedObject()
	out.set("content", []interface{}{map[string]interface{}{"type": "text", "text": string(text)}})
	out.set("structuredContent", value)
	return out
}

func mcpToolError(message string) *orderedObject {
	out := newOrderedObject()
	out.set("content", []interface{}{map[string]interface{}{"type": "text", "text": message}})
	out.set("isError", true)
	return out
}

// ---------------------------------------------------------------- store adapters

func mcpCreateRepo(owner, name, description string, isPrivate bool) *Repo {
	return createRepo(owner, name, description, isPrivate)
}

func mcpRepoJSON(repo *Repo) interface{}                { return repoJSON(repo) }
func mcpIssueJSON(issue *Issue, repo *Repo) interface{} { return issueJSON(issue, repo) }
func mcpPullJSON(pull *Pull, repo *Repo) interface{}    { return pullJSON(pull, repo) }

func mcpCallTool(name string, args map[string]interface{}) *orderedObject {
	if name == "create_repo" {
		owner := mcpArgString(args, "owner")
		if owner == "" {
			owner = Viewer.Login
		}
		repoName := mcpArgString(args, "name")
		if repoName == "" {
			repoName = mcpArgString(args, "repo")
		}
		if repoName == "" {
			return mcpToolError("name is required")
		}
		if existing := findRepo(owner, repoName); existing != nil {
			return mcpToolResult(mcpRepoJSON(existing))
		}
		created := mcpCreateRepo(owner, repoName, mcpArgString(args, "description"), args["private"] == true)
		return mcpToolResult(mcpRepoJSON(created))
	}
	if name == "get_repo" {
		repo := mcpResolveRepo(args)
		if repo == nil {
			owner := mcpArgString(args, "owner")
			if owner == "" {
				owner = Viewer.Login
			}
			return mcpToolError("Repository " + owner + "/" + mcpArgString(args, "repo") + " was not found")
		}
		return mcpToolResult(mcpRepoJSON(repo))
	}
	if name == "create_issue" {
		repo := mcpResolveRepo(args)
		if repo == nil {
			return mcpToolError("repository not found")
		}
		issue := gqlCreateIssue(repo, mcpArgString(args, "title"), mcpArgString(args, "body"))
		return mcpToolResult(mcpIssueJSON(issue, repo))
	}
	if name == "list_issues" {
		repo := mcpResolveRepo(args)
		if repo == nil {
			return mcpToolError("repository not found")
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		list := make([]interface{}, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				list = append(list, mcpIssueJSON(issue, repo))
			}
		}
		return mcpToolResult(list)
	}
	if name == "create_pull_request" {
		repo := mcpResolveRepo(args)
		if repo == nil {
			return mcpToolError("repository not found")
		}
		created, errText := gqlCreatePull(repo, mcpArgString(args, "title"), mcpArgString(args, "body"), mcpArgString(args, "head"), mcpArgString(args, "base"))
		if errText != "" {
			return mcpToolError(errText)
		}
		return mcpToolResult(mcpPullJSON(created, repo))
	}
	if name == "merge_pull_request" {
		repo := mcpResolveRepo(args)
		if repo == nil {
			return mcpToolError("repository not found")
		}
		raw, exists := args["pull_number"]
		if !exists || raw == nil {
			raw = args["number"]
		}
		if raw == nil {
			raw = args["pull_request_number"]
		}
		number := 0
		switch v := raw.(type) {
		case float64:
			number = int(v)
		case int:
			number = v
		default:
			number, _ = gqlParseIntPrefix(mcpJSString(raw))
		}
		var pull *Pull
		for _, entry := range repo.Pulls {
			if entry.Number == number {
				pull = entry
				break
			}
		}
		if pull == nil {
			return mcpToolError("pull request " + mcpJSString(raw) + " not found")
		}
		okMerge, message := gqlMergePull(repo, pull)
		if !okMerge {
			return mcpToolError(message)
		}
		return mcpToolResult(mcpPullJSON(pull, repo))
	}
	return mcpToolError("unknown tool " + name)
}

// ---------------------------------------------------------------- JSON-RPC responses

func mcpID(id interface{}) interface{} {
	if id == nil {
		return nil
	}
	return id
}

func mcpRPCResult(status int, id interface{}, result interface{}) HTTPResult {
	body := newOrderedObject()
	body.set("jsonrpc", "2.0")
	body.set("id", mcpID(id))
	body.set("result", result)
	b, _ := jsonMarshal(body)
	b = append(b, '\n')
	return HTTPResult{Status: status, ContentType: JSONType, Body: b, Headers: map[string]string{}}
}

func mcpRPCError(status int, id interface{}, code int, message string) HTTPResult {
	detail := newOrderedObject()
	detail.set("code", code)
	detail.set("message", message)
	body := newOrderedObject()
	body.set("jsonrpc", "2.0")
	body.set("id", mcpID(id))
	body.set("error", detail)
	b, _ := jsonMarshal(body)
	b = append(b, '\n')
	return HTTPResult{Status: status, ContentType: JSONType, Body: b, Headers: map[string]string{}}
}

func mcpCapabilities() *orderedObject {
	tools := newOrderedObject()
	tools.set("listChanged", false)
	capabilities := newOrderedObject()
	capabilities.set("tools", tools)
	return capabilities
}

/** POST /mcp. Authentication is decided by the caller; this handles the JSON-RPC exchange. */
func handleMcp(bodyText string) HTTPResult {
	var parsed interface{}
	if err := json.Unmarshal([]byte(bodyText), &parsed); err != nil {
		return mcpRPCError(400, nil, -32700, "Parse error: the request body is not JSON")
	}
	request := mcpBodyMap(parsed)
	method, _ := request["method"].(string)
	id := request["id"]
	params := mcpBodyMap(request["params"])

	if method == "" {
		return mcpRPCError(200, id, -32600, "Invalid request: method is required")
	}
	if strings.HasPrefix(method, "notifications/") {
		return HTTPResult{Status: 202, Headers: map[string]string{}}
	}
	if method == "server/discover" {
		meta := newOrderedObject()
		meta.set("io.modelcontextprotocol/protocolVersion", mcpProtocolVersion)
		meta.set("io.modelcontextprotocol/serverInfo", mcpServerInfo)
		result := newOrderedObject()
		result.set("supportedVersions", []interface{}{mcpProtocolVersion})
		result.set("capabilities", mcpCapabilities())
		result.set("serverInfo", mcpServerInfo)
		result.set("instructions", mcpServerDescription)
		result.set("_meta", meta)
		return mcpRPCResult(200, id, result)
	}
	if method == "initialize" {
		requested, ok := params["protocolVersion"].(string)
		if !ok {
			requested = mcpProtocolVersion
		}
		result := newOrderedObject()
		result.set("protocolVersion", requested)
		result.set("capabilities", mcpCapabilities())
		result.set("serverInfo", mcpServerInfo)
		result.set("instructions", mcpServerDescription)
		return mcpRPCResult(200, id, result)
	}
	if method == "ping" {
		return mcpRPCResult(200, id, newOrderedObject())
	}
	if method == "tools/list" {
		// The tools array is serialized once, so two answers carry the same bytes.
		idJSON, _ := jsonMarshal(mcpID(id))
		body := `{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":` + mcpToolsResult + `}`
		return HTTPResult{Status: 200, ContentType: JSONType, Body: []byte(body), Headers: map[string]string{}}
	}
	if method == "tools/call" {
		name, _ := params["name"].(string)
		args := mcpBodyMap(params["arguments"])
		var spec *mcpToolSpec
		for i := range mcpTools {
			if mcpTools[i].name == name {
				spec = &mcpTools[i]
				break
			}
		}
		if spec == nil {
			return mcpRPCError(200, id, -32602, "Unknown tool: "+name)
		}
		missing := make([]string, 0, len(spec.required))
		for _, key := range spec.required {
			value, exists := args[key]
			if !exists || value == nil {
				missing = append(missing, key)
				continue
			}
			if s, ok := value.(string); ok && s == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return mcpRPCError(200, id, -32602, "Missing required argument(s): "+strings.Join(missing, ", "))
		}
		return mcpRPCResult(200, id, mcpCallTool(name, args))
	}
	return mcpRPCError(200, id, -32601, "Method not found: "+method)
}
