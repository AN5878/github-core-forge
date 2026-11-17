// The forge: one process serving git smart HTTP, REST v3, GraphQL v4 and MCP
// from a single in-memory store. It binds an ephemeral port, prints the port
// on stdout for the launcher, and serves until it is killed.

package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

// mu guards the store. Reads run concurrently under RLock; writes take the
// exclusive Lock. Every read path is a pure read of the store: no read
// handler advances the deterministic clock, mutates a repo, or writes a
// shared map — the only shared mutable structure a read touches is the fetch
// cache, which is a sync.Map (see githttp.go), and the request-id counter,
// which is atomic. Body reads and socket writes happen outside the lock, one
// goroutine per connection.
var mu sync.RWMutex

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("listening on port %d\n", port)

	ctx := &Ctx{
		Forge: newForge(),
		Token: os.Getenv("MAKEFUN_SHOP_TOKEN"),
	}
	serve(ln, ctx, route)
}

func route(ctx *Ctx, req *Request) Response {
	if isReadOnly(req) {
		mu.RLock()
		defer mu.RUnlock()
		return dispatch(ctx, req)
	}
	mu.Lock()
	defer mu.Unlock()
	return dispatch(ctx, req)
}

// isReadOnly reports whether a request can only read the store. GET/HEAD
// never mutate, and git upload-pack (ls-refs / fetch) is a pure read; every
// other method (REST creates/updates/deletes, GraphQL, MCP, receive-pack)
// can mutate and takes the exclusive lock.
func isReadOnly(req *Request) bool {
	if req.Method == "GET" || req.Method == "HEAD" {
		return true
	}
	if req.Method == "POST" && strings.Contains(req.Path, ".git/git-upload-pack") {
		return true
	}
	return false
}

func dispatch(ctx *Ctx, req *Request) Response {
	pathname := req.Path
	method := req.Method

	if method == "GET" && pathname == "/healthz" {
		return jsonResponse(200, map[string]interface{}{"ok": true}, nil)
	}

	if pathname == "/api/graphql" || strings.HasPrefix(pathname, "/api/graphql/") {
		if method != "POST" {
			return jsonResponse(404, map[string]interface{}{"message": "Not Found"}, [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}})
		}
		if !checkAuth(req, ctx) {
			return jsonResponse(401, map[string]interface{}{"message": "Unauthorized"}, [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}})
		}
		return handleGraphQL(req, ctx)
	}

	if pathname == "/mcp" {
		if !checkAuth(req, ctx) {
			text := marshalJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32001, "message": "Unauthorized"},
			})
			return textResponse(401, text, "application/json", nil)
		}
		return handleMcp(req, ctx)
	}

	if strings.HasPrefix(pathname, "/api/v3") {
		return handleRest(req, ctx)
	}

	if strings.Contains(pathname, ".git/") || strings.HasSuffix(pathname, ".git") {
		return handleGit(req, ctx)
	}

	return jsonResponse(404, map[string]interface{}{"message": "Not Found"}, [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}})
}
