# 調査メモ: `--pr` / `--range` モードでの LSP 定義ジャンプ対応

日付: 2026-08-26

## 1. 現状と原因

`crit --pr <n>` / `crit --range A..B`(`Focus.Kind == FocusRange`)では LSP 機能が無効。

- `internal/server/lsp_handlers.go:75-82` `lspAvailable()` が `FocusRange` のとき明示的に `false` を返す。
- 根本理由: **レビュー画面は `Focus.HeadSHA` の内容を表示しているが、LSP はディスク(現在の作業ツリー)を読む**ため、位置がズレて誤った答えを返す。誤答よりは無効化、という判断。

### 関連コードの読み方

| 箇所 | 役割 |
|---|---|
| `internal/session/session_focus.go:302` `validateFocusSHAs` | `--pr` は checkout せず SHA を fetch するだけ |
| `session_focus.go:446` `readFileAtSHA` | 表示用の内容は `git show HeadSHA:path` で取得 |
| `internal/lsp/manager.go:169` `syncFileLocked` | `os.ReadFile(absPath)` でディスクを読み `didOpen/didChange` |
| `lsp_handlers.go:371` `resolveLocation` → `:416` `readPeek` | gopls が返した行番号を使い、**ディスク**のファイルから peek を切り出す |
| `lsp_handlers.go:148-158` | 絶対パス受け付け(peek からのチェーンジャンプ)。repo root / GOROOT / GOMODCACHE 配下のみ許可 |
| `session.go:469` `RemoteFiles` | `--remote` ではローカル fetch すらしない(内容は `gh api` 経由) |

### 用語: peek

定義ジャンプ先を画面遷移せずポップアップに抜粋表示する機能(VS Code の Peek Definition と同じ)。
`lspLocationResponse` の `Line` / `PeekStart` / `Peek` / `PeekTruncated` / `InSession`。
ファイル 2000 行以下なら全文、超えたら ±100 行(references は 50 行 / ±10 行)。peek 上でさらに識別子をクリックしてチェーンジャンプできる。

## 2. 制約

1. **表示内容 ≠ ディスク内容**(上記)
2. **変更ファイル以外も HeadSHA でなければ意味がない** — ジャンプ先は PR 外ファイルが普通。`go.mod` 差分や新規パッケージも含む
3. **peek もディスク読み** — gopls の返す行番号と peek 本文の基準が一致している前提
4. **`--remote`** — ローカルに object が無いので LSP 対象外
5. **セキュリティ境界** — peek / 絶対パスは repo root・GOROOT・GOMODCACHE 配下のみ。新しい読み取り元を足すなら同じ境界を維持
6. **リソース** — gopls は 1 daemon 1 プロセス、遅延起動、3 分アイドル停止。追加リソースも同じライフサイクルに乗せる

## 3. アプローチ比較

| 案 | 内容 | 評価 |
|---|---|---|
| A. didOpen overlay | 変更ファイルだけ HeadSHA の内容を gopls に送る | 制約 2・3 が解決せず「たまに間違う」状態。既存コードコメントが否定した案 |
| **B. 一時 git worktree** | `HeadSHA` の detached worktree を作り、その root で gopls を起動。peek もそこから読む | 制約 1〜3 を根本解決。**推奨** |
| C. 実際に checkout | `--pr` 時に HeadSHA へ切替 | 作業ツリーを壊す。不可 |

### overlay 方式の不整合(制約 3 の詳細)

overlay = 変更ファイルのみ HeadSHA、他はディスク。gopls 内部の状態が混在する。

- **ジャンプ先が変更ファイル**: gopls は overlay 基準の行番号(例 118)を返すが、`readPeek` はディスク版を読む → 別の関数や存在しない箇所が表示される
- **ジャンプ先が PR 外ファイル**: 行番号と peek は一致するが、ジャンプ元は HeadSHA 基準なので、リネーム等があると解決失敗・別物に飛ぶ
- **references**: overlay 基準とディスク基準の行番号が同じリストに混在し、`InSession` 判定・スクロール導線がズレる
- **チェーンジャンプ**: 1 段目のズレ(ディスク版 peek の座標)が 2 段目の gopls 問い合わせ(overlay 基準)に伝播

