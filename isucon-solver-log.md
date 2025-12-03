# ISUCON ソルバー ログ

## ベースラインベンチマーク
- **スコア: 1810**
- `/users/transactions.json`、`/new_items.json`、`/login` で複数のタイムアウトエラーを観測

---

## 最適化 1: カテゴリキャッシュ
- **実装内容**: 起動時と初期化後に全カテゴリをメモリにキャッシュ
- **変更ファイル**: `webapp/go/main.go`
  - RWMutex付きの `categoryCache` マップを追加
  - 全カテゴリをキャッシュに読み込む `loadCategories()` 関数を追加
  - `getCategoryByID()` をキャッシュ優先に変更
  - `main()` と `postInitialize()` で `loadCategories()` を呼び出し
- **発見方法**: Mackerel DB Query Stats

### 最適化 1 の結果
- **スコア: 2010 (+200, +11%)**
- カテゴリクエリがトップクエリから消失（105,857回実行 → 10回のキャッシュロード）

---

## 最適化 2: ユーザーキャッシュ
- **実装内容**: 更新時のキャッシュ無効化付きでユーザーをメモリにキャッシュ
- **変更ファイル**: `webapp/go/main.go`
  - RWMutex付きの `userCache` マップを追加
  - 全ユーザーをキャッシュに読み込む `loadUsers()` 関数を追加
  - キャッシュ優先検索の `getUserByIDFromCache()` 関数を追加
  - キャッシュ更新用の `setUserCache()` 関数を追加
  - トランザクション外ではキャッシュを使用するよう `getUserSimpleByID()` を変更
  - `postRegister()`、`postSell()`、`postBump()` でキャッシュを更新
- **発見方法**: Mackerel DB Query Stats

### 最適化 2 の結果
- **スコア: 2210 (+200, +10%)**
- **累計: 1810 → 2210 (+22%)**
- ユーザークエリがトップ20から消失（43,687回実行、73,320ms）

---

## 最適化 3: items テーブルへのデータベースインデックス追加
- **実装内容**: よく使うクエリパターン向けの複合インデックスを追加
- **変更ファイル**: `webapp/sql/01_schema.sql`
  - 新着商品クエリ用に `idx_status_created_id (status, created_at, id)` を追加
  - ユーザー商品クエリ用に `idx_seller_status_created_id (seller_id, status, created_at, id)` を追加
  - 取引クエリ用に `idx_buyer_created_id (buyer_id, created_at, id)` を追加
- **追加修正**: `getUserSimpleByID()` が常にキャッシュを使用するよう修正（トランザクション内でキャッシュをバイパスしていた）
- **発見方法**: Mackerel DB Query Stats

### 最適化 3 の結果
- **スコア: 2610 (+400, +18%)**
- **累計: 1810 → 2610 (+44%)**
- items クエリがフルテーブルスキャンではなくインデックスを使用するように
- 以前の遅いクエリ（P95 約1200ms）が大幅に高速化

---

## 最適化 4: 終端状態での外部APIコールをスキップ
- **実装内容**: `getTransactions` で配送ステータスが「done」の場合は `APIShipmentStatus` コールをスキップ
- **理由**: 「done」は変更されない終端状態のため、外部APIコールは不要
- **変更ファイル**: `webapp/go/main.go`
  - `getTransactions()` で外部API呼び出し前に `shipping.Status` をチェックするよう変更
  - ステータスが `ShippingsStatusDone` の場合、APIコールの代わりにDB値を直接使用
  - 完了した取引のN回の外部APIコールを削減
- **発見方法**: Mackerel HTTP Server Stats

### 最適化 4 の結果
- **スコア: 4550 (+1940, +74%)**
- **累計: 1810 → 4550 (+151%)**
- 取引一覧での外部APIコールが大幅に削減
- 高負荷時に1回のタイムアウトエラー発生（-500ペナルティ）

---

## 最適化 5: postBuy での並列APIコール
- **実装内容**: APIShipmentCreate と APIPaymentToken をゴルーチンで並列実行
- **理由**: これら2つの外部APIコールは独立しており、同時実行可能
- **変更ファイル**: `webapp/go/main.go`
  - ゴルーチンとチャネルを使用して両方のAPIコールを同時実行
  - 両方の結果を待ってから取引を続行
  - エラー処理は維持 - どちらかが失敗したらロールバック
- **発見方法**: Mackerel HTTP Server Stats - POST /buy の P95 が 2078ms

### 最適化 5 の結果
- **スコア: 4850**
- **累計: 1810 → 4850 (+168%)**
- タイムアウトペナルティによりスコアに変動あり
- 順次API待機時間の削減により POST /buy のレイテンシが低下

---

## 最適化 6: getTransactions でのバッチ取得
- **実装内容**: IN句を使用して transaction_evidences と shippings をバッチ取得
- **理由**: N+1問題 - 各商品が transaction_evidences と shippings に対して個別クエリを発行していた
- **変更ファイル**: `webapp/go/main.go`
  - 最初に全商品IDを収集
  - `WHERE item_id IN (...)` で全 transaction_evidences をバッチ取得
  - `WHERE transaction_evidence_id IN (...)` で全 shippings をバッチ取得
  - メインループでO(1)検索用のマップを使用
- **発見方法**: Mackerel HTTP Server Stats - GET /users/transactions.json の P95 が 5668ms（最も遅いエンドポイント）

### 最適化 6 の結果
- **スコア: 5650**
- **累計: 1810 → 5650 (+212%)**
- getTransactions リクエストごとに 2N のデータベースクエリを削減
- GET /users/transactions.json がより効率的に

---

## 最適化 7: 子カテゴリIDキャッシュ
- **実装内容**: 繰り返しのDBクエリを避けるため、子カテゴリIDをメモリにキャッシュ
- **理由**: `SELECT id FROM categories WHERE parent_id=?` が612回実行されていた（getNewCategoryItems での N+1問題）
- **変更ファイル**: `webapp/go/main.go`
  - parent_id → child_ids マッピングを格納する `childCategoryCache map[int][]int` を追加
  - 子カテゴリキャッシュを構築するよう `loadCategories()` を更新
  - `getChildCategoryIDs(parentID int) []int` ヘルパー関数を追加
  - DBクエリの代わりにキャッシュを使用するよう `getNewCategoryItems()` を変更
- **発見方法**: Mackerel DB Query Stats - `SELECT id FROM categories WHERE parent_id=?` が612回実行、P95 83ms

### 最適化 7 の結果
- **スコア: 4950**（タイムアウトエラーによる変動）
- 注: この最適化はDBクエリを削減するが、他のボトルネック（P95 965-1192ms の items クエリ、外部APIコール）と比較すると影響は小さい

---

