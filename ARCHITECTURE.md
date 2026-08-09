# crit アーキテクチャ概観

crit は「単一バイナリの Go 製 CLI」で、ブラウザ UI を起動してコードレビュー（GitHub PR ライクなインラインコメント）を行うツール。フロントエンドは `embed.FS` でバイナリに埋め込まれており、npm/webpack 等のビルドステップを持たない vanilla JS。

## 1. 全体構成

```mermaid
flowchart TB
    subgraph CLI["cmd/crit（package main）"]
        Main["main.go / cli_*.go<br/>コマンド dispatch"]
    end

    subgraph Daemon["デーモンプロセス（crit _serve）"]
        Session["internal/session<br/>セッション管理・レジストリ"]
        Server["internal/server<br/>HTTP API + SSE"]
        VCS["internal/vcs<br/>git / sapling / jj 抽象化<br/>(読み取り専用: diff/status/log。commit等の書き込みは一切しない)"]
        GH["internal/github<br/>GitHub PRコメント同期<br/>(crit pull/push、git pushとは無関係)"]
        Share["internal/share<br/>crit-webとのHTTP同期<br/>(publish/pull/reshare/unpublish)"]
        LSP["internal/lsp<br/>gopls クライアント<br/>(読み取り専用: hover/definition/referencesのみ。ファイル編集はしない)"]
    end

    subgraph Web["web/（embed 済みフロントエンド）"]
        Index["index.html<br/>コードレビュー / live モード fork"]
        AppJS["app.js<br/>コードレビューモード"]
        LiveJS["live-mode*.js<br/>ライブモード"]
        Shared["crit-*.js<br/>共有モジュール群"]
    end

    Browser(["ブラウザ UI"])
    Gopls(["gopls<br/>(別プロセス。hover/definitionのみ応答)"])
    GitRepo[("ローカル Git リポジトリ<br/>(.git オブジェクト。読み取りのみ)")]
    GitHubAPI(["GitHub API<br/>(gh CLI 経由・リモート)"])
    CritWeb(["crit-web<br/>(オプトインの共有先・リモート)"])
    ReviewFiles[("~/.crit/reviews/&lt;key&gt;.json<br/>コメント・レビュー状態")]
    Sessions[("~/.crit/sessions/<br/>デーモン登録情報")]

    Main -->|"起動 / 再利用"| Session
    Session --> Server
    Session -->|"読み書き"| Sessions
    Server --> VCS -.->|"exec (子プロセス・読み取りのみ)"| GitRepo
    Server -->|"読み書き"| ReviewFiles
    Server --> GH -.->|"HTTP POST/PATCH/DELETE (外部)"| GitHubAPI
    Server --> Share -.->|"HTTP POST/PUT/GET/DELETE (外部・opt-in)"| CritWeb
    Server --> LSP -.->|"stdio JSON-RPC (子プロセス・読み取りのみ)"| Gopls

    Server -->|"埋め込み配信"| Index
    Index --> AppJS
    Index --> LiveJS
    AppJS --> Shared
    LiveJS --> Shared

    Browser <-->|"HTTP API + SSE"| Server
    Browser --- Web
```

図形の凡例: 円柱 `[( )]` = 実データが存在する場所（`~/.crit/reviews/`, `~/.crit/sessions/` は crit が書き込む所有データ、`.git` は crit が読み取るだけの他者所有データ）。角丸 `( )` = 別プロセス／リモートサービス（gopls, GitHub API, crit-web）で、crit 自身のストレージではない。破線 `-.->` は「別プロセスの起動」または「外部HTTP呼び出し」を表す（実線は自プロセス内の関数呼び出し・自己所有データへの読み書き）。

各連携先の実態:
- **`internal/vcs` → ローカル Git リポジトリ**: `git diff`/`status`/`log`/`show`/`rev-parse` 等の**読み取り専用**シェルアウトのみ。`commit`/`add`/`checkout`/`push`/`reset` は一切実行しない（sapling/jjバックエンドも同様に読み取りのみ）。
- **`internal/lsp` → gopls**: stdioでJSON-RPC（LSP）を送信するが `initialize` / `textDocument/hover` / `textDocument/definition` / `textDocument/references` / `shutdown` のみ。`workspace/applyEdit` 等の書き込み系は使わず、ファイルもgoplsの状態も変更しない。
- **`internal/github` → GitHub API**: `crit pull` は PRコメントを取得するのみ（GET相当）。`crit push` はPRへ新規レビュー投稿（POST）、返信投稿（POST）、コメント編集（PATCH）、削除（DELETE）を行う——`git push`（コード送信）とは無関係。
- **`internal/share` → crit-web**: `crit share`＝新規公開（POST `/api/reviews`）、再共有＝更新（PUT）、コメント取り込み＝取得（GET）、`crit unpublish`＝削除（DELETE）。オプトイン（`share_url`未設定なら通信しない）。