`readPeek` を overlay 内容から読むよう変えても、PR 外ファイルとの整合は原理的に直らない。

## 4. 推奨設計: sparse worktree

### 4.1 worktree の作成

```bash
git worktree add --detach --no-checkout <dir> <HeadSHA>
git -C <dir> sparse-checkout set --no-cone '*.go' 'go.mod' 'go.sum' 'go.work' 'go.work.sum'
git -C <dir> checkout
```

- `--no-cone` パターン方式なら `*.go` が全階層にマッチ。`web/`・画像・vendored JS・他言語は checkout されない
- gopls に必要なのは Go ソースと `go.mod/go.sum` のみ。`//go:embed` 対象が無いことによる diagnostics エラーは出るが、定義ジャンプ・hover・references には影響しない(crit は diagnostics を表示していない)
- 場所: `~/.crit/lsp-worktrees/<session-key>/` など
- `vcs` パッケージに `AddDetachedWorktree(sha, dir, patterns)` / `RemoveWorktree(dir)` を追加。git のみ対応、jj/sapling は従来どおり無効

### 4.2 ディスク使用量

- Git オブジェクト: **増えない**(`.git/objects` を共有)
- ワーキングツリー: HeadSHA 時点の sparse 対象ファイルのみ
- gopls キャッシュ: `GOMODCACHE` / `GOCACHE` は共有、追加なし
- untracked 物(node_modules 等): 含まれない
- partial clone(`--filter=blob:none`)なら sparse 対象の blob しか取得されず、自然に恩恵を受ける

### 4.3 圧迫防止

1. **遅延作成**: LSP を実際に使ったときだけ作る(`lspManager()` 生成タイミング)。`--pr` を開くだけでは作らない
2. **短命化**: gopls のアイドル停止(3 分)と同時に worktree も削除。再使用時は作り直す
3. **必ず消す**: daemon 終了・`crit stop` で `git worktree remove --force`
4. **孤児掃除**: daemon 起動時と `crit cleanup` で `~/.crit/lsp-worktrees/` を走査し、対応セッションが生きていないものを削除 + `git worktree prune`
5. **閾値ガード**: checkout 前に `git ls-tree -r -l <HeadSHA>` で `*.go` の合計サイズを見積もり、`lsp_worktree_max_mb`(デフォルト 500MB 程度)超過や空き容量不足なら作らず、`lsp_available: false` + 理由を UI に出す
6. **上限**: 1 daemon 1 worktree。Focus 切替(PR 切替・HeadSHA 更新)時は作り直し

→ 常駐は「ジャンプを使っている数分間の Go ソース分」に収まる。

### 4.4 第 2 段階(必要なら): 対象モジュールだけに絞る

複数 Go モジュールのモノレポ向け。

1. 変更 `.go` ファイルから最寄りの `go.mod` を探してモジュールルート特定
2. `replace ../other` と `go.work` の `use` を推移的に辿ってローカル依存モジュールを追加
3. sparse パターンを `<module>/**/*.go` 形式に絞る

「結局全部」を指す構成では効果がないので、4.1 で足りない場合の追加オプション扱い。

### 4.5 サーバー側の変更点

- `lsp_handlers.go` の `repoRoot := s.session.Load().RepoRoot` を「LSP 用 root」(通常時 RepoRoot、range focus 時 worktree)を返すヘルパーに差し替える。相対→絶対変換、`lspPathAllowed` の許可 root、`resolveLocation` の peek 読み、`InSession` 判定(worktree 絶対パス → repo 相対)すべてがこの root を使う
- `lsp.NewManager(root, ctx)` の `root` を worktree に。Focus 変更時に Manager を `Shutdown` → 破棄 → 再生成(`dropStaleCacheOnPRSwitch` 付近にフック)
- `lspAvailable()` のガードを `FocusRange && RemoteFiles`(および jj/sapling)に絞る
- 初回リクエストは checkout 時間がかかるので既存の `warmupTimeout`(15s)のリトライで吸収
- フロント(`crit-lsp.js`)は repo 相対パスを送っているので変更不要
- `GOMODCACHE` は共有なので、依存追加 PR は初回ジャンプ時に gopls がダウンロード。オフラインなら該当箇所だけ未解決