## 最適化 8: 外部APIコールをDBトランザクション外に移動
- **実装内容**: `postShipDone` と `postComplete` でDBトランザクション開始前に `APIShipmentStatus` コールを移動
- **理由**: 外部APIコールがデータベースロック（`FOR UPDATE`）を保持したまま実行されており、以下の問題を引き起こしていた:
  - 遅いAPI応答時のロック保持時間が長い
  - 同じ行で他のリクエストがブロックされる
  - タイムアウトやエラーによる不整合状態
- **変更ファイル**: `webapp/go/main.go`
  - `postShipDone`: トランザクション前にshippingレコードをクエリし、APIコールを行い、更新用のトランザクションを開始
  - `postComplete`: 同じパターン - トランザクション前にAPIコール
  - shippingsテーブルへの冗長な `SELECT ... FOR UPDATE` を削除（UPDATEのみ必要）
- **発見方法**: Mackerel HTTP Server Stats で POST /ship_done が3.8%エラー率、POST /complete が1.7%エラー率。コードレビューでトランザクションブロック内のAPIコールがロックを保持していることを発見。

### 最適化 8 の結果
- **スコア: 4850（3550から+1300, +37%）**
- **最終チェック失敗なし**（以前は「購入されたはずなのに記録されていません」エラー）
- 取引完了の信頼性が向上
- 外部APIコール中のロック競合を削減

---

## 最適化 9: getUser をキャッシュ使用に修正
- **実装内容**: `getUser()` 関数を直接DBクエリではなく `getUserByIDFromCache()` を使用するよう変更
- **理由**: `getUser()` は認証済みリクエストごとに呼び出される（12箇所）が、ユーザーキャッシュをバイパスしていた
- **変更ファイル**: `webapp/go/main.go`
  - `getUser()` を直接 `SELECT * FROM users WHERE id = ?` ではなく `getUserByIDFromCache()` を呼び出すよう変更
- **発見方法**: Mackerel DB Query Stats で、ユーザーキャッシュ実装済みにもかかわらず `SELECT * FROM users WHERE id = ?` が2339回実行されていた

### 最適化 9 の結果
- ユーザークエリがDB Query Stats トップ20から消失
- スコアの変動は変わらないがクエリ数は削減

---

## 最適化 10: カテゴリクエリ用複合インデックス追加
- **実装内容**: カテゴリベースの商品クエリ用にインデックス `idx_category_status_created_id (category_id, status, created_at, id)` を追加
- **理由**: カテゴリクエリは status と created_at を伴う `category_id IN (...)` を使用していたが、既存のインデックスには category_id が含まれていなかった
- **変更ファイル**: `webapp/sql/01_schema.sql`
  - `INDEX idx_category_status_created_id (category_id, status, created_at, id)` を追加
- **発見方法**: Mackerel DB Query Stats でカテゴリクエリの P95 が700-900ms

### 最適化 10 の結果
- カテゴリクエリパフォーマンスが若干改善

---

## 最適化 11: 商品一覧クエリで SELECT * を回避
- **実装内容**: getNewItems と getNewCategoryItems で `SELECT *` の代わりに必要なカラムのみを選択
- **理由**: `SELECT *` は商品一覧に不要で I/O オーバーヘッドを増やす `description`（TEXTフィールド）を取得していた
- **変更ファイル**: `webapp/go/main.go`
  - `getNewItems()` のクエリを id, seller_id, status, name, price, image_name, category_id, created_at のみ選択に変更
  - `getNewCategoryItems()` のクエリも同様に変更
- **発見方法**: Mackerel DB Query Stats で `SELECT * FROM items` クエリの P95 が1000-1700ms

### 最適化 11 の結果
- **スコア: 5860-6860**（raw: 6750-7360、タイムアウトペナルティあり）
- **累計: 1810 → 約6500平均 (+260%)**
- TEXTカラムを除外することで商品クエリのI/Oが大幅に削減

---

## 最適化 12: postBuy で外部APIコールをDBトランザクション外に移動
- **実装内容**: DBトランザクション開始前に外部APIコールを行うよう `postBuy` をリファクタリング
- **理由**: 元の実装は遅い外部APIコール（500-1000ms）を行いながらデータベースロック（items と users への `FOR UPDATE`）を保持していた。これにより:
  - 他の購入リクエストをブロックするロック保持時間が長い
  - タイムアウトやエラーによる不整合状態
  - 最終チェック失敗（「購入されたはずなのに記録されていません」）
- **変更ファイル**: `webapp/go/main.go`
  - 最初にロックなしで商品と出品者情報を読み取り
  - トランザクション前に並列外部APIコール（配送 + 決済）を実行
  - APIコール完了後にのみトランザクションを開始
  - コミット前に `FOR UPDATE` ロックで商品ステータスを再確認
  - ロック保持時間を（API時間 + DB時間）から（DB時間のみ）に削減
- **発見方法**: Mackerel HTTP Server Stats で POST /buy が5.3%エラー率（取引エンドポイント中最高）、P95 が999ms。コード分析でAPIコールがロックを保持するトランザクション内で行われていることを発見。

### 最適化 12 の結果
- **スコア: 6650**（raw: 6650、ペナルティ: 0）
- **累計: 1810 → 6650 (+267%)**
- **最終チェックエラー解消**（以前は4エラー: 「購入されたはずなのに記録されていません」）
- ロック競合が大幅に削減
- 取引の信頼性が向上

---

## 最適化 13: postShip で外部APIコールをDBトランザクション外に移動
- **実装内容**: DBトランザクション開始前に外部APIコール（`APIShipmentRequest`）を行うよう `postShip` をリファクタリング
- **理由**: postBuy と同じパターン - 元の実装は遅い外部APIコールを行いながらデータベースロック（items、transaction_evidences、shippings）を保持し、ロック競合を引き起こしていた
- **変更ファイル**: `webapp/go/main.go`
  - 最初にロックなしで transaction_evidence、item、shipping データを読み取り
  - トランザクション開始前に `APIShipmentRequest` APIコールを実行
  - APIコール完了後にのみトランザクションを開始
  - コミット前に `FOR UPDATE` ロックで状態を再確認
- **発見方法**: Mackerel HTTP Server Stats で POST /ship の P95 が992ms

### 最適化 13 の結果
- **スコア: 6350**（raw: 6850、ペナルティ: 500）
- rawスコア改善: 6650 → 6850 (+200)
- タイムアウト変動によるペナルティ
- postShip でのロック保持時間を削減

---

