// The forge's in-memory store: repositories, refs, git objects, issues, pull
// requests, comments and labels. Everything is deterministic: numeric ids and
// timestamps come from counters seeded at the scenario epoch, never the clock.

package main

import (
	"encoding/base64"
	"sort"
	"strings"
)

// Comment is one issue comment.
type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UserLogin string
}

// IssueLabel is a label attached to an issue.
type IssueLabel struct {
	Name  string
	Color string
}

// Issue is one repository issue.
type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []IssueLabel
	CreatedSeq int
	CreatedAt  string
	UpdatedAt  string
	UserLogin  string
	Comments   []*Comment
}

// Pull is one pull request.
type Pull struct {
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	HeadRef       string
	BaseRef       string
	Merged        bool
	MergedAt      string
	MergeCommitSha string
	CreatedSeq    int
	CreatedAt     string
	UserLogin     string
}

// RepoLabel is a label defined on a repository.
type RepoLabel struct {
	Name  string
	Color string
}

// Repo is one repository.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string
	Objects       map[string]*GitObject
	Labels        map[string]*RepoLabel
	LabelNames    []string // label creation order (Node's Map iteration order)
	Issues        []*Issue
	Pulls         []*Pull
	NextIssue     int
	NextPull      int
}

func repoKey(owner, name string) string {
	return owner + "/" + name
}