### 4.6 テスト

- `manager_test.go` の fake start と `lsp_handlers` の `newProvider` 注入を活用
- ユニット: range focus で root が worktree になる / Focus 変更で Manager 再生成 / peek が worktree から読まれる / 閾値超過で無効化
- 実 gopls: `real_gopls_manual_test.go` に 1 ケース追加

## 5. 他言語への拡張性

sparse worktree の仕組み自体は言語非依存(拡張子・設定ファイルのパターンを差し替えるだけ)。
言語ごとに本当に違うのは **依存物の所在**。

| 言語 / サーバー | sparse に含めるもの | 依存の所在 | 難所 |
|---|---|---|---|
| Go (gopls) | `*.go go.mod go.sum go.work` | `GOMODCACHE`(グローバル) | なし。最も楽 |
| TypeScript (typescript-language-server) | `*.ts *.tsx *.js *.jsx *.d.ts tsconfig*.json package.json` | `node_modules/`(untracked・repo 内・巨大) | worktree に無い → 外部パッケージへのジャンプ不可 |
| Python (pyright) | `*.py *.pyi pyproject.toml` | venv の site-packages(repo 外) | venv パスをサーバーに教える必要。環境依存 |
| Rust (rust-analyzer) | `*.rs Cargo.toml Cargo.lock` | `~/.cargo/registry` + `target/`(repo 内・巨大) | `cargo metadata/check` で worktree ごとに `target/` を生成してしまう |
| Java/Kotlin | ソース + `pom.xml` / `build.gradle` | `~/.m2` / `~/.gradle` | サーバーが重い。遅延起動・アイドル停止は有効 |

### 依存物の扱い

1. **グローバルキャッシュ型**(Go, Rust registry, Maven): 何もしない。許可 root にキャッシュ dir を足す
2. **repo 内 untracked 型**(node_modules, target): 元 repo の同名 dir へ symlink。tsserver は realpath 解決するので peek の許可 root に元 repo の `node_modules` を追加(symlink 越しのパス検証が必要)。Rust の `target/` は symlink すると書き込みが混ざるので `rust-analyzer.cargo.targetDir` を別 dir に向けるか `checkOnSave` を切る
3. **環境依存型**(Python venv): 設定キーで指定、`.venv/` / `VIRTUAL_ENV` を自動検出フォールバック

### 言語プロファイル(2 言語目が来たときに導入)

```go
type Profile struct {
    Name            string            // "go", "typescript"
    Extensions      []string          // ".go"
    LanguageID      string            // "go"
    SparsePatterns  []string          // "*.go", "go.mod", ...
    Command         []string          // {"gopls"} — コード側で固定、PATH lookup のみ(config から乗っ取れない方針を維持)
    ExtraRoots      func() []string   // GOROOT, GOMODCACHE / node_modules / site-packages
    PrepareWorktree func(wt, repo string) error // symlink 等(Go は nil)
}
```

- Manager はプロファイルごとに 1 つ(別プロセス)、アイドル停止は個別
- worktree は言語横断で 1 つ、sparse パターンは有効プロファイルの和集合
- 今の Go 決め打ち箇所: `goLanguageID`、`GoEnv()`、`.go` 判定、sparse パターン。3 箇所未満で抽象化しない方針に従い、今は寄せない

## 6. 他サービス・OSS の事例

### (1) 事前インデックス方式

コミットごとに定義・参照グラフを生成し、UI はそれを引くだけ。