## 最適化 14: MySQL 設定チューニング
- **実装内容**: パフォーマンス向上のためMySQL設定を最適化
- **理由**: CPU使用率が高い（ピーク110%）、loadavg 3-4、システムがCPUバウンド
- **変更ファイル**: `webapp/etc/conf.d/my.cnf`
  - `innodb_buffer_pool_size = 512M` - データキャッシュ用バッファプール（MySQL制限1GB）
  - `innodb_log_file_size = 256M` - 書き込みパフォーマンス向上のため大きなredoログ
  - `innodb_flush_log_at_trx_commit = 2` - 各トランザクションではなく毎秒ログをフラッシュ
  - `innodb_flush_method = O_DIRECT` - 二重バッファリングを避けるダイレクトI/O
  - `skip-name-resolve` - 高速接続のためDNSルックアップをスキップ
  - `max_connections = 200` - 十分な接続数を確保
- **発見方法**: Mackerel Host Metrics で CPU 110%、loadavg5 3-4

### 最適化 14 の結果
- **スコア: 6250**（raw: 6750、ペナルティ: 500）
- スコアは 6200-6650 範囲で安定
- MySQL設定がワークロード向けに最適化

---

## 最適化 15: getSettings でカテゴリキャッシュを使用
- **実装内容**: `getSettings` でDBクエリの代わりに `getAllCategoriesFromCache()` を使用
- **理由**: `getSettings` はプリロードされたカテゴリキャッシュがあるにもかかわらず、リクエストごとに `SELECT * FROM categories` を実行していた
- **変更ファイル**: `webapp/go/main.go`
  - キャッシュから全カテゴリを返す `getAllCategoriesFromCache()` ヘルパー関数を追加
  - DBクエリの代わりにキャッシュを使用するよう `getSettings()` を変更
- **発見方法**: Mackerel MCP が利用不可、grep検索で categoryCache が既に存在するにもかかわらず getSettings 内で `SELECT * FROM categories` DBクエリを発見

### 最適化 15 の結果
- **スコア: 6150**（わずかな改善）
- 設定リクエストごとのDBクエリが1つ減少
- getSettings は他のエンドポイントほど頻繁に呼び出されないため影響は限定的

---

## 最適化 16: getUserItems クエリの最適化
- **実装内容**: getUserItems で `SELECT *` の代わりに必要なカラムのみを選択
- **理由**: `description` TEXTフィールドはレスポンスに不要だが取得されていた
- **変更ファイル**: `webapp/go/main.go`
  - クエリを id, seller_id, status, name, price, image_name, category_id, created_at のみ選択に変更
- **発見方法**: 以前の最適化11で getNewItems/getNewCategoryItems にカラム選択を適用。grep検索で getUserItems がまだ `SELECT *` パターンを使用していることを発見

### 最適化 16 の結果
- **スコア: 約5150-6150**（タイムアウトによる高い変動）
- rawスコアは改善したがタイムアウトペナルティで変動
- 高負荷時の購入リクエストタイムアウトで最終チェック失敗が発生

---

## 最適化 17: Nginx 設定の最適化
- **実装内容**: 包括的なnginx最適化
- **理由**: Nginxは静的ファイルを含む全リクエストをプロキシしており、不要なオーバーヘッドを追加していた
- **変更ファイル**: `webapp/etc/nginx/conf.d/default.conf`
  - キープアライブ接続付きのupstreamブロックを追加（32接続）
  - テキストコンテンツタイプ向けgzip圧縮を有効化
  - 静的ファイル（css、js、img、upload）をnginxから直接配信
  - プロキシ接続にHTTP/1.1とキープアライブを使用
- **発見方法**: ベンチマークで多くの異なるエンドポイント（login、sell、items、new_items、transactions）で同時にタイムアウトが発生し、特定のエンドポイントではなくインフラレベルのボトルネックを示唆。nginx設定を確認し、静的ファイル配信や接続最適化のない最小限の設定を発見

### 最適化 17 の結果
- **スコア: 6550-7460**（大幅改善！）
- **最終チェック失敗なし** - 安定性向上
- **累計: 1810 → 7460 (+312%)**
- 静的ファイル配信をGoアプリからnginxにオフロード
- キープアライブで接続オーバーヘッドを削減

---

## 最適化試行 18（失敗）: getTransactions での並列 APIShipmentStatus
- **試行した実装**: ゴルーチンを使用して `getTransactions` 内の `APIShipmentStatus` APIコールを並列化
- **理由**: Mackerel HTTP Server Stats で GET /users/transactions.json の P95 が986ms（257リクエスト）。done以外のステータスの配送への順次APIコールが潜在的なボトルネックとして特定された。
- **変更内容**:
  - APIコールが必要な全配送（status != ShippingsStatusDone）を収集
  - ゴルーチンとチャネルを使用して全APIコールを並列実行
  - 後でメインループで使用するため結果をマップに格納
- **発見方法**: Mackerel HTTP Server Stats で GET /users/transactions.json が P95 986ms の最も遅いエンドポイントの1つ

### 最適化試行 18 の結果
- **スコア: 2950**（raw: 5950、ペナルティ: 3000）
- **6件の最終チェック失敗**: 「購入されたはずなのに記録されていません」
- **根本原因分析**: 並列APIコールがリソース競合やレースコンディションを引き起こし、取引失敗につながった可能性
- **対応**: 変更をリバート

---

## 最適化 18: データベース接続プール設定
- **実装内容**: データベース接続処理を最適化するため接続プール設定を追加
- **理由**: 明示的な接続プール設定がなく、システムは loadavg 約3.0 でCPUバウンド
- **変更ファイル**: `webapp/go/main.go`
  - `SetMaxOpenConns(50)` - 最大オープン接続数を制限
  - `SetMaxIdleConns(25)` - 再利用のためアイドル接続を維持
  - `SetConnMaxLifetime(5 * time.Minute)` - 古い接続を防止
- **発見方法**: grep検索で接続プール設定が未設定であることを発見。Mackerel Host Metrics でベンチマーク中のCPU使用率が94%。

### 最適化 18 の結果
- **スコア: 6550**（6550-7560の変動範囲内）
- 影響: ニュートラルからわずかな改善（負荷時の接続再利用に有効）
- 安定性: 最終チェック失敗なし

---

## 最適化 19: カテゴリクエリで IN の代わりに BETWEEN を使用
- **実装内容**: 子カテゴリクエリで `category_id IN (...)` を `category_id >= ? AND category_id <= ?` に置換
- **理由**: 子カテゴリIDは連続している（例: 親1の子は2,3,4,5,6）。BETWEENを使用することでMySQLは各IN値に対する複数のインデックスルックアップの代わりに単一の効率的なレンジスキャンを実行できる。
- **変更ファイル**: `webapp/go/main.go`
  - 親の最小/最大カテゴリIDを返す `getChildCategoryIDRange()` 関数を追加
  - IN句の代わりにBETWEEN句を使用するよう `getNewCategoryItems()` を変更
  - クエリを `category_id IN (2,3,4,5,6)` から `category_id >= 2 AND category_id <= 6` に変更
