// Package main 实现审计日志插件：记录网关上的每一次写请求（谁、什么路径、
// 什么状态码、什么时候），提供一个分页只读 API，并按管理员配置的保留期
// 定时清理过期记录。
//
// 这是第三个示例，展示 log 阶段的 filter：它不提供路由给用户，也不拦截
// 请求，只是在响应发出之后把发生过的事记下来。
//
// 它最初是一次实验的产物：由一个只被允许读 docs/plugin-development.md 的
// 作者写成，用来找出指南没讲清楚的地方。它当时编译不过，而每一处失败都
// 对应指南的一个缺口 —— 那些缺口后来都补上了，见 README.md。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// AuditEntry 是存进文档存储的一条审计记录。
type AuditEntry struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	At     string `json:"at"` // RFC3339，字典序即时间序，才能用 Lt/Gt 比较
}

const (
	collection           = "audit_log"
	defaultRetentionDays = 30
)

var (
	cfgMu         sync.RWMutex
	retentionDays = defaultRetentionDays
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /entries", listEntries)

	sdk.Serve(sdk.Config{
		Handler:         mux,
		OnConfigChanged: onConfigChanged,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhaseLog: recordWrite,
		},
		Jobs: map[string]sdk.JobFunc{
			"purge-expired": purgeExpired,
		},
	})
}

// recordWrite 在 log 阶段跑，把这次写请求存成一条审计记录。
//
// log 阶段在响应发出之后异步执行，所以这里做多慢都不影响用户；返回值被
// 忽略，`Continue()` 只是这个阶段唯一有意义的答复。
//
// 用 sdk.User(ctx).Name() 而不是 .Username：匿名请求下 User 返回 nil，
// 直接取字段会 panic，而插件里的 panic 会杀掉进程 —— 一个未登录的请求
// 就能让审计停摆。
func recordWrite(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	entry := AuditEntry{
		ID:     sdk.NewID(),
		User:   sdk.User(ctx).Name(),
		Method: req.Method,
		Path:   req.Path,
		Status: req.ResponseStatus,
		At:     time.Now().UTC().Format(time.RFC3339),
	}

	if _, err := sdk.DB.Put(ctx, collection, entry.ID, entry); err != nil {
		// log 阶段是异步的、不影响响应，所以这里只能记日志，没有别的补救手段。
		sdk.Log.Error(ctx, "audit: failed to record entry", "err", err.Error())
	}

	// log 阶段的返回值会被忽略（响应早就发出去了），Continue 只是这个
	// 签名下唯一说得通的答复。
	return sdk.Continue(), nil
}

// onConfigChanged 在插件启动时被调用一次（带着管理员当前的配置），之后
// 每次改动再调用一次 —— 所以配置只需要这一条代码路径。
func onConfigChanged(cfg map[string]string) {
	days := defaultRetentionDays

	// retention_days 在 manifest 的 config: 段里声明过，所以 Core 会把
	// 声明的默认值补进来，控制台也按声明渲染表单。仍然要自己解析，因为
	// 实际生效的是管理员键入的字符串。
	if v, ok := cfg["retention_days"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		} else {
			// 解析不了就用默认值，而不是把清理关掉：控制台上的一个笔误
			// 不该变成"审计记录永远不过期"。
			sdk.Log.Error(context.Background(), "audit: invalid retention_days, using default",
				"value", v, "default", defaultRetentionDays)
		}
	}

	cfgMu.Lock()
	retentionDays = days
	cfgMu.Unlock()
}

// expiredQuery is the page of entries old enough to drop.
//
// Split out so the cutoff can be checked without a store behind it: the
// arithmetic is the part worth testing, and getting it wrong deletes either too
// much of the record or none of it.
//
// scheduled is job.Scheduled rather than time.Now(): it is the occurrence this
// run is for, so a job that ran ten minutes late does not move the retention
// boundary ten minutes with it.
//
// Comparisons are strings, so "at" is stored as RFC3339 rather than unix
// seconds — lexical order has to equal chronological order for Lt to mean what
// it says.
func expiredQuery(scheduled int64, days int) *sdk.Query {
	cutoff := time.Unix(scheduled, 0).UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	return sdk.DB.Where(collection).Lt("at", cutoff).SortDesc("at").Limit(200)
}

// purgeExpired 是 manifest 里 jobs: 声明的 "purge-expired" 定时任务的实现。
func purgeExpired(ctx context.Context, job *sdk.Job) error {
	cfgMu.RLock()
	days := retentionDays
	cfgMu.RUnlock()

	// 分页查出来再逐条删，而不是塞进一个事务：事务有超时，而要删的行数
	// 没有上限。
	for {
		var stale []AuditEntry
		_, err := expiredQuery(job.Scheduled, days).All(ctx, &stale)
		if err != nil {
			return err
		}
		if len(stale) == 0 {
			return nil
		}
		for _, e := range stale {
			if _, err := sdk.DB.Delete(ctx, collection, e.ID); err != nil {
				return err
			}
		}
	}
}

// listEntries 是操作员分页查看审计记录的只读接口，可选按 user 过滤，
// 新记录在前。
//
// 鉴权在这里做，不是靠 manifest 里的 menus.roles —— 那个只决定菜单项
// 显示给谁，任何知道 URL 的人都能直接请求这条路由。
func listEntries(w http.ResponseWriter, r *http.Request) {
	// The audit trail is admin-only. A menu's roles: [admin] does not do this
	// — that only decides who sees the menu item, and anyone able to type the
	// URL reaches the route regardless.
	if !sdk.User(r.Context()).HasRole("admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ctx := r.Context()

	var entries []AuditEntry
	next, err := entriesQuery(r.URL.Query()).All(ctx, &entries)
	if err != nil {
		sdk.Log.Error(ctx, "audit: list query failed", "err", err.Error())
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": entries,
		"next":    next,
	})
}

// pageLimit clamps the caller's page size.
//
// Anything unparseable, zero, negative or above the ceiling falls back to the
// default rather than being taken literally: a limit of 0 would return nothing
// and read as an empty audit trail, and an unbounded one hands the caller the
// whole table.
func pageLimit(raw string) int {
	if raw == "" {
		return defaultPageLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxPageLimit {
		return defaultPageLimit
	}
	return n
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// entriesQuery builds the operator's view: newest first, optionally one user,
// one page at a time.
func entriesQuery(q url.Values) *sdk.Query {
	query := sdk.DB.Where(collection)
	if user := q.Get("user"); user != "" {
		query = query.Eq("user", user)
	}
	query = query.SortDesc("at").Limit(pageLimit(q.Get("limit")))

	// The cursor from the previous page's "next" goes back in here.
	if cursor := q.Get("cursor"); cursor != "" {
		query = query.After(cursor)
	}
	return query
}