## 2. コンポーネント図（Go パッケージ依存関係）

`go list` で `internal/*` の実際の import 関係を確認した上での構成（`cmd/crit` は表示簡略化のため主要な依存のみ）。

```mermaid
flowchart TB
    subgraph Entry["エントリポイント"]
        cmd["cmd/crit<br/>(package main)"]
    end

    subgraph Core["コアオーケストレーション"]
        server["server<br/>HTTP API + SSE"]
        daemon["daemon<br/>デーモン起動・生存管理"]
        session["session<br/>セッション登録・読込"]
    end

    subgraph Domain["ドメインロジック"]
        review["review<br/>レビューファイル読み書き"]
        comment["comment<br/>headless comment CLI"]
        focus["focus<br/>file/range/stacked focus"]
        share["share<br/>crit-webとのHTTP同期"]
        github["github<br/>GitHub PRコメント同期"]
        auth["auth<br/>crit-web 認証"]
        story["story<br/>統計/セッション記録"]
        preview["preview<br/>ローカルHTMLプレビュー"]
        live["live<br/>ライブモードプロキシ"]
        lsp["lsp<br/>gopls クライアント (読み取り専用)"]
        diff["diff<br/>diffハンク計算"]
        hooks["hooks<br/>plan-hook (Claude/Codex)"]
        prompt["prompt<br/>agent向けプロンプト生成"]
    end

    subgraph Foundation["基盤"]
        vcs["vcs<br/>git/sapling/jj 抽象化 (読み取り専用)"]
        config["config<br/>2階層 config マージ"]
        clicmd["clicmd<br/>コマンド実行ラッパー"]
        browser["browser<br/>ブラウザ起動"]
        reviewpath["reviewpath<br/>レビューファイルパス解決"]
        notify["notify<br/>デスクトップ通知"]
        testutil["testutil<br/>テストヘルパー"]
    end

    cmd --> server & daemon & session & live & preview & share & github & auth & story & notify

    server --> auth & comment & config & diff & focus & github & hooks & lsp & prompt & review & session & share & story & vcs
    live --> browser & clicmd & comment & config & daemon & focus & github & review & server & session & share & testutil
    preview --> browser & clicmd & config & daemon & review & server & session & share

    comment --> clicmd & config & daemon & review & reviewpath & session & vcs
    focus --> daemon & review & session & vcs
    github --> clicmd & config & review & session & share & vcs
    share --> auth & clicmd & config & focus & review & session
    review --> clicmd & config & daemon & reviewpath & session & vcs
    session --> browser & clicmd & config & daemon & diff & reviewpath & vcs
    auth --> browser & config & vcs
    story --> session
    hooks --> prompt
    prompt --> config
    daemon --> config
    config --> vcs
    vcs --> diff
```

- **`server` が最大のハブ**: HTTP API 層は auth/comment/diff/focus/github/hooks/lsp/prompt/review/session/share/story/vcs のほぼ全ドメインパッケージに依存する（route dispatch の性質上妥当）。
- **`session` と `review` が下位共有基盤**: comment・focus・github・share・live・preview のいずれもこの2つを経由してレビューファイル・セッション状態にアクセスする。
- **`vcs` が最下層**: config・review・session・comment・focus・github・auth が git/sapling/jj 抽象化に依存し、`vcs` 自体は `diff` のみに依存する葉に近いパッケージ。
- **循環依存なし**: 上記は全て一方向（`server`→ domain → foundation）で、`internal` 配下に import サイクルは存在しない。
- **`live` と `preview` が唯一 `server` に依存する非エントリパッケージ**: どちらもレビュー用サーバーを内部で起動・再利用するため（ライブモードのiframeプロキシ、ローカルHTMLプレビュー）。

## 3. デーモンとセッションのライフサイクル

`crit` は毎回プロセスを起動し直すのではなく、cwd + 引数（+ git モードでは branch）から算出したキーでバックグラウンドデーモンを再利用する。