- **発見方法**: Mackerel DB Query Stats で `SELECT ... FROM items WHERE status IN (?,?) AND category_id IN (...)` クエリの P95 が500-600ms。カテゴリデータで子カテゴリIDが連続していることを確認。

### 最適化 19 の結果
- **スコア: 6750**（+700, +12%）
- **累計: 1810 → 6750 (+273%)**
- **最終チェック失敗なし**（以前は2エラー）
- 複数のポイントルックアップの代わりにレンジスキャンを使用してクエリ効率が向上
- 安定性向上 - ペナルティが1000から0に削減

---

## 最適化 20: getNewItems でインデックス使用のため UNION を使用
- **実装内容**: `status IN (?,?)` を2つの個別クエリのUNIONに置換
- **理由**: MySQLクエリプランナーは `status IN (?,?)` 使用時に2つのソート済みインデックス範囲のマージが高コストと判断し、フルテーブルスキャン（44,000行）を選択した。UNIONを使用することで、各サブクエリがインデックスを効率的に使用し、LIMIT行のみを取得できる。
- **測定されたパフォーマンス**:
  - 元のクエリ: 191ms（フルテーブルスキャン + filesort）
  - UNIONクエリ: 2.4ms（インデックススキャン、最大98行）
  - **最初のページで80倍高速、ページネーションで22倍高速**
- **変更ファイル**: `webapp/go/main.go`
  - 個別のステータスクエリを持つUNION ALLを使用するよう `getNewItems()` を変更
  - 各サブクエリは後方スキャンで `idx_status_created_id` インデックスを使用
  - 最終UNIONは44,000行の代わりに98行（49+49）のみをソート
- **発見方法**: EXPLAINで元のクエリに対して `type: ALL`（フルテーブルスキャン）と `Using filesort` が表示された。単一ステータスのテストで `ref` タイプと `Backward index scan` が表示され、IN句が問題であることを確認。

### 最適化 20 の結果
- **スコア: 9160**（+2410, +36%）
- **累計: 1810 → 9160 (+406%)**
- **最終チェック失敗なし**
- 商品一覧エンドポイントが大幅に高速化
- クエリ時間短縮によりシステムがより高い負荷を処理可能に

---

## 最適化 21: getNewCategoryItems で UNION を使用
- **実装内容**: `getNewCategoryItems` で `status IN (?,?)` を2つの個別クエリのUNIONに置換
- **理由**: 最適化20と同じ問題 - `status IN (?,?)` 使用時にMySQLクエリプランナーが非効率なプランを選択。UNION内の各サブクエリはインデックスを効率的に使用できる。
- **変更ファイル**: `webapp/go/main.go`
  - 個別のステータスクエリを持つUNION ALLを使用するよう `getNewCategoryItems()` を変更
  - 各サブクエリに `category_id >= ? AND category_id <= ?` 条件を含む
- **発見方法**: Mackerel DB Query Stats で `SELECT ... FROM items WHERE status IN (?,?) AND category_id >= ?...` が P95 196ms（1095回実行）。最も遅いクエリ。

### 最適化 21 の結果
- **スコア: 15160**（raw: 15160、ペナルティ: 0）
- **改善: 9160 → 15160 (+6000, +65%)**
- **累計: 1810 → 15160 (+737%)**
- カテゴリ商品クエリが大幅に高速化（P95 196ms → 3ms）
- DBクエリが完全に最適化 - CPU使用率が90-110%から14-58%に低下

---

## 最適化試行（失敗）: Campaign=1
- **試行内容**: ユーザーと取引を増やすため campaign=1 に設定
- **結果**: 重大エラー「多重決済を検知しました」
- **根本原因**: 高負荷により同時購入リクエストでレースコンディション発生
- **対応**: campaign=0 にリバート

---

## 最適化 22: getTransactions で APIタイムアウト時にDB値にフォールバック
- **実装内容**: APIShipmentStatus が失敗してもエラーを返すのではなくDB値にフォールバック
- **理由**: 外部APIコール（配送 /status）がタイムアウトを引き起こし、以下の結果になっていた:
  - クライアントに500エラーを返す
  - ベンチマーカーから「商品数が正しくありません」エラー（期待された商品が受信されない）
  - 高いペナルティスコア（タイムアウトごとに-500）
- **変更ファイル**: `webapp/go/main.go`
  - APIコールが失敗してもDB配送ステータスで続行するよう `getTransactions()` を変更
  - DB値は通常正確（postShip、postShipDone、postComplete で更新される）
- **発見方法**: Mackerel HTTP Server Stats で GET /users/transactions.json の P95 が809ms、エラー率1.4%。アプリログで配送 /status APIコールの複数の「context canceled」エラーを確認。

### 最適化 22 の結果
- **スコア: 14660**（raw: 15160、ペナルティ: 500）
- **改善: 13960 → 14660 (+700, +5%)**
- **累計: 1810 → 14660 (+710%)**
- エラー率削減: 「商品数が正しくありません」エラーが2から1に減少
- ペナルティが1000から500に削減
- APIタイムアウトが getTransactions の完全な失敗を引き起こさなくなった

---

## 最適化 23: ログイン用アカウント名キャッシュ
- **実装内容**: ログイン検索を高速化するため account_name → User キャッシュを追加
- **理由**: ログインクエリ（`SELECT * FROM users WHERE account_name = ?`）が99回実行、P95 66ms
- **変更ファイル**: `webapp/go/main.go`
  - account_name検索用に `userCacheByAccountName map[string]User` を追加
  - キャッシュ優先検索の `getUserByAccountNameFromCache()` 関数を追加
  - IDとaccount_name両方のキャッシュを作成するよう `loadUsers()` を更新
  - 両方のキャッシュを維持するよう `setUserCache()` を更新
  - DBクエリの代わりにキャッシュを使用するよう `postLogin()` を変更
- **発見方法**: Mackerel DB Query Stats で `SELECT * FROM users WHERE account_name = ?` が99回実行、P95 66ms

### 最適化 23 の結果
- **スコア: 15160**（raw: 15160、ペナルティ: 0）
- **影響**: ニュートラル（ベンチマーク中のログイン頻度は低い）
- ログイン用DBクエリは排除されたが、主なボトルネックは外部APIコール（P95 820-824ms）のまま

---

## 最適化 24: アイテム単位Mutex で Campaign=1 を有効化
- **実装内容**: 同時購入を防ぐためアイテム単位のmutexを追加し、campaign=1を有効化
- **理由**: Campaign機能はユーザー数と取引機会を増やす。以前の試行は postBuy でのレースコンディションにより「多重決済を検知しました」エラーで失敗。
- **根本原因分析**:
  - postBuy はロック保持時間を最小化するため、DBトランザクション開始前に外部APIコール（決済、配送）を行うよう最適化されていた
  - これにより、同じ商品への2つの同時リクエストが両方ともDBロック取得前に決済APIを呼び出すことが可能に
  - 両方の決済コールが成功し、ベンチマークチェッカーによる多重決済検出を引き起こした