| サービス | 仕組み |
|---|---|
| Sourcegraph | SCIP(旧 LSIF)。CI で `scip-go` 等を実行しアップロード(precise)。無いコミットは tree-sitter の検索型にフォールバック |
| GitHub code navigation | tree-sitter + stack graphs。型を見ない名前解決。依存パッケージには飛べない |
| GitLab | CI で LSIF を artifact upload |
| Google Critique / Chromium CodeSearch | Kythe。ビルド統合の完全解析 |
| Meta | Glean |

長所: レビュー時コストゼロ、任意 SHA で正確、ディスク不要。
短所: CI にインデクサが必要(ローカルツールには重い)、依存側インデックスは別途。

### (2) 実体チェックアウト + 本物の言語サーバー

| サービス | 仕組み |
|---|---|
| GitHub Codespaces / Gitpod | PR ごとに VM/コンテナ、prebuild でキャッシュ |
| VS Code GitHub Pull Requests 拡張 | 「Checkout」で PR ブランチをローカルにチェックアウト(作業ツリーを切替) |
| JetBrains | 同様にローカルに取り込んでから IDE 解析 |

長所: 精度最高、依存も解決。短所: VM コスト or 作業ツリーの乗っ取り。

### (3) 仮想 FS + 制限付き言語サービス

| サービス | 仕組み |
|---|---|
| github.dev / vscode.dev | ブラウザ内仮想 FS で TS サービス。`node_modules` 無しで外部依存不可。他言語は `anycode`(tree-sitter)近似 |
| VS Code Remote Repositories | 同上。公式に「full language intelligence 不可」 |

gopls は仮想 FS 非対応なので Go には使えない。

### crit への示唆

- gopls は実ファイルが必要 → (3) 不可。(1) はローカル単体ツールにはセットアップが重い
- **(2) の軽量版 = sparse worktree** が crit の立ち位置に合う。VS Code 拡張が Checkout させるのに対し、作業ツリーを汚さず別 dir で同じことをする
- (1) の発想は **フォールバック** に借りられる: gopls が使えない状況(`--remote`、閾値超え、未インストール)で tree-sitter / `grep "^func Name"` ベースの検索型ジャンプを出す(Sourcegraph の precise / search-based の 2 段構え)。UI で「概算」と分かるようにする
- 依存物はどのサービスも「本物を置く」か「諦める」。Go はグローバルキャッシュのおかげで例外的に楽

## 7. 実装の着手順(案)

1. `lsp_handlers.go` の root ヘルパー化(通常モードで挙動不変のリファクタ)
2. `vcs` に sparse worktree の add/remove
3. range focus 時の worktree 遅延作成 + Manager 再生成 + `lspAvailable` ガード緩和
4. 後始末(アイドル削除・daemon 終了・孤児掃除)+ 閾値ガード + `lsp_worktree_max_mb` 設定キー
5. テスト(ユニット + real gopls 1 ケース)
6. (任意)モジュール絞り込み、検索型フォールバック、言語プロファイル

## 8. 性能テストの仕組み(大規模 Go リポジトリ向け)

実装前に計測基盤だけ先に作り、実装後は同じ基盤で回帰を見る。
既存の `roundtrip_integration_test.go`(build tag `e2e_github`)と同じ「ローカル限定・opt-in」方式にする。

### 8.1 何を測るか

| 指標 | 意味 | 目安の合格ライン(要調整) |
|---|---|---|
| `worktree_add_ms` | `git worktree add --no-checkout` + `sparse-checkout set` + `checkout` の合計 | kubernetes 級で < 10s |
| `worktree_bytes` | worktree の実サイズ(`du -sk`、`.git` ファイルを除く) | フル checkout の Go 比率以下 |
| `estimate_ms` / `estimate_bytes` | `git ls-tree -r -l` による事前見積もりの時間と、実サイズとの誤差 | 誤差 ±20%、時間 < 1s |
| `gopls_first_def_ms` | gopls 起動 → 初回 definition 応答(warm-up 込み) | < `warmupTimeout`(15s) |
| `gopls_warm_def_ms` | 2 回目以降の definition | < 200ms |
| `gopls_rss_mb` | 初回応答後の gopls の RSS | 参考値(フル checkout と比較) |
| `worktree_remove_ms` | `git worktree remove --force` + `prune` | < 2s |
| `baseline_*` | 同じ計測を **フル checkout の作業ツリー** で取ったもの | sparse が baseline より悪化していないこと |