func b64url(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

// gid builds an opaque, stable global id (never the plain database id or number).
func gid(kind string, parts ...interface{}) string {
	strs := make([]string, len(parts))
	for i, p := range parts {
		switch v := p.(type) {
		case int:
			strs[i] = itoa(v)
		case string:
			strs[i] = v
		}
	}
	return b64url(kind + ":" + strings.Join(strs, ":"))
}

// parseGid decodes a global id into its kind and parts.
func parseGid(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	text := string(raw)
	colon := strings.Index(text, ":")
	if colon < 0 {
		return "", nil, false
	}
	return text[:colon], strings.Split(text[colon+1:], ":"), true
}

// Forge is the whole in-memory store.
type Forge struct {
	Repos        map[string]*Repo
	ReposByID    map[int]*Repo
	NextRepoID   int
	NextIssueID  int
	NextCommentID int
	Clock        DetClock
}

func newForge() *Forge {
	return &Forge{
		Repos:         map[string]*Repo{},
		ReposByID:     map[int]*Repo{},
		NextRepoID:    1,
		NextIssueID:   1,
		NextCommentID: 1,
	}
}

func (f *Forge) viewer() ScenarioUser {
	return VIEWER
}

// ------------------------------------------------------------------ repos

func (f *Forge) createRepo(owner, name string, private bool, description string) *Repo {
	if _, exists := f.Repos[repoKey(owner, name)]; exists {
		return nil
	}
	repo := &Repo{
		ID:            f.NextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: DEFAULT_BRANCH,
		Refs:          map[string]string{},
		Objects:       map[string]*GitObject{},
		Labels:        map[string]*RepoLabel{},
		LabelNames:    []string{},
		Issues:        []*Issue{},
		Pulls:         []*Pull{},
		NextIssue:     1,
		NextPull:      1,
	}
	f.NextRepoID++
	f.Repos[repoKey(owner, name)] = repo
	f.ReposByID[repo.ID] = repo
	return repo
}

func (f *Forge) getRepo(owner, name string) *Repo {
	return f.Repos[repoKey(owner, name)]
}

func (f *Forge) getRepoByID(id int) *Repo {
	return f.ReposByID[id]
}

func (f *Forge) deleteRepo(owner, name string) bool {
	repo, ok := f.Repos[repoKey(owner, name)]
	if !ok {
		return false
	}
	delete(f.Repos, repoKey(owner, name))
	delete(f.ReposByID, repo.ID)
	return true
}

func (f *Forge) listViewerRepos(owner string) []*Repo {
	out := []*Repo{}
	for _, repo := range f.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ------------------------------------------------------------------ refs

// resolveRef resolves a ref name, a full ref, a sha, or 'HEAD' to a commit oid.
func (f *Forge) resolveRef(repo *Repo, ref string) string {
	if ref == "HEAD" {
		if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return oid
		}
		return ""
	}
	if strings.HasPrefix(ref, "refs/") {
		return repo.Refs[ref]
	}
	if isObjectID(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
		return ""
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid
	}
	return repo.Refs["refs/tags/"+ref]
}

// resolveCommit returns the commit oid a ref points at, peeling annotated tags.
func (f *Forge) resolveCommit(repo *Repo, ref string) string {
	oid := f.resolveRef(repo, ref)
	if oid == "" {
		return ""
	}
	object := repo.Objects[oid]
	if object == nil {
		return ""
	}
	if object.Type == "tag" {
		return parseTag(object.Data).Object
	}
	if object.Type == "commit" {
		return oid
	}
	return ""
}

// CommitInfo is one entry of a commit listing.
type CommitInfo struct {
	SHA       string
	Message   string
	Author    string
	Committer string
}

// listCommits returns commits reachable from ref, newest first
// (tip, then parents depth-first).
func (f *Forge) listCommits(repo *Repo, ref string) []CommitInfo {
	var tip string
	if ref == "" {
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else {
		tip = f.resolveCommit(repo, ref)
	}
	if tip == "" {
		return nil
	}
	out := []CommitInfo{}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := repo.Objects[oid]
		if object == nil || object.Type != "commit" {
			continue
		}
		commit := parseCommit(object.Data)
		out = append(out, CommitInfo{SHA: oid, Message: commit.Message, Author: commit.Author, Committer: commit.Committer})
		for i := len(commit.Parents) - 1; i >= 0; i-- {
			stack = append(stack, commit.Parents[i])
		}
	}
	return out
}

// ------------------------------------------------------------------ contents

func (f *Forge) identity(epochSec int) string {
	return VIEWER.Name + " <" + VIEWER.Email + "> " + itoa(epochSec) + " +0000"
}

// commitFile commits one file change on branch (creating the branch from the
// default tip when it does not exist yet, or from nothing when the repo is
// unborn). content nil deletes the file.
func (f *Forge) commitFile(repo *Repo, branch, path string, content []byte, hasContent bool, message string) (commitSha string, blobSha string) {
	tipRef := "refs/heads/" + branch
	tip, ok := repo.Refs[tipRef]
	if !ok {
		tip, ok = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if !ok {
		tip = ""
	}
	epochSec, _ := f.Clock.Next()
	files := map[string]*FileEntry{}
	var parents []string
	if tip != "" {
		if tipObject := repo.Objects[tip]; tipObject != nil && tipObject.Type == "commit" {
			parents = append(parents, tip)
			files = flattenTree(parseCommit(tipObject.Data).Tree, func(oid string) *GitObject { return repo.Objects[oid] })
		}
	}
	if !hasContent {
		delete(files, path)
	} else {
		blobSha = putObject(repo.Objects, "blob", content)
		files[path] = &FileEntry{OID: blobSha, Mode: "100644"}
	}
	treeOID := buildTreeFromFiles(files, repo.Objects)
	who := f.identity(epochSec)
	commitData := buildCommitText(treeOID, parents, who, who, message)
	commitSha = putObject(repo.Objects, "commit", commitData)
	repo.Refs[tipRef] = commitSha
	return commitSha, blobSha
}

// filesAt returns the file map at a commit, or nil when the commit is unknown.
func (f *Forge) filesAt(repo *Repo, commitOID string) map[string]*FileEntry {
	object := repo.Objects[commitOID]
	if object == nil || object.Type != "commit" {
		return nil
	}
	return flattenTree(parseCommit(object.Data).Tree, func(oid string) *GitObject { return repo.Objects[oid] })
}

// ------------------------------------------------------------------ issues

func (f *Forge) createIssue(repo *Repo, title, body string, labelNames []string) (*Issue, string) {
	labels := []IssueLabel{}
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, "Label '" + name + "' does not exist"
		}
		labels = append(labels, IssueLabel{Name: label.Name, Color: label.Color})
	}
	_, iso := f.Clock.Next()
	issue := &Issue{
		ID:         f.NextIssueID,
		Number:     repo.NextIssue,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     labels,
		CreatedSeq: f.Clock.seq,
		CreatedAt:  iso,
		UpdatedAt:  iso,
		UserLogin:  VIEWER.Login,
		Comments:   []*Comment{},
	}
	f.NextIssueID++
	repo.NextIssue++
	repo.Issues = append(repo.Issues, issue)
	return issue, ""
}