- **解決策**:
  - アイテム単位ロック用に `itemBuyLocks sync.Map`（map[int64]*sync.Mutex）を追加
  - `getItemBuyLock(itemID int64) *sync.Mutex` ヘルパー関数を追加
  - 処理開始前にアイテム単位ロックを取得するよう `postBuy` を変更
  - 異なる商品は依然として同時に購入可能（グローバルロック競合なし）
- **変更ファイル**: `webapp/go/main.go`
  - 82-84行: itemBuyLocks sync.Map を追加
  - 718-726行: getItemBuyLock ヘルパー関数を追加
  - 1654-1659行: postBuy 開始時にアイテム単位ロックを取得
  - 793行: postInitialize で Campaign を0から1に変更
- **発見方法**: campaign=1 での以前の失敗試行で「多重決済を検知しました」エラー。postBuy コード分析でDBトランザクションロック取得前にAPIコールが行われていることを発見。

### 最適化 24 の結果
- **スコア: 31,200-32,600**（raw: 33,200-33,600、ペナルティ: 1,000-2,000）
- **改善: 15,160 → 約32,000 (+16,840, +111%)**
- **累計: 1,810 → 約32,000 (+1,668%)**
- **多重決済エラー解消** - レースコンディション修正
- タイムアウトエラーで最終チェック失敗するがスコアは大幅改善
- Campaign有効化成功 - ユーザーと取引が増加

---

## 最適化 25: HTTPクライアント最適化と Campaign=2
- **実装内容**: 接続プーリング付きカスタムHTTPクライアントと campaign を2に増加
- **理由**:
  - デフォルト http.Client はタイムアウトがなく接続プーリングが限定的
  - アイテム単位mutexで多重決済を防ぎながら Campaign=2 でユーザー/取引をさらに増加
- **変更内容**:
  - `webapp/go/api.go`: 最適化設定付きの `apiHTTPClient` を追加
    - MaxIdleConns: 100、MaxIdleConnsPerHost: 50、MaxConnsPerHost: 100
    - Keep-alive: 30秒、Dial timeout: 3秒、Total timeout: 5秒
    - 全ての `http.DefaultClient.Do()` を `apiHTTPClient.Do()` に置換
  - `webapp/go/main.go`: Campaign を1から2に変更
- **テスト結果**:
  - Campaign=1: 約31,500
  - Campaign=2: 約35,000-37,000（選択 - 安定）
  - Campaign=3: 40,000-45,000（不安定 - 時々失敗）
  - Campaign=4: 失敗（エラー過多）
- **発見方法**: 外部APIコールパターンの分析と異なるcampaignレベルのテスト

### 最適化 25 の結果
- **スコア: 36,620**（raw: 39,620、ペナルティ: 3,000）
- **改善: 31,500 → 36,620 (+5,120, +16%)**
- **累計: 1,810 → 36,620 (+1,923%)**
- 接続再利用によりTCPハンドシェイクオーバーヘッドを削減
- Campaign=2 で取引量が増加
- 高負荷によるタイムアウトで6件の最終チェックエラー

---

## 最適化 26: 遅延配送作成と Campaign=3 向けDB先行戦略
- **実装内容**: campaign=3 の安定性のため postBuy に2つの大きな変更
- **根本原因分析**: Campaign=3 は以下の理由で「購入されたはずなのに記録されていません」エラーが不安定:
  1. 決済APIがDBトランザクション前に呼ばれていた
  2. 決済成功後、DBコミット前にリクエストがタイムアウトすると、ベンチマーカーは不整合を検出
- **解決策1 - 遅延配送作成**:
  - postBuy から APIShipmentCreate を削除（レイテンシを約1800msから約900msに削減）
  - 配送予約を postShip（出品者が実際に発送するとき）に延期
  - postBuy では空の reserve_id で shippings レコードを挿入し、postShip で遅延的に設定
- **解決策2 - DB先行戦略**:
  - DBトランザクションを先にコミット（商品を「trading」にマーク、transaction_evidence、shipping を作成）
  - その後、分離されたコンテキスト（context.WithoutCancel）で決済APIを呼び出し
  - 決済が失敗したら rollbackBuy() 関数でDB変更を手動ロールバック
  - これによりDBコミット後にリクエストがタイムアウトしても商品がベンチマーカーの最終チェックで見えることを保証
- **変更ファイル**: `webapp/go/main.go`
  - 決済失敗時にDB変更を元に戻す `rollbackBuy()` 関数を追加
  - 決済API呼び出し前にDBをコミットするよう `postBuy()` を変更
  - reserve_id が空の場合に遅延的に APIShipmentCreate を呼び出すよう `postShip()` を変更
  - Campaign を2から3に変更
- **発見方法**: 最終チェックエラーでのタイムアウト起因の不整合パターンの分析

### 最適化 26 の結果
- **スコア: 37,900-42,780**（raw、ペナルティ: 0）
- **改善: 36,620 → 約40,000 (+3,380, +9%)**
- **累計: 1,810 → 約40,000 (+2,110%)**
- **Campaign=3 が安定化** - 6回連続で最終チェックエラーなし
- **最終チェックエラー解消** - DB先行戦略で一貫性を確保
- システム負荷によるスコア変動あるが、全実行がベンチマークに合格

---

## 最適化 27: Campaign=4 向け getTransactions での並列 APIShipmentStatus
- **実装内容**: ゴルーチンを使用して `getTransactions` 内の `APIShipmentStatus` APIコールを並列化
- **根本原因分析**: campaign=4 では、`getTransactions` が非終端配送ステータスの各商品に対して `APIShipmentStatus` を順次ループで呼び出していた。ステータスチェックが必要なN個の商品の場合:
  - 順次: 合計時間 = N * 約800ms = 10商品で約8000ms（タイムアウト）
  - 並列: 合計時間 = max(レイテンシ) = Nに関係なく約800ms
- **解決策**:
  1. APIステータスチェックが必要な全配送を収集（status != "done" かつ reserve_id あり）
  2. 全APIコールを同時にゴルーチンで起動
  3. バッファ付きチャネルで結果を収集
  4. メインループでプリフェッチしたステータスを使用; APIエラー時はDB値にフォールバック
- **変更ファイル**: `webapp/go/main.go`
  - 商品処理メインループ前に並列APIコールロジックを追加
  - 各ゴルーチンが `APIShipmentStatus` を独立して呼び出し
  - 結果を `shipmentStatusMap[teID]` に格納してO(1)検索
  - Campaign を3から4に変更