baseline との比較が重要。sparse で `//go:embed` 対象や testdata が欠けることで gopls がエラー処理に時間を使ったり、逆に非 Go ファイルの監視が減って速くなったりする。どちらに転ぶか実測で確認する。

### 8.2 構成

```
test/perf/
├── README.md                 # 使い方・対象 repo の準備手順
├── lsp_worktree_perf_test.go # build tag: perf_lsp。Go テストとして計測し JSON を出力
├── synth/                    # 合成モノレポ生成(8.4)
└── results/                  # .gitignore。JSON 結果の置き場
scripts/perf-lsp-worktree.sh  # 対象 repo の clone → テスト実行 → 結果の比較表を出す
```

Makefile: `make perf-lsp REPO=<path> [SHA=<sha>]`

環境変数:

- `CRIT_PERF_REPO` — 対象リポジトリのローカルパス(必須。無ければ `t.Skip`)
- `CRIT_PERF_SHA` — HeadSHA 相当(省略時は `HEAD`)
- `CRIT_PERF_FILE` / `CRIT_PERF_LINE` / `CRIT_PERF_CHAR` — definition を投げる位置(省略時は repo 内の最初の `.go` ファイルで最初の import された識別子を自動選択)
- `CRIT_PERF_OUT` — 結果 JSON の出力先

### 8.3 テストの流れ(`lsp_worktree_perf_test.go`)

実装前でも動くように、worktree 操作は生の `git` コマンド、gopls は既存の `internal/lsp.Manager` を直接使う。
実装後は worktree 部分を `vcs.AddDetachedWorktree` に差し替えるだけ。

```go
//go:build perf_lsp

func TestPerf_LSPWorktree(t *testing.T) {
    repo := requireEnv(t, "CRIT_PERF_REPO")
    sha  := envOr("CRIT_PERF_SHA", "HEAD")

    // 1. 事前見積もり
    r.estimate = timeIt(func() { r.estimateBytes = lsTreeGoBytes(repo, sha) })

    // 2. sparse worktree 作成
    dir := t.TempDir()
    r.worktreeAdd = timeIt(func() {
        git(repo, "worktree", "add", "--detach", "--no-checkout", dir, sha)
        git(dir, "sparse-checkout", "set", "--no-cone", "*.go", "go.mod", "go.sum", "go.work", "go.work.sum")
        git(dir, "checkout")
    })
    r.worktreeBytes = duBytes(dir)

    // 3. gopls: 初回 / ウォーム
    m := lsp.NewManager(dir, ctx)
    r.firstDef = timeIt(func() { m.Definition(target...) })
    r.warmDef  = median(5, func() { m.Definition(target...) })
    r.goplsRSS = rssOf(m)   // Manager にテスト用の PID 取得口を足す
    m.Shutdown()

    // 4. 後始末
    r.worktreeRemove = timeIt(func() {
        git(repo, "worktree", "remove", "--force", dir)
        git(repo, "worktree", "prune")
    })

    // 5. baseline(作業ツリーそのもので 3 を再実行)
    // 6. JSON 出力 + しきい値チェック(CRIT_PERF_STRICT=1 のときだけ fail)
}
```

しきい値は最初は fail させず記録のみ。数回計測して分布が分かってから `CRIT_PERF_STRICT` で fail に切り替える。

### 8.4 対象リポジトリ

