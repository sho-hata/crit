---
title: "PRレビュー画面で定義ジャンプを動かす：作業ツリーを汚さずにgoplsを使うには"
emoji: "🌲"
type: "tech"
topics: ["go", "gopls", "lsp", "git", "codereview"]
published: false
---

ソフトウェアエンジニアの [hata](https://x.com/sho_hata_) です。

[crit](https://github.com/tomasz-tomczyk/crit) という、ブラウザ上でコードレビューができるCLIツールがあります。私はこれをフォークして、Goのコードをレビューするときに gopls の機能（hover・定義ジャンプ・参照検索）が使えるようにする改造を加えています。

今回、`crit --pr <n>` でGitHubのPull Requestをレビューするときにも定義ジャンプが使えるようにしました。その過程で「レビュー画面が表示している内容」と「language serverが読むファイル」がズレるという問題にぶつかり、最終的に **sparse checkoutした一時的なgit worktree** をlanguage serverのワークスペースにする方法を採りました。

本記事では、そこに至るまでの制約の整理、他サービスがこの問題をどう解いているかの調査、そして採用した設計について書きます。

## 背景：PRレビューでこそ定義ジャンプが欲しい

critは、ローカルのgit差分をGitHub PR風のUIでブラウザに表示し、行にインラインコメントを付けられるツールです。単一バイナリで動き、コメントはJSONに書き出されてAIエージェントへのフィードバックになります。

ここに `internal/lsp` パッケージを足して、goplsとstdioで喋らせています。サーバー側は3つのエンドポイントを生やしているだけです。

```
GET /api/lsp/hover?path=X&line=N&char=M
GET /api/lsp/definition?path=X&line=N&char=M
GET /api/lsp/references?path=X&line=N&char=M
```

定義ジャンプには **peek** も実装しています。VSCodeのPeek Definitionと同じで、画面遷移せずにポップアップで定義先を抜粋表示するものです。peekの中でさらに識別子をクリックしてチェーンジャンプもできます。

これは通常モード（作業ツリーの変更をレビューする）では問題なく動きます。goplsはディスクを読み、レビュー画面もディスクの内容を表示しているので、両者が一致しているからです。

ところが `crit --pr <n>` や `crit --range A..B` では、LSP機能を丸ごと無効化していました。

```go
func (s *Server) lspAvailable() bool {
	...
	if sess.Focus.Kind == FocusRange {
		return false
	}
	...
}
```

理由は単純で、**レビュー画面は特定のコミット（`Focus.HeadSHA`）の内容を表示しているのに、goplsは現在の作業ツリーを読む**からです。行番号がズレて、まったく違う関数に飛びます。誤答を返すくらいなら機能ごと無効にする、という判断でした。

とはいえ、PRレビューこそ定義ジャンプが欲しい場面です。「この関数の呼び出し元はどこか」「この型の定義はどうなっているか」を知りたいのは、むしろ他人が書いたコードを読むときです。無効化したままにはしておきたくありませんでした。

## 制約の整理

まず、何がこの問題を難しくしているのかを洗い出しました。

### 1. 表示内容 ≠ ディスクの内容

`crit --pr` は作業ツリーを切り替えません。SHAをfetchするだけで、表示用の内容は `git show <HeadSHA>:<path>` で取得しています。作業ツリーは開発者が作業中の状態のままです。

これは意図的な設計です。レビューのために作業中の変更をstashさせられるツールは、あまり使いたくありません。

### 2. ジャンプ先はPRに含まれないファイルが普通

定義ジャンプの行き先は、たいていPRの差分に入っていないファイルです。「差分のあるファイルだけ何とかする」では足りません。

### 3. peekもディスクを読んでいる

peekは、goplsが返した行番号を使ってファイルから該当箇所を切り出します。つまり **goplsが見ているファイルと、peekが読むファイルは同一でなければならない** という制約があります。

### 4. リソースを増やしたくない

goplsは1デーモンにつき1プロセス、遅延起動（最初のLSPリクエストで初めてspawn）、3分アイドルで停止、という設計にしています。複数のworktreeで並行してレビューしていても、実際にホバーしているセッションの分しかgoplsが常駐しません。追加するリソースも同じライフサイクルに乗せる必要があります。

### 5. セキュリティ境界を広げない

peekの読み出しと、チェーンジャンプ用の絶対パス受け付けは、repo root / GOROOT / GOMODCACHE 配下のみに制限しています。汎用のファイル読み出しAPIにしてはいけません。読み取り元を増やすなら、同じ境界を維持する必要があります。

## 却下した案

### didOpen overlayで差分ファイルだけ差し替える

LSPには、ディスクの内容ではなくクライアントが持っている内容をサーバーに教える仕組みがあります（`textDocument/didOpen` の `text`）。差分のあるファイルだけHeadSHAの内容を送りつければいいのでは、と最初に考えました。

しかしこれは制約2・3が解決しません。gopls内部の状態が「差分ファイルはHeadSHA基準、それ以外はディスク基準」と混在してしまいます。

- **ジャンプ先が差分ファイルの場合**：goplsはoverlay基準の行番号を返すが、peekはディスク版を読む。別の関数が表示される
- **ジャンプ先がPR外ファイルの場合**：行番号とpeekは一致するが、ジャンプ元がHeadSHA基準なので、リネームがあると解決に失敗する
- **references**：overlay基準とディスク基準の行番号が同じリストに混ざる
- **チェーンジャンプ**：1段目のズレ（ディスク版peekの座標）が、2段目のgopls問い合わせに伝播する

peekの読み出し元をoverlayに変えても、PR外ファイルとの整合は原理的に直りません。「たいてい合っているが、たまに間違う」は、レビューツールとしては無効化より悪いと考えました。

### 実際にcheckoutする

作業ツリーを壊すので論外です。

## 他のサービスはどう解決しているのか

自分だけが悩んでいる問題ではないはずなので、既存サービスがどうしているかを調べました。大きく3つに分かれます。

### (1) 事前インデックス方式

コミットごとに定義・参照のグラフを生成しておき、UIはそれを引くだけ。レビュー時にはlanguage serverを動かしません。

| サービス | 仕組み |
|---|---|
| Sourcegraph | SCIP（旧LSIF）。CIで `scip-go` 等を実行してアップロード（precise）。インデックスの無いコミットではtree-sitterベースの検索型にフォールバック |
| GitHub code navigation | tree-sitter + stack graphs。型を見ない名前解決。依存パッケージには飛べない |
| GitLab | CIでLSIFをartifactとしてupload |
| Google Critique / Chromium CodeSearch | Kythe。ビルド統合による完全解析 |
| Meta | Glean |

レビュー時のコストがゼロで、任意のSHAに対して正確、ローカルにチェックアウトも要りません。ただしCIにインデクサを組み込む前提があります。ローカルで完結する単体ツールには重すぎます。

なお、Sourcegraphがprecise（正確なインデックス）とsearch-based（検索ベースの近似）の2段構えになっているのは示唆的でした。これは後述します。

### (2) 実体をcheckoutして本物のlanguage serverを動かす

| サービス | 仕組み |
|---|---|
| GitHub Codespaces / Gitpod | PRごとにVM・コンテナを立て、prebuildでキャッシュ |
| VS Code GitHub Pull Requests拡張 | 「Checkout」ボタンでPRブランチをローカルにチェックアウトする |
| JetBrains | 同様にローカルに取り込んでからIDEの解析にかける |

精度は最高で、依存も解決できます。代償はVMのコストか、作業ツリーの乗っ取りです。

### (3) 仮想FS + 制限付きの言語サービス

| サービス | 仕組み |
|---|---|
| github.dev / vscode.dev | ブラウザ内の仮想FS上でTypeScriptサービスを動かす。`node_modules` が無いので外部依存には飛べない。他言語は `anycode`（tree-sitter）による近似 |
| VS Code Remote Repositories | 同上。ドキュメントに「full language intelligenceは提供できない」と明記されている |

TypeScriptサービスは仮想FSを前提に作られているので成立しますが、goplsは実ファイルを要求するのでこの手は使えません。

### critへの示唆

- goplsが実ファイルを要求する以上、(3)は不可
- (1)はローカル単体ツールにはセットアップが重すぎる
- **(2)の軽量版**が居場所になりそう。VS Code拡張がユーザーに「Checkout」させるところを、作業ツリーを汚さず別ディレクトリで同じことをする

これがsparse worktree案です。

## 採用：sparse checkoutした一時worktree

### やっていること

`Focus.HeadSHA` をdetached worktreeとしてチェックアウトし、そこをgoplsのワークスペースrootにします。

```bash
git worktree add --detach --no-checkout <dir> <HeadSHA>
git -C <dir> sparse-checkout set --no-cone '*.go' 'go.mod' 'go.sum' 'go.work' 'go.work.sum'
git -C <dir> checkout
```

ポイントは3つあります。

**作業ツリーに触らない。** `git worktree` は別ディレクトリに独立したチェックアウトを作る仕組みなので、元の作業ツリーはそのままです。開発者がstashを強いられることはありません。

**sparse checkoutでGoソースだけ落とす。** goplsに必要なのはGoソースと `go.mod`/`go.sum` だけです。`web/` 配下の画像やvendorされたJSは要りません。`--no-cone`（パターン方式）にすると `*.go` が全階層にマッチします。cone modeはディレクトリ単位でしか指定できないので、拡張子で絞るには非coneが必要でした。

**オブジェクトストアは共有される。** worktreeは元のリポジトリと `.git/objects` を共有するので、ディスクに増えるのはチェックアウトされたファイルの実体だけです。cloneし直すのとはコストが全然違います。

`//go:embed` の対象ファイルが欠けることによるdiagnosticsエラーは出ますが、critはdiagnosticsを表示していないので実害はありません。定義ジャンプ・hover・referencesは問題なく動きます。

### 「LSPのroot」を1箇所に寄せる

実装で一番効いたのは、機能追加の前に入れたリファクタでした。

LSPまわりのコードには「repo root」の用途が4つありました。

1. language serverのワークスペースroot
2. リクエストの相対パスを絶対パスに直すときのベース
3. peekの読み出しを許可するroot（セキュリティ境界）
4. goplsが返した絶対パスをrepo相対に戻すときのベース

これが全部それぞれ `sess.RepoRoot` を読んでいました。ここにworktreeを差し込むと、1つ直し忘れただけで「goplsはworktreeを見ているのにpeekは作業ツリーを読む」という、まさに避けたかった状態になります。

なので先に `lspRoot()` というヘルパーを作り、4箇所すべてをそこに通しました。この時点では挙動は変わりません（常に `sess.RepoRoot` を返す）。

```go
// lspRoot returns the directory LSP features operate on: the workspace the
// language server is anchored at, the base for repo-relative request paths,
// the root peek reads are authorized against, and the base used to map
// server result paths back to repo-relative paths for the frontend. Every
// LSP code path must go through this rather than sess.RepoRoot so that the
// workspace can be pointed somewhere other than the working tree (a checkout
// of Focus.HeadSHA for range/PR focus) without the pieces drifting apart.
func (s *Server) lspRoot() string {
```

そのうえで、`lspRoot()` がrange focusのときだけworktreeを返すように変えます。機能追加そのものの差分は小さくなり、レビューもしやすくなりました。

### ライフサイクル

worktreeを作るのは簡単ですが、消し忘れるとディスクを圧迫します。goplsのライフサイクルにそのまま乗せました。

- **遅延作成**：`--pr` を開いただけでは作らない。最初のLSPリクエスト（＝実際にホバーした瞬間）に作る。PRを何度か切り替えてから初めてホバーする、というのはよくある操作です
- **アイドル削除**：goplsのアイドル停止（3分）と同じタイマーでworktreeも消す。放置されたレビューがチェックアウトを抱え続けないようにする
- **SHA変更で作り直し**：focusが別のコミットに切り替わったら、次のLSPリクエストで作り直す
- **デーモン終了で削除**：`ShutdownLSP()` から必ず消す
- **孤児の回収**：デーモンがクラッシュして消し損ねた場合に備え、作る前に同じパスの残骸があれば `git worktree remove` してからやり直す

worktreeの置き場所はレビューデータと同じフォルダ配下（`<review folder>/lsp-worktree`）にしています。セッションキーで一意になるので、並行レビューでも衝突しません。

### サイズガード

チェックアウトする前に、`git ls-tree -r -l <sha>` でsparse対象のblobサイズを合計して見積もります。`lsp_worktree_max_mb`（デフォルト500MB）を超えたらworktreeを作らず、理由を返して機能を無効にします。

```go
if limitMB := s.cfg.LSPWorktreeSizeLimitMB(); limitMB > 0 {
	size, err := vcs.SparseTreeSize(s.shutdownCtx, sess.RepoRoot, sess.Focus.HeadSHA, goLspSparsePatterns)
	if err != nil {
		return fmt.Errorf("estimating lsp worktree size: %w", err)
	}
	if limitBytes := int64(limitMB) * 1024 * 1024; size > limitBytes {
		return fmt.Errorf("lsp worktree would be %dMB, over the %dMB lsp_worktree_max_mb limit", size/(1024*1024), limitMB)
	}
}
```

この見積もりが実用になるかは実測しました（後述）。

### 対応範囲

sparse worktreeが成立するのはローカルのgitリポジトリだけなので、次の場合は従来どおり無効のままです。

- `--remote`：ローカルにobjectが無い（内容は `gh api` 経由で取得している）
- Sapling / Jujutsu：`git worktree` がない

## 計測してから実装した

設計判断（非coneモードで速度が足りるか、閾値のデフォルトをいくつにするか）を勘で決めたくなかったので、**実装の前に計測基盤だけ先に作りました**。既存のGitHub roundtripテストと同じく、build tagで隔離したローカル限定のopt-inテストです。

`make perf-lsp REPO=<path>` で、対象リポジトリに対して以下を測り、フルチェックアウトのbaselineと並べてJSONに出します。

- worktreeの作成・削除時間
- worktreeの実サイズ
- `ls-tree` による事前見積もりの時間と誤差
- goplsの初回definition応答（warm-up込み）とウォーム時の応答
- goplsのRSS

### kubernetes/kubernetes での結果

| 指標 | 値 |
|---|---|
| フルツリー | 277.6 MB / 31,295 files |
| sparse worktree | 187.1 MB / 17,963 files |
| worktree作成 | 1.6〜1.7 s |
| worktree削除 | 1.4 s |
| サイズ見積もり | 187.1 MB を 69 ms、誤差 0% |
| gopls初回definition（sparse, cold cache） | 13.3 s |
| gopls初回definition（baseline, cache warm） | 9.2 s |
| gopls初回definition（sparse, cache warm） | 6.9 s |
| gopls RSS | sparse 0.95〜1.1 GB / baseline 1.2 GB |

分かったことは4つです。

**純Goのリポジトリでは、sparseの削減率はそこまで高くない。** kubernetesはGoファイルだけで187MBあり、削減できたのは3割程度（ドキュメント・YAML・テストデータ分）でした。一方、Goとフロントエンドが同居するリポジトリでは効きます。合成リポジトリで検証したところ、非Goファイルを300MB積んでもworktreeは1.1MBのままでした。

**非coneモードのsparse checkoutは、31kエントリでも十分速い。** 1.7秒。非coneはcone modeよりパターンマッチのコストが高いので身構えていましたが、許容範囲でした。ここでcone modeへの切り替えが必要だと分かっていたら、設計をやり直すところでした。

**`ls-tree` による見積もりは誤差0%で69ms。** 閾値ガードの根拠としては十分です。そしてkubernetesの187MBがデフォルト500MBを通ることも確認できました。

**sparseによる劣化は無く、むしろ速い。** cache warm同士の比較で sparse 6.9s < baseline 9.2s、RSSもsparseのほうが小さくなりました。監視対象の非Goファイルが減るぶん、goplsが軽くなっているようです。

そして、**本当のボトルネックはsparseではなくgoplsのwarm-upだった**というのが一番の収穫でした。冷えた状態で13.3秒かかっており、現在のリトライ上限（15秒）ぎりぎりです。baselineでも9.2秒なので、sparseのせいではありません。ここはタイムアウトを延ばすか、UIで「初回は時間がかかる」と出すかの判断が残っています。

計測を先にやったおかげで、実装に入る前に「非coneで行ける」「閾値500MBは妥当」「遅さの原因はworktreeではない」の3つが確定しました。

## 他言語への拡張性

sparse worktreeの仕組み自体は言語非依存で、パターンを差し替えるだけです。言語ごとに本当に違うのは **依存物の所在** でした。

| 言語 / サーバー | sparseに含めるもの | 依存の所在 | 難所 |
|---|---|---|---|
| Go (gopls) | `*.go go.mod go.sum go.work` | `GOMODCACHE`（グローバル） | なし。最も楽 |
| TypeScript | `*.ts *.tsx tsconfig*.json package.json` | `node_modules/`（untracked・repo内・巨大） | worktreeに無いので外部パッケージへ飛べない |
| Python (pyright) | `*.py *.pyi pyproject.toml` | venvのsite-packages（repo外） | venvのパスを教える必要があり、環境依存 |
| Rust (rust-analyzer) | `*.rs Cargo.toml Cargo.lock` | `~/.cargo/registry` + `target/` | `cargo check` がworktreeごとに `target/` を作ってしまう |
| Java / Kotlin | ソース + `pom.xml` / `build.gradle` | `~/.m2` / `~/.gradle` | サーバーが重い |

依存物の扱いは3パターンに分かれます。

1. **グローバルキャッシュ型**（Go, Rustのregistry, Maven）：何もしなくてよい。peekの許可rootにキャッシュディレクトリを足すだけ
2. **repo内untracked型**（node_modules, target）：元リポジトリの同名ディレクトリへsymlinkする。ただしtsserverはrealpathを解決するので、peekのパス検証をsymlink越しに通す必要がある。Rustの `target/` はsymlinkすると書き込みが混ざるので別の手が要る
3. **環境依存型**（Python venv）：設定キーで指定し、`.venv/` や `VIRTUAL_ENV` の自動検出をフォールバックにする

Goが例外的に楽なのは、依存が全部グローバルキャッシュにあるからです。worktreeを作るだけで依存解決までついてきます。TypeScriptで同じことをやろうとすると、`node_modules` の扱いで一段難しくなります。

言語プロファイルとして抽象化する構想はありますが、Go決め打ちの箇所が3箇所程度なので、2言語目が来るまでは寄せない方針にしました。

## おわりに

「レビュー画面が表示している内容と、language serverが読むファイルを一致させる」という一点に絞ると、選択肢は「インデックスを事前に作る」「実体をどこかにcheckoutする」「仮想FSで諦める」の3つしかない、というのが調べてみての結論でした。

ローカルで完結する単体CLIツールという立ち位置だと、2つ目を作業ツリーを汚さない形でやる、つまりsparse checkoutした一時worktreeが素直な答えになりました。git worktreeがオブジェクトストアを共有してくれるおかげで、思っていたよりずっと安く実現できます。

残タスクとして、事前インデックス方式の発想をフォールバックに借りることを考えています。goplsが使えない状況（`--remote`、閾値超過、goplsが未インストール）で、tree-sitterや `grep "^func Name"` ベースの検索型ジャンプを出す。Sourcegraphのprecise / search-basedの2段構えと同じ発想で、UI上で「これは概算です」と分かるようにする形です。

同じような「特定コミットの内容に対してlanguage serverを動かしたい」場面に遭遇した方の参考になれば幸いです。