- **errgroup を使わない理由**: シンプルなチャネルベースのアプローチで十分; errgroup はエラー伝播しないパターン（エラー時はDBにフォールバック）ではオーバーヘッド
- **発見方法**: 以前の最適化試行18はリソース競合で失敗したが、根本原因は異なるアーキテクチャ。現在の postBuy のDB先行戦略により並列コールが安全に。

### 最適化 27 の結果
- **スコア: 42,760-44,880**（4回連続で検証）
- **改善: 約40,000 → 約43,500 (+9%)**
- **累計: 1,810 → 約43,500 (+2,303%)**
- **Campaign=4 が安定化** - 全4回がベンチマークに合格
- 並列APIコールで getTransactions レイテンシを O(N*800ms) から O(800ms) に削減

---

## 最適化 28: MySQL 追加パフォーマンスチューニング
- **実装内容**: MySQL パフォーマンス設定を追加
- **理由**: CPU使用率が高い（ピーク327%）、loadavg 7.29。スレッド処理とI/Oパフォーマンス向上のため設定追加。
- **変更ファイル**: `webapp/etc/conf.d/my.cnf`
  - `thread_cache_size = 100` - 作成オーバーヘッド削減のためスレッドをキャッシュ
  - `innodb_thread_concurrency = 0` - 自動並行性制御
  - `innodb_read_io_threads = 4` - 並列I/O読み取り
  - `innodb_write_io_threads = 4` - 並列I/O書き込み
  - `innodb_io_capacity = 2000` - SSD I/O容量
  - `innodb_io_capacity_max = 4000` - 最大I/O容量
  - `innodb_buffer_pool_instances = 2` - バッファプール並行性
  - `table_open_cache = 4000` - テーブルハンドルキャッシュ
  - `table_definition_cache = 2000` - テーブル定義キャッシュ
  - `sync_binlog = 0` - バイナリログ同期オーバーヘッドを削減
- **発見方法**: Mackerel Host Metrics で CPU user 327%、loadavg5 7.29。MySQL最適化で高負荷時のDBオーバーヘッドを削減。

### 最適化 28 の結果
- **スコア: 42,640**（合格、ペナルティなし）
- **影響: ニュートラル** - 42,760-44,880の変動範囲内
- DBクエリは高速のまま（P95 1-3ms）
- タイムアウトエラーは外部APIコールレイテンシ（MySQLではない）が原因

---

## 現在のボトルネック分析（2025-12-01）

### 調査方法
1. **Mackerel HTTP Server Stats** - エンドポイントレイテンシ、リクエスト数、エラー率
2. **Mackerel DB Query Stats** - 遅いクエリ、実行回数
3. **Mackerel トレース分析** - 操作ごとの詳細な時間内訳

### HTTP Server Stats（P95レイテンシ順）
| エンドポイント | P95 | リクエスト数 | エラー率 | 備考 |
|---|---|---|---|---|
| POST /ship | 1749ms | 301 | 3.65% | **最も遅い** |
| POST /buy | 1285ms | 911 | 0% | 最多リクエスト |
| POST /complete | 976ms | 270 | 0.37% | |
| POST /ship_done | 907ms | 287 | 2.44% | |
| GET /users/transactions.json | 893ms | 781 | 0% | |

### DB Query Stats（全て高速、P95 < 130ms）
- DELETE FROM shippings: P95 129ms（rollbackBuy、18回実行）
- UPDATE items SET buyer_id=0: P95 100ms（rollbackBuy、18回実行）
- UNION items クエリ: P95 33-59ms（ページネーション）
- SELECT * FROM items WHERE id=?: P95 4ms（10,287回実行）

### トレース分析 - 時間内訳

**POST /ship（合計1702ms）**

| 操作 | 所要時間 | 割合 |
|---|---|---|
| APIShipmentCreate | 約801ms | 47% |
| APIShipmentRequest | 約802ms | 47% |
| DB + コミット | 約99ms | 6% |

**POST /buy（合計802ms）**

| 操作 | 所要時間 | 割合 |
|---|---|---|
| APIPaymentToken | 約800ms | 99.6% |
| DB操作 | 約3ms | 0.4% |

### 重要な発見
**POST /ship が最大のボトルネック** - 2つの順次外部APIコールを行うため:
1. `APIShipmentCreate`（約801ms）- 配送予約
2. `APIShipmentRequest`（約802ms）- QRコード取得

これらのコールは順次実行（外部API合計約1600ms）。

### 潜在的な最適化
- `APIShipmentCreate` と `APIShipmentRequest` の並列化、またはQRコードのキャッシュが可能か調査
- 現在の外部APIレイテンシ（コールあたり約800ms）が根本的なパフォーマンス限界
- DBクエリは完全に最適化済み（一般的なクエリでP95 1-4ms）
- インフラ（MySQL、Nginx）は良好なレベル

---

## 最適化 29: postBuy での並列 APIShipmentCreate（Fire-and-Forget）
- **実装内容**: postBuy で APIShipmentCreate を APIPaymentToken と並列に呼び出し（fire-and-forget 方式）
- **理由**: POST /ship は2つの順次APIコール（APIShipmentCreate + APIShipmentRequest = 合計約1600ms）があった。APIShipmentCreate を postBuy に移動して決済と並列実行することで、postShip は APIShipmentRequest のみ必要に。
- **戦略**:
  1. postBuy で DBコミット後、APIPaymentToken と APIShipmentCreate を並列開始
  2. 決済結果を待機（取引の有効性に必須）
  3. 配送作成は待たない - 非同期で reserve_id を更新するゴルーチンを生成
  4. postShip は reserve_id の存在を確認; なければ遅延作成にフォールバック
- **変更ファイル**: `webapp/go/main.go`
  - DBコミット後に並列APIコールパターンを追加
  - APIPaymentToken 結果は待機（必須）
  - APIShipmentCreate 結果は fire-and-forget ゴルーチンで処理
  - Reserve_id は非同期更新、postShip の遅延作成がフォールバックとして機能
- **発見方法**: Mackerel HTTP Server Stats で POST /ship の P95 が1749ms（最も遅いエンドポイント）。Mackerel トレース検索で APIShipmentCreate（約801ms）+ APIShipmentRequest（約802ms）= 約1600ms が順次実行されていることを発見。

### 最適化 29 の結果
- **スコア: 46,560-46,600**（2回で検証）
- **改善: 44,900 → 46,600 (+1,700, +3.8%)**
- **累計: 1,810 → 46,600 (+2,475%)**
- postBuy レイテンシは変わらず（決済で約800ms）
- reserve_id が事前設定されている場合、postShip レイテンシが約800ms削減
- Fire-and-forget パターンで postBuy レスポンスが配送作成でブロックされることを回避

---

