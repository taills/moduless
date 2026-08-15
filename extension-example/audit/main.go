// Package main 实现审计日志插件：记录网关上的每一次写请求（谁、什么路径、
// 什么状态码、什么时候），提供一个分页只读 API，并按管理员配置的保留期
// 定时清理过期记录。
//
// 这份代码只依据 docs/plugin-development.md 写成。凡是指南没有明确给出
// 类型名/字段名/方法名的地方，都在旁边用 "猜测" 标出——这些地方我没有
// 任何办法在提交前自己验证，只能照最像的模式猜一个。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
			// 猜测：sdk.PhaseLog 这个常量名不存在于指南的任何代码示例里。
			// 指南只展示过 sdk.PhasePreRoute 和 sdk.PhasePreHandler，是照
			// 同样的命名模式（Phase + 阶段名的驼峰形式）推出来的。
			sdk.PhaseLog: recordWrite,
		},
		Jobs: map[string]sdk.JobFunc{
			"purge-expired": purgeExpired,
		},
	})
}

// recordWrite 在 log 阶段跑，把这次写请求存成一条审计记录。
//
// 猜测点（指南完全没有文档化 FilterRequest 除 ClientIP /
// ResponseStatus / ResponseHeader / ResponseBody 之外的其它字段）：
//   - req.Method、req.Path 是否存在、叫这个名字 —— 纯猜测。指南从没有
//     给过一个完整的 FilterRequest 字段列表，pre_route 示例里只用到了
//     req.ClientIP。
//   - req.ResponseStatus 在 log 阶段是否有值 —— 指南原文只说
//     "post_handler 和 on_error 阶段能拿到 req.ResponseStatus"，log 阶段
//     没提。log 阶段发生在"响应已发出"之后，按道理应该也能拿到，但这是
//     推断，不是文档给的保证。
//   - sdk.User(ctx) 在 filter 的 ctx（不是从 *http.Request 来的）里能不能
//     用——指南唯一的身份读取示例是在 HTTP handler 里写
//     sdk.User(r.Context()).Username，从没在 filter 里出现过。
func recordWrite(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	entry := AuditEntry{
		ID:     newID(),
		User:   sdk.User(ctx).Username, // 猜测：ctx 在 filter 里也能喂给 sdk.User
		Method: req.Method,             // 猜测：字段名未在指南中出现
		Path:   req.Path,               // 猜测：字段名未在指南中出现
		Status: req.ResponseStatus,     // 猜测：log 阶段是否填充未被文档确认
		At:     time.Now().UTC().Format(time.RFC3339),
	}

	if _, err := sdk.DB.Put(ctx, collection, entry.ID, entry); err != nil {
		// log 阶段是异步的、不影响响应，所以这里只能记日志，没有别的补救手段。
		sdk.Log.Error(ctx, "audit: failed to record entry", "err", err.Error())
	}

	// 猜测：log 阶段的返回值指南完全没讨论过意义是什么（响应已经发出去了，
	// Stop/Continue 还有意义吗？）。照其它阶段的签名要求，返回 Continue()。
	return sdk.Continue(), nil
}

// onConfigChanged 在插件启动时和管理员每次改配置时都会被调用一次
// （指南原文如此保证），所以只需要这一条代码路径。
func onConfigChanged(cfg map[string]string) {
	days := defaultRetentionDays

	// 猜测：manifest.yaml 里没有任何字段能声明这个插件用到哪些配置键、
	// 它们的类型或默认值——指南的 manifest 参考里根本没有 config: 这一节。
	// 只能假设管理员在控制台上是通过一个通用的自由格式键值编辑器来填，
	// 而 "retention_days" 这个键名完全是插件作者和管理员之间口头的约定，
	// 没有任何机制帮你们对齐拼写。
	if v, ok := cfg["retention_days"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		} else {
			// 指南明确要求："无法解析的配置值应当回落到默认值，而不是关掉这项功能"
			sdk.Log.Error(context.Background(), "audit: invalid retention_days, using default",
				"value", v, "default", defaultRetentionDays)
		}
	}

	cfgMu.Lock()
	retentionDays = days
	cfgMu.Unlock()
}

// purgeExpired 是 manifest 里 jobs: 声明的 "purge-expired" 定时任务的实现。
func purgeExpired(ctx context.Context, job *sdk.Job) error {
	cfgMu.RLock()
	days := retentionDays
	cfgMu.RUnlock()

	// 用 job.Scheduled 而不是 time.Now()，按指南的说法：这是这次运行
	// "本该"发生的时刻，跟实际执行时间之间的漂移不应该影响清理的边界。
	cutoff := time.Unix(job.Scheduled, 0).UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// 猜测：指南从没展示过"按查询条件批量删除"的 API，也没展示过脱离
	//事务的单条删除——唯一出现过的删除方法是事务里的 tx.Delete。这里
	// 只能假设 sdk.DB 也有一个不需要事务的 Delete，而且假设 Where(...)
	// 支持 Lt()（指南只示范过 Eq 和 Gt，没有 Lt，但两者对称，应该存在）。
	for {
		var stale []AuditEntry
		_, err := sdk.DB.Where(collection).
			Lt("at", cutoff). // 猜测：Lt 方法未在指南中出现过
			SortDesc("at").
			Limit(200).
			All(ctx, &stale)
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
// 猜测/已知缺口：
//   - manifest 里 menus.roles: [admin] 只影响这个入口在控制台菜单里是否
//     可见（指南原文："非空时只有该角色可见（Core 侧过滤，不下发）"），
//     和这条 HTTP 路由本身要不要做鉴权是两回事。指南完全没有给出任何
//     "在插件自己的 handler 里检查当前调用者角色/权限" 的 API，所以下面
//     故意没有做权限检查——不是因为不需要，是因为指南没给我能做这件事
//     的工具。这行为上等于：只要有人知道 /entries 这个 URL，不管是不是
//     admin 都能读到全部审计记录。
func listEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	query := sdk.DB.Where(collection)
	if user := q.Get("user"); user != "" {
		query = query.Eq("user", user)
	}
	query = query.SortDesc("at").Limit(limit)

	// 猜测：指南说 All() 会返回一个 next 游标用于翻页，但从没show过怎么把
	// 这个游标喂回下一次查询——没有 .After()/.Cursor()/.Since() 之类的
	// 方法出现在任何示例里。这里猜的是 .After(cursor)。
	if cursor := q.Get("cursor"); cursor != "" {
		query = query.After(cursor)
	}

	var entries []AuditEntry
	next, err := query.All(ctx, &entries)
	if err != nil {
		sdk.Log.Error(ctx, "audit: list query failed", "err", err.Error())
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// 猜测：指南没有规定插件 HTTP API 的响应信封格式，这里是我自己拍的。
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": entries,
		"next":    next,
	})
}

// newID 生成一个审计记录的主键。指南没有提供任何 ID 生成辅助函数
// （对比：sdk.DB.Put(ctx, "notes", id, note) 的例子里 id 是从哪来的完全
// 没交代），所以只能自己手搓一个，不引入没见过在这个项目里用过的第三方
// uuid 依赖。
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