```mermaid
sequenceDiagram
    participant User as ユーザー
    participant CLI as crit (クライアント)
    participant Daemon as crit _serve (デーモン)
    participant Registry as ~/.crit/sessions/
    participant Review as ~/.crit/reviews/<key>.json

    User->>CLI: crit
    CLI->>Registry: 既存セッションを検索 (hash(cwd, branch/args))
    alt セッションが無い/死んでいる
        CLI->>Daemon: crit _serve を起動
        Daemon->>Daemon: HTTPポートbind → readyパイプ通知
        Daemon-->>CLI: ready (session initはバックグラウンド継続)
        CLI->>Daemon: GET /api/session をポーリング (503回避)
    else セッションが生きている
        CLI->>Daemon: round-complete を通知
    end
    Daemon->>Review: レビューファイル読み書き (200msデバウンス)
    Daemon-->>User: ブラウザを開く / SSEで変更通知
    User->>Daemon: ブラウザでコメント・Approve
    Daemon-->>CLI: /api/wait-for-event で完了をブロック解除
```

- **Lazy init + Readiness gate**: `/api/health` と `/api/qr` 以外の全エンドポイントは `SetSession()` 完了まで 503 を返す。クライアントは `/api/session` をポーリングしてから他のAPIを叩く。
- **Idle 無し**: デーモンは Ctrl+C / `crit stop` / シグナルでのみ終了する（アイドルタイムアウトなし）。
- **LSP だけ例外的に lazy start + idle shutdown**: 初回の LSP リクエストで `gopls` を起動し、10分未使用で自動終了。デーモン終了時にも kill される。

## 4. HTTP API の構造（`internal/server`）

```mermaid
flowchart LR
    subgraph SessionScoped["セッション単位 API"]
        S1["/api/session<br/>/api/config<br/>/api/review-cycle"]
        S2["/api/share, /api/share-url<br/>/api/finish, /api/round-complete"]
        S3["/api/events (SSE)<br/>/api/wait-for-event"]
        S4["/api/focus<br/>/api/branches, /api/commits"]
        S5["/api/comments<br/>/api/review-comment/{id}"]
    end

    subgraph FileScoped["ファイル単位 API (?path=X)"]
        F1["/api/file<br/>/api/file/diff"]
        F2["/api/file/comments<br/>/api/comment/{id}"]
        F3["/api/comment/{id}/replies<br/>/api/comment/{id}/resolve"]
        F4["/api/lsp/hover<br/>/api/lsp/definition<br/>/api/lsp/references"]
    end

    subgraph Static["静的配信"]
        ST1["/files/&lt;path&gt;<br/>(パストラバーサル対策)"]
        ST2["/ (埋め込みフロントエンド)"]
    end
```

- 状態変更系（POST/PUT/PATCH/DELETE）は `Sec-Fetch-Site: cross-site` を拒否（loopback への CSRF 対策）。
- `--allow-unauthenticated-network` なしでは `127.0.0.1` 以外へのバインドや `public_url` 設定を拒否。

## 5. フロントエンド：Two-Paradigm ページフォーク

`index.html` が単一の HTML シェルとして、URL パスに応じて「コードレビューモード」と「ライブモード」のどちらのスクリプト群を読み込むか分岐する。

```mermaid
flowchart TD
    Path{{"window.location.pathname"}}
    Path -->|"/live"| LiveMode["ライブモード<br/>(iframe ベースの pin レビュー)"]
    Path -->|"それ以外"| ReviewMode["コードレビューモード<br/>(ファイルツリー + diff/document view)"]

    subgraph SharedModules["共有モジュール (window.crit.*)"]
        M1["crit-shared.js<br/>テーマ/cookie/tip"]
        M2["crit-renderer.js<br/>ContentRenderer registry"]
        M3["crit-sse.js"]
        M4["crit-comment-form.js<br/>crit-comment-card.js"]
        M5["crit-settings-overlay.js<br/>crit-settings-panes.js"]
    end

    subgraph ReviewOnly["コードレビュー専用"]
        R1["crit-line-blocks.js<br/>markdown block分割"]
        R2["crit-diff-renderer.js<br/>word-level diff"]
        R3["crit-lsp.js<br/>hover / go-to-definition / find-references"]
        R4["app.js（メインロジック）"]
    end

    subgraph LiveOnly["ライブモード専用"]
        L1["live-mode.dispatch.js"]
        L2["live-mode.composer.js<br/>live-mode.panel*.js"]
        L3["live-mode.queue.js<br/>live-mode.sse.js"]
        L4["crit-agent.js / agent-*.js<br/>(注入されるiframeスクリプト)"]
        L5["live-mode.js（メインロジック）"]
    end

    ReviewMode --> SharedModules
    ReviewMode --> ReviewOnly
    LiveMode --> SharedModules
    LiveMode --> LiveOnly

    ReviewOnly -.->|"ContentRenderer.register()"| Renderer["共有UIが呼ぶ<br/>scrollToAnchor / highlightAnchor / ..."]
    LiveOnly -.->|"ContentRenderer.register()"| Renderer
```

