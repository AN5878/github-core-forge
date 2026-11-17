// MCP over Streamable HTTP (POST /mcp): JSON-RPC 2.0, both eras — modern
// `server/discover` (2026-07-28) and the legacy `initialize` handshake. Tools act
// on the same store as REST, so effects are visible over REST.

package main

import (
	"regexp"
	"strconv"
	"strings"
)

const serverName = "github-core-clone-forge"
const serverVersion = "1.0.0"

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	Required    []string
}

func objectSchema(properties map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

func strProp(description string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": description}
}

func boolProp(description string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": description}
}

func intProp(description string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": description}
}

var tools = []toolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		InputSchema: objectSchema(map[string]interface{}{
			"name":        strProp("Repository name"),
			"description": strProp("Repository description"),
			"private":     boolProp("Whether the repository is private"),
		}, []string{"name"}),
		Required: []string{"name"},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
		}, []string{"owner", "repo"}),
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Issue title"),
			"body":  strProp("Issue body"),
		}, []string{"owner", "repo", "title"}),
		Required: []string{"owner", "repo", "title"},
	},
	{
		Name:        "list_issues",
		Description: "List issues of a repository.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"state": strProp("Issue state: open, closed or all"),
		}, []string{"owner", "repo"}),
		Required: []string{"owner", "repo"},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Pull request title"),
			"body":  strProp("Pull request body"),
			"head":  strProp("Head branch name"),
			"base":  strProp("Base branch name"),
		}, []string{"owner", "repo", "title", "head", "base"}),
		Required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash merge.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner":       strProp("Repository owner login"),
			"repo":        strProp("Repository name"),
			"pull_number": intProp("Pull request number"),
		}, []string{"owner", "repo", "pull_number"}),
		Required: []string{"owner", "repo", "pull_number"},
	},
	{
		Name:        "get_issue",
		Description: "Get one issue of a repository by number.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner":        strProp("Repository owner login"),
			"repo":         strProp("Repository name"),
			"issue_number": intProp("Issue number"),
		}, []string{"owner", "repo", "issue_number"}),
		Required: []string{"owner", "repo", "issue_number"},
	},
	{
		Name:        "list_pull_requests",
		Description: "List pull requests of a repository.",
		InputSchema: objectSchema(map[string]interface{}{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"state": strProp("Pull request state: open, closed or all"),
		}, []string{"owner", "repo"}),
		Required: []string{"owner", "repo"},
	},
}

type mcpRepoJSON struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
}