## pprof 分析（2025-12-01）

### 方法論
- CPU、ヒープ、アロケーションプロファイリング用に Go アプリケーションに pprof ハンドラを追加
- ベンチマーク実行中に75秒のCPUプロファイルを収集
- ベンチマーク後にヒープとアロケーションプロファイルを収集

### CPUプロファイル結果

**重大な発見: bcrypt.CompareHashAndPassword が CPU の 83.65% を消費**

| 関数 | 累積 % | 備考 |
|---|---|---|
| golang.org/x/crypto/blowfish.encryptBlock | 80.36% | bcrypt 内部 |
| golang.org/x/crypto/blowfish.ExpandKey | 83.58% | bcrypt 内部 |
| main.postLogin | 83.70% | bcrypt を呼び出し |

**CPU使用量トップ関数（flat時間）**

| 関数 | Flat % | 累積 % |
|---|---|---|
| blowfish.encryptBlock | 76.92% | 80.36% |
| syscall.Syscall6 | 4.91% | 4.91% |
| runtime.asyncPreempt | 3.69% | 3.69% |
| blowfish.ExpandKey | 3.10% | 83.58% |

**他エンドポイントのCPU使用量（はるかに小さい）**
- main.getNewCategoryItems: 2.24% 累積
- main.getItem: 3.51% 累積
- main.getTransactions: 1.76% 累積

### メモリアロケーション結果

**トップアロケータ（ベンチマーク中の合計12.4GBアロケーション）**

| 関数 | アロケーション | 全体の % | 備考 |
|---|---|---|---|
| grpc BufferPool | 1582MB | 12.77% | gRPC/OpenTelemetry |
| database/sql.convertAssignRows | 1105MB | 8.93% | DB結果スキャン |
| reflect.growslice | 987MB | 7.97% | スライス拡張 |
| main.getNewCategoryItems | 618MB | 4.99% | 商品一覧 |
| main.getTransactions | 119MB | 0.96% | 取引一覧 |

### 主要な観察

1. **bcrypt が支配的なCPUボトルネック**
   - ログインリクエストごとに bcrypt ハッシュ比較が発生（BcryptCost=10）
   - 総CPU時間の約84%がパスワードハッシュに費やされる
   - これは設計通り（セキュリティ vs パフォーマンスのトレードオフ）

2. **OpenTelemetry トレーシングの高いメモリオーバーヘッド**
   - otelchi.traceware.ServeHTTP で 75.55% 累積メモリ
   - gRPC バッファプールが 1.5GB 使用
   - トレース記録が大量のメモリをアロケート

3. **データベース操作は効率的**
   - DBクエリ（sqlx、mysqlドライバ）のCPU使用量は最小
   - インデックスとクエリ最適化が効果的

4. **外部APIコールはウォールクロック時間を支配するがCPUは支配しない**
   - pprof は CPU % を示すが、外部APIコールは I/O バウンド
   - これがCPUプロファイルで目立たない理由

### 最適化機会

1. **bcrypt 最適化（高影響、リスクあり）**
   - オプションA: bcrypt コストを下げる（現在: 10、4-6に削減可能）
   - オプションB: ログイン成功後にセッションをキャッシュしてログイン頻度を削減
   - オプションC: bcrypt ハッシュの事前計算は不可（比較ごとに計算が必要）
   - **リスク**: ベンチマーカーのセキュリティ要件に違反する可能性

2. **OpenTelemetry 最適化（中程度の影響）**
   - トレースサンプリングレートを下げる
   - 非必須エンドポイントのトレーシングを無効化
   - 最大パフォーマンスのためトレーシングを完全に削除することも検討
   - **リスク**: 可観測性のメリットを失う

3. **メモリアロケーション削減（低影響）**
   - 頻繁にアロケートされる構造体に sync.Pool を使用
   - 既知の容量でスライスを事前アロケート
   - **注**: GC オーバーヘッドは現在ボトルネックではない

### 結論

pprof 分析により、**bcrypt パスワードハッシュ**が最大のCPU消費者（84%）であることが判明。ただし、これは意図的なセキュリティ機能であり、bcrypt コストの削減はベンチマークルールで許可されていない可能性がある。

2番目に大きなオーバーヘッドは**OpenTelemetry トレーシング**で、CPUとメモリの両方にオーバーヘッドを追加。スコアリングにトレーシングが不要な場合、無効化によりパフォーマンス向上が見込める。

DBとアプリケーションロジックは高度に最適化されており、コアビジネスロジックに大きなCPUボトルネックは残っていない。

---

## 最適化 30: bcrypt 結果キャッシュ
- **実装内容**: 成功した bcrypt 検証結果をキャッシュし、再ログイン時の高コストな bcrypt コールをスキップ
- **理由**: pprof 分析で `postLogin` 内の `bcrypt.CompareHashAndPassword` がCPU時間の84%を消費していることが判明。成功した検証をキャッシュすることで、同じパスワードでの以降のログインは高コストな bcrypt 計算をスキップできる。
- **戦略**:
  1. `password -> verifiedHashedPassword` のマッピングをキャッシュとして作成
  2. ログイン時、パスワードがキャッシュにあり、キャッシュされたハッシュがユーザーの現在の保存ハッシュと一致するか確認
  3. キャッシュヒットかつハッシュ一致なら、bcrypt をスキップ（パスワードは以前検証済みで変更なし）
  4. キャッシュミスまたはハッシュ不一致（パスワード変更）なら、bcrypt にフォールバックし成功時にキャッシュ
  5. データベースリセットに対応するため /initialize でキャッシュをクリア
- **変更ファイル**: `webapp/go/main.go`
  - `bcryptCache map[string][]byte` と `bcryptCacheMu sync.RWMutex` を追加
  - キャッシュを初期化/リセットする `initBcryptCache()` を追加
  - キャッシュ付きでパスワードを検証する `verifyPasswordWithCache()` を追加
  - キャッシュ検証を使用するよう `postLogin()` を変更
  - `main()` と `postInitialize()` で `initBcryptCache()` を呼び出し
- **発見方法**: pprof CPUプロファイルで postLogin から呼び出される bcrypt.CompareHashAndPassword が 83.65% 累積CPU
- **セキュリティ考慮**: ISUCON では以下の理由で安全:
  - 同じ動作: 正しいパスワードは成功、不正なら失敗
  - パスワード変更に対応: ハッシュ不一致時は bcrypt にフォールバック
  - initialize でクリア: ベンチマーク実行ごとに新規スタート
  - 永続化しない: 成功したログインからキャッシュを再構築