| リポジトリ | 特徴 | 狙い |
|---|---|---|
| `kubernetes/kubernetes` | 単一巨大モジュール、`staging/` に replace 多数、vendor あり | 最大級の Go 単一モジュール。`vendor/` が sparse でどう扱われるか(`vendor/**/*.go` は含まれる → 除外パターン要検討) |
| `golang/go` | GOROOT 自体。`src/` 配下に stdlib | 特殊ケース。gopls が混乱しないか |
| `cockroachdb/cockroach` | Bazel 併用、生成コード多数 | 生成ファイルが無いと解決できない箇所の割合 |
| `grafana/grafana` | Go + 巨大 TS フロント | **モノレポの本命**。sparse による削減率を測る |
| `tailscale/tailscale` | 中規模、`go.mod` 1 つ | 現実的な下限。ここで遅ければ設計が悪い |
| `hashicorp/terraform` | 中〜大規模 | 参考 |
| **合成モノレポ**(`test/perf/synth`) | パラメータで生成 | 8.5 |

clone は `git clone --filter=blob:none`(partial clone)で行う。sparse checkout に必要な blob だけ落ちるので、テスト自体が partial clone との相性確認にもなる。フルの baseline を取るときは別途 `git checkout` で blob を落とす。

### 8.5 合成モノレポ(再現性のある計測)

実リポジトリはネットワークと時期で結果が揺れるので、CI や回帰確認には合成 repo を使う。

```
test/perf/synth/gen.go   # go run ./test/perf/synth -go-files 20000 -junk-mb 2000 -modules 5 -out /tmp/synth
```

パラメータ:

- `-go-files N` — Go ファイル数。パッケージ間に import の連鎖を張り、definition が実際に別パッケージへ飛ぶようにする
- `-junk-mb M` — 非 Go ファイル(画像・JSON・`*.min.js` 相当のランダムバイト)の総量。sparse の削減率を直接コントロールする
- `-modules K` — `go.work` で束ねる Go モジュール数(4.4 のモジュール絞り込みの検証用)
- `-embed` — `//go:embed` を含むパッケージを入れる(sparse で欠けたときの gopls の挙動確認)

生成後に commit して、`CRIT_PERF_REPO=/tmp/synth` で同じテストを回す。
「`junk-mb` を 0 → 2000 に変えても `worktree_bytes` と `worktree_add_ms` がほぼ一定」が sparse の効果の証明になる。

### 8.6 見るべき落とし穴

- **`vendor/`**: `*.go` パターンに引っかかるので kubernetes では数十 MB 増える。`-mod=vendor` 前提のリポジトリでは逆に無いと解決できない。`vendor/modules.txt` の有無で含める/含めないを決める案
- **`testdata/`**: 除外してよいが、`go test` 用の `.go` ファイル(`*_test.go`)は definition 対象になり得るので含めたままにする
- **生成コード(`.pb.go` など)**: tracked なら sparse に含まれる。untracked(ビルド生成)なら欠けるので、その割合を cockroach で見る
- **sparse-checkout の非 cone モード自体の速度**: 非 cone は cone より遅い(パターンマッチが全パスに走る)。kubernetes 級で許容範囲か確認し、遅ければ cone モード + ディレクトリ列挙に切り替える
- **gopls のファイル監視**: 非 Go ファイルが無いぶん `didChangeWatchedFiles` の負荷が減るはずだが、逆に `go.mod` の `replace` 先が sparse で欠けているとエラーが出る。ログ(`gopls -rpc.trace`)を結果に添付する

### 8.7 実装前に作れる部分

worktree 機能の実装に依存しないので、**先にこれだけ作って実リポジトリで数字を取り、設計判断(非 cone で足りるか、vendor をどうするか、閾値のデフォルト値)を確定してから本実装に入る**のが安全。

1. `test/perf/lsp_worktree_perf_test.go`(生 git + 既存 `lsp.Manager`)
2. `scripts/perf-lsp-worktree.sh`(clone → 実行 → 表)
3. `test/perf/synth/gen.go`
4. `lsp.Manager` にテスト用の PID 取得(`RSS` 計測用)を追加 — 唯一のプロダクションコード変更、数行