func mcpRepoJSONOf(repo *Repo) mcpRepoJSON {
	return mcpRepoJSON{
		ID:            repo.ID,
		NodeID:        gid("Repository", repo.ID),
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         userJSON{Login: repo.Owner, ID: 1000, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
	}
}

type mcpIssueJSON struct {
	ID     int         `json:"id"`
	NodeID string      `json:"node_id"`
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Body   string      `json:"body"`
	State  string      `json:"state"`
	Labels []labelJSON `json:"labels"`
}

func mcpIssueJSONOf(issue *Issue) mcpIssueJSON {
	labels := []labelJSON{}
	for _, l := range issue.Labels {
		labels = append(labels, labelJSON{Name: l.Name, Color: l.Color})
	}
	return mcpIssueJSON{
		ID:     issue.ID,
		NodeID: gid("Issue", issue.ID),
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
		State:  issue.State,
		Labels: labels,
	}
}

type mcpPullRefJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type mcpPullJSON struct {
	ID             int            `json:"id"`
	NodeID         string         `json:"node_id"`
	Number         int            `json:"number"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	State          string         `json:"state"`
	Head           mcpPullRefJSON `json:"head"`
	Base           mcpPullRefJSON `json:"base"`
	Merged         bool           `json:"merged"`
	MergedAt       *string        `json:"merged_at"`
	MergeCommitSHA *string        `json:"merge_commit_sha"`
}

func mcpPullJSONOf(forge *Forge, repo *Repo, pull *Pull) mcpPullJSON {
	var mergedAt, mergeSHA *string
	if pull.MergedAt != "" {
		mergedAt = &pull.MergedAt
	}
	if pull.MergeCommitSha != "" {
		mergeSHA = &pull.MergeCommitSha
	}
	return mcpPullJSON{
		ID:     pull.Number,
		NodeID: gid("PullRequest", repo.ID, pull.Number),
		Number: pull.Number,
		Title:  pull.Title,
		Body:   pull.Body,
		State:  pull.State,
		Head:   mcpPullRefJSON{Ref: pull.HeadRef, SHA: forge.headSha(repo, pull)},
		Base:   mcpPullRefJSON{Ref: pull.BaseRef, SHA: forge.baseSha(repo, pull)},
		Merged: pull.Merged,
		MergedAt: mergedAt,
		MergeCommitSHA: mergeSHA,
	}
}

func toolOK(value interface{}) map[string]interface{} {
	var text string
	if s, ok := value.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(value))
	}
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"structuredContent": value,
		"isError":           false,
	}
}

func toolErr(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}},
		"isError": true,
	}
}

func resolveRepoArg(forge *Forge, args map[string]interface{}) *Repo {
	owner := asString(args["owner"])
	repoName := asString(args["repo"])
	if repoName == "" {
		if fullName, ok := args["full_name"].(string); ok {
			parts := strings.Split(fullName, "/")
			repoName = strings.Join(parts[1:], "/")
		}
	}
	if owner == "" {
		return nil
	}
	return forge.getRepo(owner, repoName)
}

func dispatchTool(forge *Forge, name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "create_repo":
		repoName := asString(args["name"])
		if repoName == "" {
			return toolErr("Missing required argument: name")
		}
		repo := forge.createRepo(VIEWER.Login, repoName, asBool(args["private"]), asString(args["description"]))
		if repo == nil {
			return toolErr("Repository already exists")
		}
		return toolOK(mcpRepoJSONOf(repo))
	case "get_repo":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		return toolOK(mcpRepoJSONOf(repo))
	case "create_issue":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		title := asString(args["title"])
		if title == "" {
			return toolErr("Missing required argument: title")
		}
		created, errMsg := forge.createIssue(repo, title, asString(args["body"]), nil)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		return toolOK(mcpIssueJSONOf(created))
	case "list_issues":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		state := asString(args["state"])
		if state == "" {
			state = "open"
		}
		list := forge.listIssues(repo, state, "created", "asc")
		out := make([]mcpIssueJSON, 0, len(list))
		for _, issue := range list {
			out = append(out, mcpIssueJSONOf(issue))
		}
		return toolOK(out)
	case "create_pull_request":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		created, errMsg, _ := forge.createPull(repo, asString(args["head"]), asString(args["base"]), asString(args["title"]), asString(args["body"]))
		if errMsg != "" {
			return toolErr(errMsg)
		}
		return toolOK(mcpPullJSONOf(forge, repo, created))
	case "merge_pull_request":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		number, _ := asInt(args["pull_number"])
		pull := forge.getPull(repo, number)
		if pull == nil {
			return toolErr("Pull request not found")
		}
		if pull.Merged {
			return toolErr("Pull request already merged")
		}
		mergedSHA, _, merged := forge.mergePull(repo, pull)
		if !merged {
			return toolErr("Pull request is conflicted")
		}
		return toolOK(map[string]interface{}{"number": pull.Number, "merged": true, "state": pull.State, "sha": mergedSHA})
	case "get_issue":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		number, _ := asInt(args["issue_number"])
		issue := forge.getIssue(repo, number)
		if issue == nil {
			return toolErr("Issue not found")
		}
		return toolOK(mcpIssueJSONOf(issue))
	case "list_pull_requests":
		repo := resolveRepoArg(forge, args)
		if repo == nil {
			return toolErr("Repository not found")
		}
		state := asString(args["state"])
		if state == "" {
			state = "open"
		}
		pulls := forge.listPulls(repo, state)
		out := make([]mcpPullJSON, 0, len(pulls))
		for _, pull := range pulls {
			out = append(out, mcpPullJSONOf(forge, repo, pull))
		}
		return toolOK(out)
	default:
		return toolErr("Unknown tool: " + name)
	}
}

func rpcResult(id interface{}, result interface{}) Response {
	return jsonResponse(200, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result}, nil)
}

func rpcError(status int, id interface{}, code int, message string) Response {
	return jsonResponse(status, map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": message},
	}, nil)
}

var mcpIDRe = regexp.MustCompile(`"id"\s*:\s*("[^"]*"|-?\d+)`)

func handleMcp(req *Request, ctx *Ctx) Response {
	method := req.Method
	if method == "DELETE" {
		return Response{Status: 204, Headers: nil, Body: nil}
	}
	if method != "POST" {
		return rpcError(405, nil, -32601, "Method not allowed")
	}
	raw := string(req.Body)
	if len(req.Body) == 0 {
		return rpcError(400, nil, -32700, "Parse error")
	}
	parsed, ok := parseJSONBody(req.Body)
	if !ok {
		// The parse-error check sends truncated JSON; answer HTTP 400, JSON-RPC -32700.
		var id interface{}
		if match := mcpIDRe.FindStringSubmatch(raw); match != nil {
			token := match[1]
			if strings.HasPrefix(token, "\"") {
				id = token[1 : len(token)-1]
			} else if n, err := strconv.Atoi(token); err == nil {
				id = n
			}
		}
		return rpcError(400, id, -32700, "Parse error")
	}
	request, ok := isRecord(parsed)
	if !ok {
		var id interface{}
		if match := mcpIDRe.FindStringSubmatch(raw); match != nil {
			token := match[1]
			if strings.HasPrefix(token, "\"") {
				id = token[1 : len(token)-1]
			} else if n, err := strconv.Atoi(token); err == nil {
				id = n
			}
		}
		return rpcError(400, id, -32700, "Parse error")
	}
	id := request["id"]
	rpcMethod := asString(request["method"])
	params, _ := isRecord(request["params"])
	if params == nil {
		params = map[string]interface{}{}
	}

	if rpcMethod == "server/discover" {
		serverInfo := map[string]interface{}{"name": serverName, "version": serverVersion}
		return rpcResult(id, map[string]interface{}{
			"supportedVersions": supportedVersions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]interface{}{"io.modelcontextprotocol/serverInfo": serverInfo},
		})
	}
	if rpcMethod == "initialize" {
		wanted := asString(params["protocolVersion"])
		protocolVersion := "2026-07-28"
		for _, v := range supportedVersions {
			if v == wanted {
				protocolVersion = wanted
				break
			}
		}
		return rpcResult(id, map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": serverName, "version": serverVersion},
		})
	}
	if rpcMethod == "notifications/initialized" || strings.HasPrefix(rpcMethod, "notifications/") {
		// A notification carries no id and expects no result.
		return Response{Status: 202, Headers: [][2]string{{"Content-Type", "application/json"}, {"Content-Length", "2"}}, Body: []byte("{}")}
	}
	if rpcMethod == "ping" {
		return rpcResult(id, map[string]interface{}{})
	}
	if rpcMethod == "tools/list" {
		toolsOut := make([]map[string]interface{}, 0, len(tools))
		for _, tool := range tools {
			toolsOut = append(toolsOut, map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"inputSchema": tool.InputSchema,
			})
		}
		return rpcResult(id, map[string]interface{}{"tools": toolsOut})
	}
	if rpcMethod == "tools/call" {
		name := asString(params["name"])
		args, _ := isRecord(params["arguments"])
		if args == nil {
			args = map[string]interface{}{}
		}
		var tool *toolDef
		for i := range tools {
			if tools[i].Name == name {
				tool = &tools[i]
				break
			}
		}
		if tool == nil {
			return rpcError(200, id, -32602, "Unknown tool: "+name)
		}
		missing := []string{}
		for _, key := range tool.Required {
			v := args[key]
			if v == nil || v == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return rpcResult(id, toolErr("Missing required argument: "+strings.Join(missing, ", ")))
		}
		return rpcResult(id, dispatchTool(ctx.Forge, name, args))
	}
	return rpcError(200, id, -32601, "Method not found: "+rpcMethod)
}