- 各モードは `ContentRenderer` インターフェース（`scrollToAnchor`, `highlightAnchor`, `getMode` など）を実装して登録し、共有チロム（コメントカードや設定画面）がモードを意識せず呼び出せるようにしている。
- 全モジュールは IIFE + dual-export（`window.crit.<namespace>` と Node向け `module.exports`）で、ES modules は未使用（ビルドステップが無いため）。

## 6. コメント／レビューファイルの永続化

```mermaid
flowchart LR
    Browser["ブラウザ UI<br/>(コメント追加/解決)"]
    CLI2["crit comment<br/>(ヘッドレスCLI)"]
    Server2["server.go"]
    ReviewFile[("~/.crit/reviews/&lt;key&gt;.json<br/>per-file section + nested replies")]
    SSE["SSE: /api/events"]

    Browser -->|"POST/PUT/DELETE"| Server2
    CLI2 -->|"直接書き込み<br/>(サーバ不要)"| ReviewFile
    Server2 -->|"200msデバウンスで書き込み"| ReviewFile
    ReviewFile -.->|"変更通知"| SSE
    SSE -.-> Browser

    ReviewFile -->|"crit push"| GitHubPR[("GitHub PR review")]
    GitHubPR -->|"crit pull<br/>(重複排除して merge)"| ReviewFile
    ReviewFile -->|"crit share"| CritWebSvc[("crit-web<br/>(opt-in)")]
```

- レビューファイルは `~/.crit/reviews/<key>.json`（cwd + branch、またはファイルモードでは cwd + args でキー化）。
- `crit pull` / `crit push` は GitHub PR コメントとの同期を担い、外部由来のコメントは必ずローカル状態に対して重複排除してからマージする（`buildLocalIDSet` + `buildLocalFingerprintIndex` + `dropDuplicateWebComment`）。

## 7. 主要な設計判断（要約）

| # | 判断 | 理由 |
|---|---|---|
| 1 | フロントエンド資産を `embed.FS` に埋め込み | 単一バイナリ配布を実現 |
| 2 | ビルドステップなし（vanilla JS） | npmはvendorライブラリ取得専用 |
| 3 | git / files の2モード | 変更差分レビューと任意ファイルレビューを両立 |
| 4 | markdown-it を採用 | `token.map` でソース行マッピングが取れる |
| 5 | ブロック単位分割 | リスト/コードブロック/表/引用を行単位でコメント可能に |
| 6 | diffハンクのデュアルガター表示 | old/new行番号を両方表示 |
| 7 | コメントはソース行参照でJSON永続化 | `~/.crit/reviews/<key>.json` |
| 8 | コメント変更毎にレビューファイル書き込み | 200msデバウンスでリアルタイム反映 |
| 9 | ファイル監視（git status polling / mtime polling） | SSE経由でブラウザに反映 |
| 10 | デフォルトで localhost バインド | 非loopbackは明示フラグが必須（認証機構が無いため） |
| 11 | 2階層 config（global/project）+ CLIフラグ | agent_cmd等の機微設定はglobal限定でリポジトリ乗っ取りを防止 |
| 16 | フォーカスモード（file/range/stacked） | PRのスタック化レビューに対応 |
| 17 | Go向けLSP統合（`internal/lsp` → gopls） | lazy start + idle shutdown、デーモン毎に独立したマネージャ |

## 8. ディレクトリマップ（再掲）

```
crit/
├── cmd/crit/            # package main — 薄いCLI (main.go, cli_*.go, wire.go)
├── internal/            # コアロジック (daemon, server, session, github, share, vcs, lsp, …)
├── web/                 # 埋め込みフロントエンド資産 (webassets; embed.go)
├── integrations/        # AIコーディングツール向け設定ファイル (claude-code, cursor, aider, …)
├── test/                # テストハーネス、E2E、roundtripドキュメント
├── Makefile
├── package.json
└── copy-deps.js         # npm依存をweb/にコピーして埋め込み
```