これで得た数字を 4.3 の `lsp_worktree_max_mb` デフォルトと 4.5 の `warmupTimeout` の妥当性判断に使う。

### 8.8 実装状況と初回計測(2026-08-26)

基盤は実装済み: `test/perf/lsp_worktree_perf_test.go`(tag `perf_lsp`)、`test/perf/synth`、
`scripts/perf-lsp-worktree.sh`、`make perf-lsp REPO=...`。`lsp.Manager.ProcessID()` を追加(RSS 計測用)。

| repo | full tree | sparse wt | add | estimate | gopls first (sparse / baseline) | RSS (sparse / baseline) |
|---|---|---|---|---|---|---|
| 合成(3000 go, 300MB junk, 3 modules, embed) | 315.7 MB / 3308 files | 1.1 MB / 3006 files | 361ms | 101ms, 誤差 0% | 682ms / 712ms | 150MB / 248MB |
| crit 自身 | 67.0 MB / 1523 files | 2.9 MB / 354 files | 73ms | 66ms, 誤差 0% | 295ms / 297ms | 201MB / 276MB |

所見:
- sparse による削減は期待どおり(ジャンクを丸ごと除外)。`ls-tree` 見積もりは実サイズと一致し、閾値ガードの根拠として十分
- gopls の初回応答は sparse と baseline で差がなく、RSS はむしろ sparse のほうが小さい(非 Go ファイルの監視が減るため)
- `//go:embed` 対象が欠けても definition は成功(retries 0)
- 次: kubernetes / grafana 級で `worktree_add_ms` と非 cone モードの速度を確認する(`./scripts/perf-lsp-worktree.sh https://github.com/...`)

### 8.9 kubernetes での計測(2026-08-26, fbb9a10, partial clone)

| 指標 | 値 |
|---|---|
| full tree | 277.6 MB / 31,295 files |
| sparse worktree | **187.1 MB / 17,963 files**(内訳: vendor 48.3 / staging 65.1 / その他 73.0 MB) |
| worktree add | 1.6〜1.7 s(非 cone sparse でも問題なし) |
| worktree remove | 1.4 s |
| 見積もり | 187.1 MB を 69 ms で算出、誤差 0% |
| gopls 初回 definition(sparse, cold cache) | 13.3 s(`warmupTimeout` 15 s の直前) |
| gopls 初回 definition(baseline, cache warm) | 9.2 s |
| gopls 初回 definition(sparse, cache warm) | **6.9 s** |
| gopls RSS | sparse 0.95〜1.1 GB / baseline 1.2 GB |
| 定義ジャンプ | 成功、worktree 内 `cmd/kube-controller-manager/names/controller_names.go` に解決 |

所見:
- **ディスク**: Go ファイルだけでも 187 MB。純 Go リポジトリでは sparse の削減率は 3 割程度にとどまる(削減はドキュメント・YAML・テストデータ分)。`vendor/` が 48 MB を占めるので、`vendor/modules.txt` がある場合に含める/除外するかは要判断(gopls は `-mod=vendor` を go.mod の go version 1.14+ かつ vendor ディレクトリありで自動的に使うため、除外すると解決に失敗する可能性 → 含めるのが安全)
- **時間**: checkout 1.7 s は許容範囲。問題は **gopls の warm-up 7〜13 s** で、これは sparse とは無関係(baseline も 9 s)。冷えた状態では `warmupTimeout`(15 s)ぎりぎりなので、本実装ではタイムアウトを延ばすか、UI で「初回は時間がかかる」表示を出す必要がある
- **sparse による劣化なし**: cache warm 同士の比較で sparse 6.9 s < baseline 9.2 s。RSS もわずかに小さい
- **見積もり**: `ls-tree` は 31k エントリでも 69 ms、誤差 0%。閾値ガードはこれで十分
- 順序効果に注意: 同一プロセスで sparse → baseline の順に走らせると後者が GOCACHE/module cache の恩恵を受ける。比較は cache warm 同士で行う(`CRIT_PERF_BASELINE=0` で再実行)
