package main

// Shared data model. This mirrors bot/store.ts and bot/objects.ts exactly so each
// surface (git, REST, GraphQL, MCP) reads and writes one deterministic in-memory store.

import "sync/atomic"

type User struct {
	ID    int
	Login string
	Name  string
	Email string
	Type  string
}

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []Label
	CreatedAt string
	UpdatedAt string
	Comments  int
	Cache     string
	// Full is the complete serialized HTTP response (status line + headers +
	// body) and is invalidated by Store(nil) on every mutation. It is immutable
	// after creation.
	Full atomic.Pointer[[]byte]
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
	User      User
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	CreatedAt      string
	UpdatedAt      string
	User           User
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type GitObject struct {
	Type string // blob | tree | commit | tag
	Data []byte
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
	Headers   []string
}

type CommitFields struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

type FlatEntry struct {
	Mode string
	OID  string
}

type PackEntry struct {
	OID  string
	Type string
	Data []byte
}

type PackReadResult struct {
	Objects []PackEntry
	OK      bool
	Error   string
}

type DiffFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Patch     string
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Objects       map[string]GitObject
	Refs          map[string]string
	Labels        map[string]Label
	Issues        []*Issue
	IssueSeq      int
	Pulls         []*Pull
	PullSeq       int
	Comments      map[int][]Comment
	// Rev is bumped whenever refs or objects change. Git responses are cached
	// per (repo, rev, request) and published through atomic pointers.
	Rev      atomic.Uint64
	Rendered atomic.Pointer[gitRendered]
}

type Store struct {
	Repos      map[string]*Repo
	CommentSeq int
	RepoSeq    int
	IssueIDSeq int
	PullIDSeq  int
	LabelSeq   int
	RequestSeq int
	ClockTick  int
}

// GlobalStore is the single deterministic in-memory forge shared by every surface.
var GlobalStore = Store{
	Repos:      make(map[string]*Repo),
	CommentSeq: 1000,
	RepoSeq:    5000,
	IssueIDSeq: 200000,
	PullIDSeq:  300000,
	LabelSeq:   1,
}

// HTTPResult is the surface every ported handler returns; server.go writes it once.
type HTTPResult struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
	CacheKey    string // when set, server.go caches the complete HTTP response bytes
	// Prebuilt is a complete serialized HTTP response. When set, serveConn writes
	// these bytes directly with no header building and no copying.
	Prebuilt []byte
	// Scratch marks Prebuilt as backed by the connection's reusable response
	// buffer. serveConn adopts any growth after the Write completes.
	Scratch bool
}

const ZeroOID = "0000000000000000000000000000000000000000"
const JSONType = "application/json; charset=utf-8"