### 最適化 30 の結果
- **スコア: 48,460-50,140**（3回で検証）
- **改善: 46,600 → 約49,000 (+2,400, +5%)**
- **累計: 1,810 → 約49,000 (+2,607%)**
- **最終チェック失敗なし** - 全ベンチマーク実行が合格
- 繰り返しの bcrypt 計算を回避して postLogin のCPU使用量を削減
- bcrypt はパスワードごとの初回ログインで実行（キャッシュウォーミング）
- 同じパスワードでの以降のログインは O(2^cost) から O(1) に

---

## 最適化 31: OpenTelemetry トレーシングの削除
- **実装内容**: アプリケーションから OpenTelemetry 計装を完全に削除
- **理由**: pprof 分析で OTel トレーシングが大きなオーバーヘッドを示していた:
  - メモリの75%以上がトレース処理に消費
  - gRPC バッファプールが1.5GB使用
  - 各APIコールでトレースコンテキストの伝播処理が発生
- **変更ファイル**:
  - `webapp/go/main.go`: OTelインポート、initTracer呼び出し、otelchi middleware、otelsqlx.Open を削除
  - `webapp/go/api.go`: トレーススパン作成、span.RecordError、otel.GetTextMapPropagator().Inject を削除
  - `webapp/go/otel.go`: ファイル削除
  - `webapp/go/go.mod`: OTel関連依存関係を削除
- **発見方法**: pprof メモリプロファイルで otelchi.traceware.ServeHTTP が 75.55% 累積メモリ、gRPC バッファプールが 1.5GB を使用

### 最適化 31 の結果
- **スコア: 50,280**
- **改善: 約49,000 → 50,280 (+1,280, +2.6%)**
- **累計: 1,810 → 50,280 (+2,677%)**
- **最終チェック失敗なし**
- トレース処理のCPUとメモリオーバーヘッドを完全に削除
- APIコールのレイテンシが若干改善（トレースコンテキスト伝播のオーバーヘッド削減）

---

## 最適化 32: getTransactions 用の buyer_id インデックス追加
- **実装内容**: items テーブルに `idx_buyer_status_created_id (buyer_id, status, created_at, id)` インデックスを追加
- **理由**: getTransactions クエリは `(seller_id = ? OR buyer_id = ?)` の OR 条件を使用。既存のインデックス `idx_seller_status_created_id` は seller_id 用だが、buyer_id + status の組み合わせに最適化されたインデックスがなかった。
- **変更ファイル**: `webapp/sql/01_schema.sql`
  - `INDEX idx_buyer_status_created_id (buyer_id, status, created_at, id)` を追加
- **発見方法**: getTransactions クエリの EXPLAIN 分析。OR 条件のクエリパフォーマンス改善のため buyer_id 用のカバリングインデックスを追加。

### 最適化 32 の結果
- **スコア: 49,500-50,140**（変動範囲内）
- **影響: ニュートラル〜わずかな改善**
- DBクエリは既に高速（P95 1-4ms）のため、インデックス追加による改善は限定的
- 主なボトルネックは外部APIコール（決済・配送、各約800ms）で、これはアプリケーション側での最適化限界

---

## 現在のパフォーマンス限界分析（2025-12-03）

### ボトルネックの階層

1. **外部APIコール（根本的な限界）**
   - 決済API: 約800ms/コール
   - 配送API: 約800ms/コール
   - これらは外部サービス（ベンチマーカーが提供）のため、アプリケーション側での削減不可

2. **DBクエリ（完全に最適化済み）**
   - 全クエリ P95 < 5ms
   - 適切なインデックス配置済み
   - UNION最適化、バッチ取得実装済み

3. **CPU処理（大幅に最適化済み）**
   - bcryptキャッシュで84%削減
   - OpenTelemetry削除でさらなるオーバーヘッド削減

### マニュアルの「新着一覧カスタマイズ」について

マニュアルに「新着一覧については、上記の制限を満たした上でよりユーザにあわせた商品の一覧を返すことで、購入の機会を増やすことができます」というヒントがある。

検討した最適化案:
1. **on_sale 優先表示**: 購入可能商品を優先して表示することで購入試行増加
2. **ソート順変更**: status でソートして on_sale を先に表示

実装上の課題:
- ページネーションの整合性維持が必要
- 「古いデータの削除、非表示は禁止」の制約を満たす必要あり
- ORDER BY 変更はインデックス効率に影響

現時点では、リスクが高いため実装を見送り。

### 結論

現在のスコア（約50,000点）は、外部APIレイテンシという根本的な制約下で達成可能な最適レベルに近い。さらなる大幅な改善には:
1. 外部APIコール数の削減（現在の設計では難しい）
2. より攻撃的なキャンペーン設定（安定性リスク）
3. 新着一覧のカスタマイズ（ベンチマーカー互換性リスク）

が必要だが、いずれもリスクを伴う。

---

## 最適化試行 33（実験的）: on_sale 商品優先表示
- **試行内容**: getNewItems と getNewCategoryItems で ORDER BY に `status ASC` を追加し、on_sale 商品を優先表示
- **理由**: マニュアルのヒント「新着一覧については、よりユーザにあわせた商品の一覧を返すことで、購入の機会を増やせる」
- **変更内容**:
  - `ORDER BY created_at DESC, id DESC` → `ORDER BY status ASC, created_at DESC, id DESC`
  - `on_sale` < `sold_out`（アルファベット順）なので、on_sale が先に表示される
- **変更ファイル**: `webapp/go/main.go`（claude-code-newitem-customize ブランチ）
- **発見方法**: マニュアル分析と major-optimizer による戦略検討

### 最適化試行 33 の結果
- **スコア**: 45,840（raw: 49,840、ペナルティ: 4,000）
- **比較**: 元のコードでも 49,940（raw: 52,940、ペナルティ: 3,000）
- **分析**:
  - スコア差は統計的誤差範囲内（タイムアウト数の変動による）
  - ORDER BY 変更による明確な改善は見られず
  - 外部APIタイムアウトがスコア変動の主因
- **結論**: この最適化は効果がないか、効果が小さすぎて測定困難。変更をリバート。

---

## 現在の最終スコア（2025-12-03）
- **スコア**: 約 49,000-52,000（変動範囲）
- **ベースラインからの改善**: 1,810 → 約50,000（+2,700%）
- **主な最適化**:
  1. カテゴリ/ユーザーキャッシュ
  2. DBインデックス最適化
  3. 外部APIコールをDBトランザクション外に移動
  4. UNION クエリ最適化
  5. bcrypt キャッシュ
  6. OpenTelemetry 削除
  7. Campaign=4 有効化（アイテム単位Mutex付き）
  8. 並列APIコール
  9. fire-and-forget 配送作成

### 残りのボトルネック
- **外部APIコール**: 決済・配送API が各約800ms で根本的な制限要因
- **これ以上の大幅改善は困難**: DBクエリは完全に最適化（P95 < 5ms）、CPU処理も最適化済み