func (f *Forge) getIssue(repo *Repo, number int) *Issue {
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

// getIssueByID is a global issue lookup: issue ids are forge-wide.
func (f *Forge) getIssueByID(id int) (*Repo, *Issue) {
	for _, repo := range f.Repos {
		for _, issue := range repo.Issues {
			if issue.ID == id {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func (f *Forge) listIssues(repo *Repo, state, sortBy, direction string) []*Issue {
	list := []*Issue{}
	for _, issue := range repo.Issues {
		if state == "all" || issue.State == state {
			list = append(list, issue)
		}
	}
	dir := 1
	if direction == "desc" {
		dir = -1
	}
	if sortBy == "created" {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].CreatedSeq != list[j].CreatedSeq {
				return (list[i].CreatedSeq-list[j].CreatedSeq)*dir < 0
			}
			return (list[i].Number-list[j].Number)*dir < 0
		})
	} else {
		sort.SliceStable(list, func(i, j int) bool {
			return (list[i].Number-list[j].Number)*dir < 0
		})
	}
	return list
}

// ------------------------------------------------------------------ pulls

func (f *Forge) createPull(repo *Repo, head, base, title, body string) (*Pull, string, int) {
	headTip := f.resolveCommit(repo, head)
	baseTip := f.resolveCommit(repo, base)
	if headTip == "" || baseTip == "" {
		return nil, "Head or base branch does not exist", 422
	}
	for _, pull := range repo.Pulls {
		if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
			return nil, "A pull request already exists for this head and base", 422
		}
	}
	_, iso := f.Clock.Next()
	pull := &Pull{
		Number:     repo.NextPull,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    head,
		BaseRef:    base,
		Merged:     false,
		MergedAt:   "",
		CreatedSeq: f.Clock.seq,
		CreatedAt:  iso,
		UserLogin:  VIEWER.Login,
	}
	repo.NextPull++
	repo.Pulls = append(repo.Pulls, pull)
	return pull, "", 0
}

func (f *Forge) getPull(repo *Repo, number int) *Pull {
	for _, pull := range repo.Pulls {
		if pull.Number == number {
			return pull
		}
	}
	return nil
}

func (f *Forge) listPulls(repo *Repo, state string) []*Pull {
	if state == "all" {
		return append([]*Pull(nil), repo.Pulls...)
	}
	out := []*Pull{}
	for _, pull := range repo.Pulls {
		if state == "closed" && pull.State == "closed" {
			out = append(out, pull)
		} else if state != "closed" && pull.State == "open" {
			out = append(out, pull)
		}
	}
	return out
}

func (f *Forge) headSha(repo *Repo, pull *Pull) string {
	return f.resolveCommit(repo, pull.HeadRef)
}

func (f *Forge) baseSha(repo *Repo, pull *Pull) string {
	return f.resolveCommit(repo, pull.BaseRef)
}

// PullFile is one changed path of a pull request.
type PullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

// pullFiles returns the changed paths of a pull request:
// diff of merge-base tree vs head tree.
func (f *Forge) pullFiles(repo *Repo, pull *Pull) []PullFile {
	headTip := f.resolveCommit(repo, pull.HeadRef)
	baseTip := f.resolveCommit(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return nil
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	mergeBase := findMergeBase(headTip, baseTip, lookup)
	baseFiles := map[string]*FileEntry{}
	if mergeBase != "" {
		if m := f.filesAt(repo, mergeBase); m != nil {
			baseFiles = m
		}
	}
	headFiles := f.filesAt(repo, headTip)
	if headFiles == nil {
		headFiles = map[string]*FileEntry{}
	}
	countLines := func(oid string) []string {
		object := repo.Objects[oid]
		if object == nil {
			return nil
		}
		text := string(object.Data)
		lines := strings.Split(text, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	pathSet := map[string]bool{}
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := []PullFile{}
	for _, path := range paths {
		before, hasBefore := baseFiles[path]
		after, hasAfter := headFiles[path]
		if hasBefore && hasAfter && before.OID == after.OID {
			continue
		}
		status := "modified"
		additions := 0
		deletions := 0
		if !hasBefore && hasAfter {
			status = "added"
			additions = len(countLines(after.OID))
		} else if hasBefore && !hasAfter {
			status = "removed"
			deletions = len(countLines(before.OID))
		} else if hasBefore && hasAfter {
			beforeLines := countLines(before.OID)
			afterLines := countLines(after.OID)
			beforeSet := map[string]int{}
			for _, line := range beforeLines {
				beforeSet[line]++
			}
			afterSet := map[string]int{}
			for _, line := range afterLines {
				afterSet[line]++
			}
			for line, count := range afterSet {
				used := count
				if b, ok := beforeSet[line]; ok && b < used {
					used = b
				}
				additions += count - used
			}
			for line, count := range beforeSet {
				used := count
				if a, ok := afterSet[line]; ok && a < used {
					used = a
				}
				deletions += count - used
			}
		}
		out = append(out, PullFile{Filename: path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

// pullConflicts reports whether the pull request conflicts now
// (both sides changed the same path differently).
func (f *Forge) pullConflicts(repo *Repo, pull *Pull) bool {
	headTip := f.resolveCommit(repo, pull.HeadRef)
	baseTip := f.resolveCommit(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return false
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	mergeBase := findMergeBase(headTip, baseTip, lookup)
	if mergeBase == "" {
		return false
	}
	baseFiles := f.filesAt(repo, mergeBase)
	if baseFiles == nil {
		baseFiles = map[string]*FileEntry{}
	}
	headFiles := f.filesAt(repo, headTip)
	if headFiles == nil {
		headFiles = map[string]*FileEntry{}
	}
	currentFiles := f.filesAt(repo, baseTip)
	if currentFiles == nil {
		currentFiles = map[string]*FileEntry{}
	}
	pathSet := map[string]bool{}
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	for p := range currentFiles {
		pathSet[p] = true
	}
	for path := range pathSet {
		var ancestor, head, current string
		if e, ok := baseFiles[path]; ok {
			ancestor = e.OID
		}
		if e, ok := headFiles[path]; ok {
			head = e.OID
		}
		if e, ok := currentFiles[path]; ok {
			current = e.OID
		}
		headChanged := head != ancestor
		currentChanged := current != ancestor
		if headChanged && currentChanged && head != current {
			return true
		}
	}
	return false
}

// mergePull squash-merges a pull request: a 3-way merge of the head's changes
// onto the base tip, one commit with the base tip as parent. Returns the merge
// commit sha, or "" when the pull request conflicts.
func (f *Forge) mergePull(repo *Repo, pull *Pull) (sha string, message string, ok bool) {
	if f.pullConflicts(repo, pull) {
		return "", "", false
	}
	headTip := f.resolveCommit(repo, pull.HeadRef)
	baseTip := f.resolveCommit(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return "", "", false
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	mergeBase := findMergeBase(headTip, baseTip, lookup)
	baseFiles := map[string]*FileEntry{}
	if mergeBase != "" {
		if m := f.filesAt(repo, mergeBase); m != nil {
			baseFiles = m
		}
	}
	headFiles := f.filesAt(repo, headTip)
	if headFiles == nil {
		headFiles = map[string]*FileEntry{}
	}
	currentFiles := f.filesAt(repo, baseTip)
	if currentFiles == nil {
		currentFiles = map[string]*FileEntry{}
	}
	merged := map[string]*FileEntry{}
	for p, e := range currentFiles {
		merged[p] = e
	}
	pathSet := map[string]bool{}
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	for path := range pathSet {
		var ancestor string
		if e, ok := baseFiles[path]; ok {
			ancestor = e.OID
		}
		head, hasHead := headFiles[path]
		var headOID string
		if hasHead {
			headOID = head.OID
		}
		if headOID == ancestor {
			continue
		}
		if !hasHead {
			delete(merged, path)
		} else {
			merged[path] = &FileEntry{OID: head.OID, Mode: head.Mode}
		}
	}
	treeOID := buildTreeFromFiles(merged, repo.Objects)
	epochSec, iso := f.Clock.Next()
	who := f.identity(epochSec)
	message = "Merge pull request #" + itoa(pull.Number) + " from " + pull.HeadRef + "\n\n" + pull.Title
	commitData := buildCommitText(treeOID, []string{baseTip}, who, who, message)
	mergeSha := putObject(repo.Objects, "commit", commitData)
	repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = iso
	pull.MergeCommitSha = mergeSha
	return mergeSha, message, true
}

// ------------------------------------------------------------------ misc

// reachableFrom returns all objects reachable from every tip.
func (f *Forge) reachableFrom(repo *Repo, tips []string) map[string]bool {
	out := map[string]bool{}
	for _, oid := range collectReachable(tips, func(oid string) *GitObject { return repo.Objects[oid] }) {
		out[oid] = true
	}
	return out
}

func (f *Forge) isCommit(repo *Repo, oid string) bool {
	object := repo.Objects[oid]
	return object != nil && object.Type == "commit"
}

func (f *Forge) objectType(repo *Repo, oid string) GitObjectType {
	if object := repo.Objects[oid]; object != nil {
		return object.Type
	}
	return ""
}

// peel peels an annotated tag to its commit, or returns the oid unchanged.
func (f *Forge) peel(repo *Repo, oid string) string {
	if object := repo.Objects[oid]; object != nil && object.Type == "tag" {
		return parseTag(object.Data).Object
	}
	return oid
}
