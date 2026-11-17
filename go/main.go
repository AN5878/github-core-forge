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

// mu serializes dispatch: the store is single-threaded by design (like the
// Node original), so no handler ever races another. Body reads and socket
// writes happen outside the lock, one goroutine per connection.
var mu sync.Mutex

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
	mu.Lock()
	defer mu.Unlock()
	return dispatch(ctx, req)
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
