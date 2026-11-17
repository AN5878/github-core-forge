// REST v3 under /api/v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Every reply is application/json with a
// non-empty X-GitHub-Request-Id; unknown routes are JSON 404s; malformed JSON
// bodies are 400s; missing or wrong tokens are 401s.

package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// issueCreateInput is the create-issue body ({title, body, labels}): a struct
// decode is much cheaper than the interface{} decode, and the write workload
// posts exactly this shape.
type issueCreateInput struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

func restErr(ctx *Ctx, status int, message string) Response {
	return jsonResponse(status, map[string]interface{}{"message": message}, [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}})
}

func restOK(ctx *Ctx, status int, body interface{}, extra [][2]string) Response {
	headers := [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}}
	headers = append(headers, extra...)
	return jsonResponse(status, body, headers)
}

// ---------------------------------------------------------------- JSON shapes

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoJSONOut struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	HasIssues     bool     `json:"has_issues"`
	Fork          bool     `json:"fork"`
}

func repoJSON(repo *Repo) repoJSONOut {
	return repoJSONOut{
		ID:            repo.ID,
		NodeID:        gid("Repository", repo.ID),
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         userJSON{Login: repo.Owner, ID: 1000, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		HasIssues:     true,
		Fork:          false,
	}
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueJSONOut struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
}

func issueJSON(issue *Issue) issueJSONOut {
	labels := []labelJSON{}
	for _, l := range issue.Labels {
		labels = append(labels, labelJSON{Name: l.Name, Color: l.Color})
	}
	return issueJSONOut{
		ID:        issue.ID,
		NodeID:    gid("Issue", issue.ID),
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      userJSON{Login: issue.UserLogin, ID: 1000, Type: "User"},
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
	}
}

type pullRefJSON struct {
	Ref   string   `json:"ref"`
	SHA   string   `json:"sha"`
	Label string   `json:"label"`
	User  userJSON `json:"user"`
}

type pullJSONOut struct {
	ID             int         `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSHA *string     `json:"merge_commit_sha"`
	User           userJSON    `json:"user"`
	CreatedAt      string      `json:"created_at"`
}

func pullJSON(ctx *Ctx, repo *Repo, pull *Pull) pullJSONOut {
	var mergedAt, mergeSHA *string
	if pull.MergedAt != "" {
		mergedAt = &pull.MergedAt
	}
	if pull.MergeCommitSha != "" {
		mergeSHA = &pull.MergeCommitSha
	}
	owner := userJSON{Login: repo.Owner, ID: 1000, Type: "User"}
	return pullJSONOut{
		ID:     pull.Number,
		NodeID: gid("PullRequest", repo.ID, pull.Number),
		Number: pull.Number,
		Title:  pull.Title,
		Body:   pull.Body,
		State:  pull.State,
		Head: pullRefJSON{
			Ref:   pull.HeadRef,
			SHA:   ctx.Forge.headSha(repo, pull),
			Label: repo.Owner + ":" + pull.HeadRef,
			User:  owner,
		},
		Base: pullRefJSON{
			Ref:   pull.BaseRef,
			SHA:   ctx.Forge.baseSha(repo, pull),
			Label: repo.Owner + ":" + pull.BaseRef,
			User:  owner,
		},
		Merged:         pull.Merged,
		MergedAt:       mergedAt,
		MergeCommitSHA: mergeSHA,
		User:           userJSON{Login: pull.UserLogin, ID: 1000, Type: "User"},
		CreatedAt:      pull.CreatedAt,
	}
}

var identRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

type ident struct {
	Name  string
	Email string
	Date  string
}

func parseIdent(line string) ident {
	match := identRe.FindStringSubmatch(line)
	if match == nil {
		return ident{Name: line}
	}
	sec, _ := strconv.ParseInt(match[3], 10, 64)
	return ident{
		Name:  match[1],
		Email: match[2],
		Date:  time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
}

// parseJSInt mimics Number.parseInt(s, 10): leading digits, NaN when none.
func parseJSInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	n := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// ---------------------------------------------------------------- router

func handleRest(req *Request, ctx *Ctx) Response {
	method := req.Method
	segments := pathSegments(req.Path)
	var rest []string
	if len(segments) > 2 {
		rest = segments[2:]
	}
	query := req.Query

	if !checkAuth(req, ctx) {
		return restErr(ctx, 401, "Bad credentials")
	}

	// Body: GET/HEAD read none; DELETE reads its body when present; the rest
	// parse and answer 400 on malformed JSON.
	var input map[string]interface{}
	if method == "POST" && len(rest) == 4 && rest[0] == "repos" && rest[3] == "issues" {
		// Fast path: issue create carries only {title, body, labels}; a
		// struct decode is much cheaper than the interface{} decode. Any
		// mismatch (wrong types, malformed JSON, trailing data) falls through
		// to the generic parse below, preserving the exact old behaviour.
		var s issueCreateInput
		if len(req.Body) > 0 && json.Unmarshal(req.Body, &s) == nil {
			input = map[string]interface{}{"title": s.Title, "body": s.Body}
			if len(s.Labels) > 0 {
				arr := make([]interface{}, len(s.Labels))
				for i, l := range s.Labels {
					arr[i] = l
				}
				input["labels"] = arr
			}
		}
	}
	if input == nil {
		var body interface{} = map[string]interface{}{}
		if method != "GET" && method != "HEAD" {
			if len(req.Body) > 0 || method != "DELETE" {
				parsed, ok := parseJSONBody(req.Body)
				if !ok {
					return restErr(ctx, 400, "Problems parsing JSON")
				}
				body = parsed
			}
		}
		input, _ = isRecord(body)
		if input == nil {
			input = map[string]interface{}{}
		}
	}

	// ---------------------------------------------------------- /api/v3/ root
	if len(rest) == 0 {
		if method != "GET" {
			return restErr(ctx, 404, "Not Found")
		}
		return restOK(ctx, 200, map[string]interface{}{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
			"user_url":         "/api/v3/users/{user}",
		}, [][2]string{{"X-OAuth-Scopes", "repo, read:org"}})
	}

	// ---------------------------------------------------------- /meta
	if rest[0] == "meta" && len(rest) == 1 {
		if method != "GET" {
			return restErr(ctx, 404, "Not Found")
		}
		return restOK(ctx, 200, map[string]interface{}{"installed_version": "1.0.0"}, nil)
	}

	// ---------------------------------------------------------- /user
	if rest[0] == "user" {
		if len(rest) == 1 {
			if method != "GET" {
				return restErr(ctx, 404, "Not Found")
			}
			return restOK(ctx, 200, map[string]interface{}{
				"login": VIEWER.Login, "id": VIEWER.ID, "name": VIEWER.Name, "email": VIEWER.Email, "type": "User",
			}, nil)
		}
		if len(rest) >= 2 && rest[1] == "repos" {
			if method == "POST" {
				name := asString(input["name"])
				if name == "" {
					return restErr(ctx, 422, "name is required")
				}
				repo := ctx.Forge.createRepo(VIEWER.Login, name, asBool(input["private"]), asString(input["description"]))
				if repo == nil {
					return restErr(ctx, 422, "Repository already exists")
				}
				return restOK(ctx, 201, repoJSON(repo), nil)
			}
			if method == "GET" {
				params := pageParams(query, 30)
				repos := ctx.Forge.listViewerRepos(VIEWER.Login)
				page, link := paginate(repos, params, req.Host, req.Path, query)
				out := make([]repoJSONOut, 0, len(page))
				for _, r := range page {
					out = append(out, repoJSON(r))
				}
				var extra [][2]string
				if link != "" {
					extra = [][2]string{{"Link", link}}
				}
				return restOK(ctx, 200, out, extra)
			}
		}
		return restErr(ctx, 404, "Not Found")
	}

	// ---------------------------------------------------------- /repos/{owner}/{name}/...
	if rest[0] == "repos" && len(rest) >= 3 {
		owner := rest[1]
		name := rest[2]
		tail := rest[3:]
		if owner != VIEWER.Login {
			return restErr(ctx, 404, "Not Found")
		}
		repo := ctx.Forge.getRepo(owner, name)
		if len(tail) == 0 {
			if repo == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "GET" {
				return restOK(ctx, 200, repoJSON(repo), nil)
			}
			if method == "PATCH" {
				if desc, ok := input["description"].(string); ok {
					repo.Description = desc
				}
				if priv, ok := input["private"].(bool); ok {
					repo.Private = priv
				}
				return restOK(ctx, 200, repoJSON(repo), nil)
			}
			if method == "DELETE" {
				ctx.Forge.deleteRepo(owner, name)
				return Response{Status: 204, Headers: [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}}, Body: nil}
			}
			return restErr(ctx, 404, "Not Found")
		}
		if repo == nil {
			return restErr(ctx, 404, "Not Found")
		}
		return handleRepoTail(req, ctx, repo, tail, input, method)
	}

	return restErr(ctx, 404, "Not Found")
}

func handleRepoTail(req *Request, ctx *Ctx, repo *Repo, tail []string, input map[string]interface{}, method string) Response {
	forge := ctx.Forge
	head := tail[0]
	q := req.Query

	// ---------------------------------------------------------- branches
	if head == "branches" {
		if len(tail) == 1 {
			if method != "GET" {
				return restErr(ctx, 404, "Not Found")
			}
			names := make([]string, 0, len(repo.Refs))
			for ref := range repo.Refs {
				names = append(names, ref)
			}
			sort.Strings(names)
			branches := []map[string]interface{}{}
			for _, ref := range names {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				branches = append(branches, map[string]interface{}{
					"name":   ref[len("refs/heads/"):],
					"commit": map[string]interface{}{"sha": repo.Refs[ref]},
				})
			}
			return restOK(ctx, 200, branches, nil)
		}
		branch := strings.Join(tail[1:], "/")
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return restErr(ctx, 404, "Not Found")
		}
		return restOK(ctx, 200, map[string]interface{}{
			"name":   branch,
			"commit": map[string]interface{}{"sha": sha},
		}, nil)
	}

	// ---------------------------------------------------------- commits
	if head == "commits" {
		if method != "GET" {
			return restErr(ctx, 404, "Not Found")
		}
		params := pageParams(q, 30)
		commits := forge.listCommits(repo, q.Get("sha"))
		page, link := paginate(commits, params, req.Host, req.Path, q)
		out := []map[string]interface{}{}
		for _, commit := range page {
			author := parseIdent(commit.Author)
			committer := parseIdent(commit.Committer)
			out = append(out, map[string]interface{}{
				"sha": commit.SHA,
				"commit": map[string]interface{}{
					"message": commit.Message,
					"author": map[string]interface{}{
						"name": author.Name, "email": author.Email, "date": author.Date,
					},
					"committer": map[string]interface{}{
						"name": committer.Name, "email": committer.Email, "date": committer.Date,
					},
				},
			})
		}
		var extra [][2]string
		if link != "" {
			extra = [][2]string{{"Link", link}}
		}
		return restOK(ctx, 200, out, extra)
	}

	// ---------------------------------------------------------- git/refs
	if head == "git" && len(tail) > 1 && tail[1] == "refs" {
		if method == "POST" && len(tail) == 2 {
			ref := asString(input["ref"])
			sha := asString(input["sha"])
			if !strings.HasPrefix(ref, "refs/") || repo.Objects[sha] == nil {
				return restErr(ctx, 422, "Invalid ref or sha")
			}
			if _, ok := repo.Refs[ref]; ok {
				return restErr(ctx, 422, "Reference already exists")
			}
			repo.Refs[ref] = sha
			return restOK(ctx, 201, map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": sha},
			}, nil)
		}
		if method == "GET" {
			if len(tail) == 2 {
				names := make([]string, 0, len(repo.Refs))
				for ref := range repo.Refs {
					names = append(names, ref)
				}
				sort.Strings(names)
				refs := []map[string]interface{}{}
				for _, ref := range names {
					refs = append(refs, map[string]interface{}{
						"ref":    ref,
						"object": map[string]interface{}{"sha": repo.Refs[ref]},
					})
				}
				return restOK(ctx, 200, refs, nil)
			}
			ref := "refs/" + strings.Join(tail[2:], "/")
			sha, ok := repo.Refs[ref]
			if !ok {
				return restErr(ctx, 404, "Not Found")
			}
			return restOK(ctx, 200, map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": sha},
			}, nil)
		}
		return restErr(ctx, 404, "Not Found")
	}

	// ---------------------------------------------------------- contents
	if head == "contents" {
		path := strings.Join(tail[1:], "/")
		refParam := q.Get("ref")
		_, hasRef := q["ref"]
		var commitOID string
		if !hasRef {
			commitOID = repo.Refs["refs/heads/"+repo.DefaultBranch]
		} else {
			commitOID = forge.resolveCommit(repo, refParam)
		}
		if method == "GET" {
			if commitOID == "" {
				return restErr(ctx, 404, "Not Found")
			}
			files := forge.filesAt(repo, commitOID)
			if files == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if path == "" {
				return restOK(ctx, 200, dirEntries(repo, files, ""), nil)
			}
			if file, ok := files[path]; ok {
				object := repo.Objects[file.OID]
				var data []byte
				if object != nil {
					data = object.Data
				}
				return restOK(ctx, 200, map[string]interface{}{
					"type":     "file",
					"encoding": "base64",
					"content":  base64.StdEncoding.EncodeToString(data),
					"sha":      file.OID,
					"size":     len(data),
					"name":     path[strings.LastIndex(path, "/")+1:],
					"path":     path,
				}, nil)
			}
			prefix := path + "/"
			isDir := false
			for key := range files {
				if strings.HasPrefix(key, prefix) {
					isDir = true
					break
				}
			}
			if isDir {
				return restOK(ctx, 200, dirEntries(repo, files, path), nil)
			}
			return restErr(ctx, 404, "Not Found")
		}
		if method == "PUT" {
			message := asString(input["message"])
			contentB64 := asString(input["content"])
			if message == "" || contentB64 == "" {
				return restErr(ctx, 422, "message and content are required")
			}
			branch := asString(input["branch"])
			if branch == "" {
				branch = repo.DefaultBranch
			}
			var branchFiles map[string]*FileEntry
			if branchTip, ok := repo.Refs["refs/heads/"+branch]; ok {
				branchFiles = forge.filesAt(repo, branchTip)
			}
			var current *FileEntry
			if branchFiles != nil {
				current = branchFiles[path]
			}
			sha, hasSHA := input["sha"].(string)
			if current != nil {
				if !hasSHA {
					return restErr(ctx, 422, "sha must be provided to update a file")
				}
				if sha != current.OID {
					return restErr(ctx, 409, "sha does not match the current file")
				}
			} else if hasSHA {
				return restErr(ctx, 422, "sha does not match any existing file")
			}
			content, _ := base64.StdEncoding.DecodeString(contentB64)
			commitSha, blobSha := forge.commitFile(repo, branch, path, content, true, message)
			status := 201
			if current != nil {
				status = 200
			}
			return restOK(ctx, status, map[string]interface{}{
				"content": map[string]interface{}{
					"name": path[strings.LastIndex(path, "/")+1:], "path": path, "sha": blobSha,
					"size": len(content), "encoding": "base64", "type": "file",
				},
				"commit": map[string]interface{}{"sha": commitSha, "message": message},
			}, nil)
		}
		if method == "DELETE" {
			message := asString(input["message"])
			branch := asString(input["branch"])
			if branch == "" {
				branch = repo.DefaultBranch
			}
			var branchFiles map[string]*FileEntry
			if branchTip, ok := repo.Refs["refs/heads/"+branch]; ok {
				branchFiles = forge.filesAt(repo, branchTip)
			}
			var current *FileEntry
			if branchFiles != nil {
				current = branchFiles[path]
			}
			if current == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if sha, hasSHA := input["sha"].(string); hasSHA && sha != current.OID {
				return restErr(ctx, 409, "sha does not match the current file")
			}
			if message == "" {
				message = "delete"
			}
			commitSha, _ := forge.commitFile(repo, branch, path, nil, false, message)
			return restOK(ctx, 200, map[string]interface{}{
				"commit": map[string]interface{}{"sha": commitSha},
			}, nil)
		}
		return restErr(ctx, 404, "Not Found")
	}

	// ---------------------------------------------------------- labels
	if head == "labels" {
		if method == "POST" {
			name := asString(input["name"])
			color := asString(input["color"])
			if color == "" {
				color = "ededed"
			}
			if name == "" {
				return restErr(ctx, 422, "name is required")
			}
			if _, ok := repo.Labels[name]; ok {
				return restErr(ctx, 422, "Label already exists")
			}
			repo.Labels[name] = &RepoLabel{Name: name, Color: color}
			repo.LabelNames = append(repo.LabelNames, name)
			return restOK(ctx, 201, map[string]interface{}{"name": name, "color": color}, nil)
		}
		if method == "GET" {
			out := []map[string]interface{}{}
			for _, name := range repo.LabelNames {
				l := repo.Labels[name]
				out = append(out, map[string]interface{}{"name": l.Name, "color": l.Color})
			}
			return restOK(ctx, 200, out, nil)
		}
		return restErr(ctx, 404, "Not Found")
	}

	// ---------------------------------------------------------- issues
	if head == "issues" {
		if len(tail) == 1 {
			if method == "POST" {
				title := asString(input["title"])
				if title == "" {
					return restErr(ctx, 422, "title is required")
				}
				labels := []string{}
				if arr, ok := input["labels"].([]interface{}); ok {
					for _, entry := range arr {
						if s, ok := entry.(string); ok {
							labels = append(labels, s)
						}
					}
				}
				created, errMsg := forge.createIssue(repo, title, asString(input["body"]), labels)
				if errMsg != "" {
					return restErr(ctx, 422, errMsg)
				}
				return restOKBytes(ctx, 201, issueJSONBytes(created), nil)
			}
			if method == "GET" {
				state := q.Get("state")
				if state == "" {
					state = "open"
				}
				sortBy := q.Get("sort")
				if sortBy == "" {
					sortBy = "created"
				}
				direction := q.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				params := pageParams(q, 30)
				issues := forge.listIssues(repo, state, sortBy, direction)
				page, link := paginate(issues, params, req.Host, req.Path, q)
				out := make([]issueJSONOut, 0, len(page))
				for _, issue := range page {
					out = append(out, issueJSON(issue))
				}
				var extra [][2]string
				if link != "" {
					extra = [][2]string{{"Link", link}}
				}
				return restOK(ctx, 200, out, extra)
			}
			return restErr(ctx, 404, "Not Found")
		}
		number, ok := parseJSInt(tail[1])
		if !ok {
			return restErr(ctx, 404, "Not Found")
		}
		issue := forge.getIssue(repo, number)
		if len(tail) == 2 {
			if issue == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "GET" {
				return restOKBytes(ctx, 200, issueJSONBytes(issue), nil)
			}
			if method == "PATCH" {
				if state, ok := input["state"].(string); ok && (state == "open" || state == "closed") {
					issue.State = state
					_, iso := forge.Clock.Next()
					issue.UpdatedAt = iso
				}
				if title, ok := input["title"].(string); ok {
					issue.Title = title
				}
				if body, ok := input["body"].(string); ok {
					issue.Body = body
				}
				return restOKBytes(ctx, 200, issueJSONBytes(issue), nil)
			}
			return restErr(ctx, 404, "Not Found")
		}
		if len(tail) >= 3 && tail[2] == "comments" {
			if issue == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "POST" {
				bodyText := asString(input["body"])
				if bodyText == "" {
					return restErr(ctx, 422, "body is required")
				}
				_, iso := forge.Clock.Next()
				comment := &Comment{ID: forge.NextCommentID, Body: bodyText, CreatedAt: iso, UserLogin: VIEWER.Login}
				forge.NextCommentID++
				issue.Comments = append(issue.Comments, comment)
				return restOK(ctx, 201, map[string]interface{}{
					"id": comment.ID, "body": comment.Body, "created_at": comment.CreatedAt,
					"user": map[string]interface{}{"login": comment.UserLogin, "id": 1000, "type": "User"},
				}, nil)
			}
			if method == "GET" {
				out := []map[string]interface{}{}
				for _, comment := range issue.Comments {
					out = append(out, map[string]interface{}{
						"id": comment.ID, "body": comment.Body, "created_at": comment.CreatedAt,
						"user": map[string]interface{}{"login": comment.UserLogin, "id": 1000, "type": "User"},
					})
				}
				return restOK(ctx, 200, out, nil)
			}
		}
		return restErr(ctx, 404, "Not Found")
	}

	// ---------------------------------------------------------- pulls
	if head == "pulls" {
		if len(tail) == 1 {
			if method == "POST" {
				title := asString(input["title"])
				headRef := asString(input["head"])
				baseRef := asString(input["base"])
				if title == "" || headRef == "" || baseRef == "" {
					return restErr(ctx, 422, "title, head and base are required")
				}
				created, errMsg, status := forge.createPull(repo, headRef, baseRef, title, asString(input["body"]))
				if errMsg != "" {
					return restErr(ctx, status, errMsg)
				}
				return restOK(ctx, 201, pullJSON(ctx, repo, created), nil)
			}
			if method == "GET" {
				state := q.Get("state")
				if state == "" {
					state = "open"
				}
				params := pageParams(q, 30)
				pulls := forge.listPulls(repo, state)
				page, link := paginate(pulls, params, req.Host, req.Path, q)
				out := make([]pullJSONOut, 0, len(page))
				for _, pull := range page {
					out = append(out, pullJSON(ctx, repo, pull))
				}
				var extra [][2]string
				if link != "" {
					extra = [][2]string{{"Link", link}}
				}
				return restOK(ctx, 200, out, extra)
			}
			return restErr(ctx, 404, "Not Found")
		}
		number, ok := parseJSInt(tail[1])
		if !ok {
			return restErr(ctx, 404, "Not Found")
		}
		pull := forge.getPull(repo, number)
		if len(tail) == 2 {
			if pull == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "GET" {
				return restOK(ctx, 200, pullJSON(ctx, repo, pull), nil)
			}
			return restErr(ctx, 404, "Not Found")
		}
		if len(tail) >= 3 && tail[2] == "files" {
			if pull == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "GET" {
				files := forge.pullFiles(repo, pull)
				out := []map[string]interface{}{}
				for _, f := range files {
					out = append(out, map[string]interface{}{
						"filename": f.Filename, "status": f.Status,
						"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
					})
				}
				return restOK(ctx, 200, out, nil)
			}
			return restErr(ctx, 404, "Not Found")
		}
		if len(tail) >= 3 && tail[2] == "merge" {
			if pull == nil {
				return restErr(ctx, 404, "Not Found")
			}
			if method == "PUT" {
				if pull.Merged {
					return restErr(ctx, 409, "Pull request already merged")
				}
				if pull.State == "closed" {
					return restErr(ctx, 409, "Pull request is closed")
				}
				sha, message, merged := forge.mergePull(repo, pull)
				if !merged {
					return restErr(ctx, 405, "Pull request is conflicted")
				}
				return restOK(ctx, 200, map[string]interface{}{
					"merged": true, "sha": sha, "message": message,
				}, nil)
			}
			return restErr(ctx, 404, "Not Found")
		}
		return restErr(ctx, 404, "Not Found")
	}

	return restErr(ctx, 404, "Not Found")
}

// dirEntry is one directory listing entry.
type dirEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size *int   `json:"size,omitempty"`
}

// dirEntries lists the entries of dir ('' for the root) from a flat file map.
func dirEntries(repo *Repo, files map[string]*FileEntry, dir string) []dirEntry {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	type seenEntry struct {
		typ  string
		oid  string
		size int
	}
	seen := map[string]*seenEntry{}
	for path, entry := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			size := 0
			if object := repo.Objects[entry.OID]; object != nil {
				size = len(object.Data)
			}
			seen[rest] = &seenEntry{typ: "file", oid: entry.OID, size: size}
		} else {
			name := rest[:slash]
			if _, ok := seen[name]; !ok {
				dirPath := name
				if dir != "" {
					dirPath = dir + "/" + name
				}
				seen[name] = &seenEntry{typ: "dir", oid: dirTreeOID(repo, dirPath), size: 0}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []dirEntry{}
	for _, name := range names {
		entry := seen[name]
		e := dirEntry{Type: entry.typ, Name: name, Path: prefix + name, SHA: entry.oid}
		if entry.typ == "file" {
			size := entry.size
			e.Size = &size
		}
		out = append(out, e)
	}
	return out
}

// dirTreeOID returns the tree oid of a directory path at the repo's default
// tip ('' for the root).
func dirTreeOID(repo *Repo, dir string) string {
	tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if !ok {
		return ""
	}
	object := repo.Objects[tip]
	if object == nil || object.Type != "commit" {
		return ""
	}
	treeOID := parseCommit(object.Data).Tree
	if dir == "" {
		return treeOID
	}
	for _, segment := range strings.Split(dir, "/") {
		tree := repo.Objects[treeOID]
		if tree == nil || tree.Type != "tree" {
			return ""
		}
		found := ""
		for _, entry := range parseTree(tree.Data) {
			if entry.Name == segment && entry.Mode == "40000" {
				found = entry.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		treeOID = found
	}
	return treeOID
}
