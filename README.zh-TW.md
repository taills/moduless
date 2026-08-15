# moduless

[English](README.md) · [简体中文](README.zh-CN.md) · **繁體中文**

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

一套功能由外掛構成的 Go Web 閘道。外掛是獨立行程，由 Core 啟動、監管、熱載入與熱更新 —— 升級期間不會掉任何一個請求。

外掛提供自己的 HTTP API、自帶微前端，並且能像 IIS Filter 那樣介入閘道中**任何**請求的生命週期。啟用外掛時它的選單會出現在主控台，停用時立即消失，不需要重新整理頁面。

```
瀏覽器 ──HTTP──▶ Core (:80)
                    ├─ Filter 管線     pre_route → authenticate → authorize →
                    │                  pre_handler → [後端] → post_handler → log
                    ├─ /api/plugins/*   外掛自己的 HTTP API
                    ├─ /plugins/*       外掛的微前端
                    └─ PluginHost ──exec──▶ 外掛子行程 ×N
                         │  HashiCorp go-plugin，走 unix socket
                         ▼
                       HostServices：文件儲存 · 持久化佇列 · 快取 · 鎖
                       設定 · 檔案 · 對外 HTTP · 事件 · 日誌與指標
```

Core 只監聽一個埠，外掛一個都不開。

## 為什麼用子行程

Core 以 `exec` 啟動每個外掛，這是其餘能力得以成立的前提：

- **熱載入、熱卸載、熱更新。** 行程歸 Core 所有，因此它能先啟動新版本、健康檢查通過後原子式切換流量、再排空舊版本。實測：切換期間持續施加流量，**零失敗請求**。
- **當機隔離且可自癒。** 外掛 panic 不會波及 Core。監管器以指數退避重新啟動它，反覆當機的則進入隔離。
- **沒有對外暴露面。** 沒有任何埠可以連到外掛，也不需要註冊協定來驗證身分 —— Core 就是它的父行程。

## Filter

外掛在 manifest 中宣告關心哪些階段與路徑，Core 會把它編譯成比對表。沒有人訂閱的請求幾乎不花成本：

| 情境 | 成本 |
|---|---|
| 該階段無人訂閱 | **1.9 ns**，不產生任何記憶體配置 |
| 有訂閱但路徑不符 | **8.2 ns**，不產生任何記憶體配置 |
| 真正跨行程呼叫一次 filter | ~37,000 ns |

Filter 預設 fail-open —— 多數 filter 是觀察者，一個壞掉的觀察者不該讓整個站台停擺。任何做安全決策的 filter 必須明確宣告 `fail_closed`。

## 一個外掛長這樣

```go
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /notes", listNotes)

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePreRoute: rateLimit,
		},
		Jobs: map[string]sdk.JobFunc{
			"nightly-summary": summarise,
		},
	})
}
```

`sdk.Serve` 接收標準的 `http.Handler`，因此任何路由套件與中介層都能直接使用。外掛能觸及的一切 —— 文件儲存、佇列、快取、鎖、檔案、對外 HTTP —— 都經過 Core，而且每次呼叫都會自動帶上該請求的 trace id，因此一次慢查詢能追溯到引發它的那個請求。

完整指南：[docs/plugin-development.md](docs/plugin-development.md)。
可執行範例：[`extension-example/plugin`](extension-example/plugin)。

## 執行

```bash
git clone git@github.com:taills/moduless.git
cd moduless

# 建置主控台（一次即可）
cd core/frontend && npm install && npm run build && cd ../..

# 把範例外掛建置到外掛目錄
mkdir -p plugins/notes/bin
CGO_ENABLED=0 go build -o plugins/notes/bin/plugin ./extension-example/plugin
cp extension-example/plugin/manifest.yaml plugins/notes/

# 啟動。未設定 DATABASE_URL 時，資料、佇列與檔案能力會回報 Unavailable，其餘照常運作
PLUGIN_DIR=./plugins go run ./core
```

或使用 Docker：

```bash
docker compose up --build   # 主控台 http://localhost:8080，admin / admin123
```

## 資料

外掛從不直接連線 PostgreSQL。它在 `manifest.yaml` 中宣告 collection，由 Core 負責建立資料表，存取一律透過文件儲存進行 —— 支援排序、游標分頁、彙總、批次寫入、交易與樂觀鎖。

採用游標分頁而非 OFFSET 是刻意的：OFFSET 會讓資料庫逐列跳過並丟棄，翻得越深越慢，而且期間若有列被新增或刪除，會造成重複或遺漏。

持久化佇列以 PostgreSQL 為基礎 —— 至少一次投遞，具備重試、退避、死信、延遲訊息與去重，不需要在部署中再增加一個中介軟體。

## 信任模型

外掛由使用者審核後安裝，以 Core 自身的權限執行 —— 就像 ISAPI Filter 執行在 IIS 工作行程之中。安全性來自「只安裝你信任的外掛」。

Core 確實會在自己這一側強制以下各項：外掛宣告的權限集合、文件／快取／佇列／檔案依外掛隔離的命名空間、交易歸屬、對外 HTTP 白名單（包含拒絕解析到私有或連結本地位址的目標），以及二進位檔的 SHA-256 校驗。

但它不會限制檔案系統、CPU 或系統呼叫。真正不受信任的程式碼應當放在容器邊界之後。

## 測試

```bash
go test ./... -race

# 資料庫相關測試在未設定時會自動略過
TEST_DATABASE_URL='postgres://postgres:pass@localhost:5432/test?sslmode=disable' go test ./...
```

端對端測試會 fork 真實的外掛行程並以真實 HTTP 驅動它們，其中包含持續加壓下的熱更新，以及一次刻意製造的當機。

## 授權

Apache 2.0 —— 見 [`LICENSE`](LICENSE)。相依套件授權審查見 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
