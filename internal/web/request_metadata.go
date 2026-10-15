package web

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/basecamp/once-campfire-go/internal/database"
	"github.com/basecamp/once-campfire-go/internal/useragent"
)

type requestInfoKey struct{}

// One request-owned context value replaces separate host/origin values. Parsing
// stays lazy for non-browser endpoints, and derived contexts share the result.
type requestInfo struct {
	host, origin    string
	response        *responseRound
	databaseVersion uint64
	snapshot        database.Snapshot
	routed          bool
	routeMethod     string
	routePath       string
	route           *routeContract
	routeParams     map[string]string
	routeErr        error
	agentOnce       sync.Once
	agent           *useragent.Agent
}

func requestMetadata(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

func requestAgent(r *http.Request) useragent.Agent {
	if info := requestMetadata(r.Context()); info != nil {
		info.agentOnce.Do(func() {
			agent := parseAgent(r.UserAgent())
			info.agent = &agent
		})
		return *info.agent
	}
	// Direct rendering and background helpers do not have an HTTP entry context.
	return useragent.Parse(r.UserAgent())
}

// recognizeRequest memoizes the route match for the request's current method and
// path. Matching scans every contract regexp, and a request asks several times.
func recognizeRequest(r *http.Request) (*routeContract, map[string]string, error) {
	path := r.URL.EscapedPath()
	info := requestMetadata(r.Context())
	if info == nil {
		return recognize(r.Method, path)
	}
	if !info.routed || info.routeMethod != r.Method || info.routePath != path {
		info.route, info.routeParams, info.routeErr = recognize(r.Method, path)
		info.routed, info.routeMethod, info.routePath = true, r.Method, path
	}
	return info.route, info.routeParams, info.routeErr
}

const agentCacheLimit = 1024

var (
	agents     sync.Map // user agent string -> useragent.Agent
	agentCount atomic.Int64
)

// Browsers repeat a handful of User-Agent strings; parsing one costs more than a lookup.
// Agent values are never modified after Parse returns.
func parseAgent(raw string) useragent.Agent {
	if len(raw) > 512 {
		return useragent.Parse(raw)
	}
	if cached, ok := agents.Load(raw); ok {
		return cached.(useragent.Agent)
	}
	agent := useragent.Parse(raw)
	if agentCount.Add(1) > agentCacheLimit {
		agents.Clear()
		agentCount.Store(1)
	}
	agents.Store(raw, agent)
	return agent
}
