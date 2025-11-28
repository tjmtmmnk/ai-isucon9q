# ISUCON Solver Log

### Baseline Benchmark
- **Score: 1810**
- Multiple timeout errors observed on `/users/transactions.json`, `/new_items.json`, `/login`

### Optimization 1: Category Caching
- **Implementation**: Cache all categories in memory at startup and after initialization
- **Changes**: `webapp/go/main.go`
  - Added `categoryCache` map with RWMutex
  - Added `loadCategories()` function to load all categories into cache
  - Modified `getCategoryByID()` to use cache first
  - Called `loadCategories()` in `main()` and `postInitialize()`
- **How to discover**: Mackerel DB Query Stats

#### Results After Optimization 1
- **Score: 2010 (+200, +11%)**
- Category queries eliminated from top queries (was 105,857 executions → now 10 cache loads)

### Optimization 2: User Caching
- **Implementation**: Cache users in memory with cache invalidation on updates
- **Changes**: `webapp/go/main.go`
  - Added `userCache` map with RWMutex
  - Added `loadUsers()` function to load all users into cache
  - Added `getUserByIDFromCache()` function for cache-first lookups
  - Added `setUserCache()` function for cache updates
  - Modified `getUserSimpleByID()` to use cache when not in transaction
  - Updated `postRegister()`, `postSell()`, `postBump()` to update cache
- **How to discover**: Mackerel DB Query Stats

#### Results After Optimization 2
- **Score: 2210 (+200, +10%)**
- **Cumulative: 1810 → 2210 (+22%)**
- User queries eliminated from top 20 (was 43,687 executions, 73,320ms)

### Optimization 3: Database Indexes for Items Table
- **Implementation**: Add composite indexes for common query patterns
- **Changes**: `webapp/sql/01_schema.sql`
  - Added `idx_status_created_id (status, created_at, id)` for new items queries
  - Added `idx_seller_status_created_id (seller_id, status, created_at, id)` for user items queries
  - Added `idx_buyer_created_id (buyer_id, created_at, id)` for transaction queries
- **Also fixed**: `getUserSimpleByID()` to always use cache (was bypassing cache in transactions)
- **How to discover**: Mackerel DB Query Stats

#### Results After Optimization 3
- **Score: 2610 (+400, +18%)**
- **Cumulative: 1810 → 2610 (+44%)**
- Items queries now use indexes instead of full table scans
- Previous slow queries (P95 ~1200ms) now significantly faster

### Optimization 4: Skip External API Calls for Terminal States
- **Implementation**: Skip `APIShipmentStatus` call in `getTransactions` when shipping status is already "done"
- **Rationale**: "done" is a terminal state that won't change, so external API call is unnecessary
- **Changes**: `webapp/go/main.go`
  - Modified `getTransactions()` to check `shipping.Status` before calling external API
  - If status is `ShippingsStatusDone`, use DB value directly instead of calling API
  - This eliminates N external API calls for completed transactions
- **How to discover**: Mackerel HTTP Server Stats

#### Results After Optimization 4
- **Score: 4550 (+1940, +74%)**
- **Cumulative: 1810 → 4550 (+151%)**
- Significant reduction in external API calls during transaction listing
- One timeout error occurred (-500 penalty) during high load

### Optimization 5: Parallel API Calls in postBuy
- **Implementation**: Execute APIShipmentCreate and APIPaymentToken in parallel using goroutines
- **Rationale**: These two external API calls are independent and can run concurrently
- **Changes**: `webapp/go/main.go`
  - Used goroutines with channels to execute both API calls simultaneously
  - Wait for both results before proceeding with transaction
  - Error handling preserved - rollback on either failure
- **How to discover**: Mackerel HTTP Server Stats - POST /buy had P95 of 2078ms

#### Results After Optimization 5
- **Score: 4850**
- **Cumulative: 1810 → 4850 (+168%)**
- Score variance observed due to timeout penalties
- POST /buy latency reduced by eliminating sequential API wait times

### Optimization 6: Batch Fetch in getTransactions
- **Implementation**: Batch fetch transaction_evidences and shippings using IN clause
- **Rationale**: N+1 problem - each item triggered individual queries for transaction_evidences and shippings
- **Changes**: `webapp/go/main.go`
  - Collect all item IDs upfront
  - Batch fetch all transaction_evidences with `WHERE item_id IN (...)`
  - Batch fetch all shippings with `WHERE transaction_evidence_id IN (...)`
  - Use maps for O(1) lookup in the main loop
- **How to discover**: Mackerel HTTP Server Stats - GET /users/transactions.json had P95 of 5668ms (slowest endpoint)

#### Results After Optimization 6
- **Score: 5650**
- **Cumulative: 1810 → 5650 (+212%)**
- Eliminated 2N database queries per getTransactions request
- GET /users/transactions.json now more efficient

### Optimization 7: Child Category ID Caching
- **Implementation**: Cache child category IDs in memory to avoid repeated DB queries
- **Rationale**: `SELECT id FROM categories WHERE parent_id=?` was executed 612 times (N+1 problem in getNewCategoryItems)
- **Changes**: `webapp/go/main.go`
  - Added `childCategoryCache map[int][]int` to store parent_id -> child_ids mapping
  - Updated `loadCategories()` to build the child category cache
  - Added `getChildCategoryIDs(parentID int) []int` helper function
  - Modified `getNewCategoryItems()` to use cache instead of DB query
- **How to discover**: Mackerel DB Query Stats - SELECT id FROM categories WHERE parent_id=? was executed 612 times with P95 of 83ms

#### Results After Optimization 7
- **Score: 4950** (variance due to timeout errors)
- Note: This optimization eliminates DB queries but the impact is small compared to other bottlenecks (items queries with P95 965-1192ms, external API calls)

### Optimization 8: Move External API Calls Outside DB Transactions
- **Implementation**: Move `APIShipmentStatus` calls before starting DB transaction in `postShipDone` and `postComplete`
- **Rationale**: External API calls were made while holding database locks (`FOR UPDATE`), causing:
  - Long lock hold times during slow API responses
  - Other requests blocked on the same rows
  - Timeouts and errors leading to inconsistent state
- **Changes**: `webapp/go/main.go`
  - `postShipDone`: Query shipping record before transaction, make API call, then start transaction for updates
  - `postComplete`: Same pattern - API call before transaction
  - Removed redundant `SELECT ... FOR UPDATE` on shippings table (only UPDATE needed)
- **How to discover**: Mackerel HTTP Server Stats showed POST /ship_done with 3.8% error rate and POST /complete with 1.7% error rate. Code review revealed API calls inside transaction blocks holding locks.

#### Results After Optimization 8
- **Score: 4850 (+1300 from 3550, +37%)**
- **No final check failures** (previously had "購入されたはずなのに記録されていません")
- Transaction completion reliability improved
- Reduced lock contention during external API calls

### Optimization 9: Fix getUser to Use Cache
- **Implementation**: Modify `getUser()` function to use `getUserByIDFromCache()` instead of direct DB query
- **Rationale**: `getUser()` is called on every authenticated request (12 call sites) but was bypassing the user cache
- **Changes**: `webapp/go/main.go`
  - Changed `getUser()` to call `getUserByIDFromCache()` instead of direct `SELECT * FROM users WHERE id = ?`
- **How to discover**: Mackerel DB Query Stats showed `SELECT * FROM users WHERE id = ?` with 2339 executions despite user cache being implemented

#### Results After Optimization 9
- User queries eliminated from DB query stats top 20
- Score variance unchanged but queries reduced

### Optimization 10: Add Composite Index for Category Queries
- **Implementation**: Add index `idx_category_status_created_id (category_id, status, created_at, id)` for category-based item queries
- **Rationale**: Category queries used `category_id IN (...)` with status and created_at, but existing index didn't include category_id
- **Changes**: `webapp/sql/01_schema.sql`
  - Added `INDEX idx_category_status_created_id (category_id, status, created_at, id)`
- **How to discover**: Mackerel DB Query Stats showed category queries with P95 700-900ms

#### Results After Optimization 10
- Minor improvement in category query performance

### Optimization 11: Avoid SELECT * in Item Listing Queries
- **Implementation**: Select only necessary columns instead of `SELECT *` in getNewItems and getNewCategoryItems
- **Rationale**: `SELECT *` retrieves `description` (TEXT field) which is not needed for item listing and adds I/O overhead
- **Changes**: `webapp/go/main.go`
  - Changed `getNewItems()` queries to select only: id, seller_id, status, name, price, image_name, category_id, created_at
  - Changed `getNewCategoryItems()` queries similarly
- **How to discover**: Mackerel DB Query Stats showed `SELECT * FROM items` queries with P95 1000-1700ms

#### Results After Optimization 11
- **Score: 5860-6860** (raw: 6750-7360 with timeout penalties)
- **Cumulative: 1810 → ~6500 average (+260%)**
- Significant reduction in item query I/O by excluding TEXT column

### Optimization 12: Move External API Calls Outside DB Transaction in postBuy
- **Implementation**: Refactor `postBuy` to make external API calls before starting the database transaction
- **Rationale**: The original implementation held database locks (`FOR UPDATE` on items and users) while making slow external API calls (500-1000ms). This caused:
  - Long lock hold times blocking other buy requests
  - Timeouts and errors leading to inconsistent state
  - Final check failures ("購入されたはずなのに記録されていません")
- **Changes**: `webapp/go/main.go`
  - Read item and seller info without lock initially
  - Make parallel external API calls (shipment + payment) before transaction
  - Start transaction only after API calls complete
  - Re-verify item status with `FOR UPDATE` lock before committing
  - Lock hold time reduced from (API time + DB time) to just (DB time)
- **How to discover**: Mackerel HTTP Server Stats showed POST /buy with 5.3% error rate (highest among transaction endpoints) and P95 of 999ms. Code analysis revealed API calls inside transaction holding locks.

#### Results After Optimization 12
- **Score: 6650** (raw: 6650, penalty: 0)
- **Cumulative: 1810 → 6650 (+267%)**
- **Final check errors eliminated** (previously 4 errors: "購入されたはずなのに記録されていません")
- Lock contention significantly reduced
- Transaction reliability improved

### Optimization 13: Move External API Calls Outside DB Transaction in postShip
- **Implementation**: Refactor `postShip` to make external API call (`APIShipmentRequest`) before starting the database transaction
- **Rationale**: Same pattern as postBuy - the original implementation held database locks (on items, transaction_evidences, shippings) while making slow external API calls, causing lock contention
- **Changes**: `webapp/go/main.go`
  - Read transaction_evidence, item, and shipping data without locks initially
  - Make `APIShipmentRequest` API call before starting transaction
  - Start transaction only after API call completes
  - Re-verify state with `FOR UPDATE` locks before committing
- **How to discover**: Mackerel HTTP Server Stats showed POST /ship with P95 of 992ms

#### Results After Optimization 13
- **Score: 6350** (raw: 6850, penalty: 500)
- Raw score improved: 6650 → 6850 (+200)
- Penalty due to timeout variance
- Lock hold time reduced in postShip

### Optimization 14: MySQL Configuration Tuning
- **Implementation**: Optimize MySQL settings for better performance
- **Rationale**: CPU usage was high (110% peak), loadavg 3-4, system was CPU-bound
- **Changes**: `webapp/etc/conf.d/my.cnf`
  - `innodb_buffer_pool_size = 512M` - Buffer pool for data caching (MySQL has 1GB limit)
  - `innodb_log_file_size = 256M` - Larger redo logs for write performance
  - `innodb_flush_log_at_trx_commit = 2` - Flush log every second instead of every transaction
  - `innodb_flush_method = O_DIRECT` - Direct I/O to avoid double buffering
  - `skip-name-resolve` - Skip DNS lookup for faster connections
  - `max_connections = 200` - Ensure enough connections
- **How to discover**: Mackerel Host Metrics showed CPU at 110%, loadavg5 at 3-4

#### Results After Optimization 14
- **Score: 6250** (raw: 6750, penalty: 500)
- Score stable in 6200-6650 range
- MySQL configuration now optimized for workload

### Optimization 15: Use Category Cache in getSettings
- **Implementation**: Use `getAllCategoriesFromCache()` instead of DB query in `getSettings`
- **Rationale**: `getSettings` was executing `SELECT * FROM categories` on every request despite having a pre-loaded category cache
- **Changes**: `webapp/go/main.go`
  - Added `getAllCategoriesFromCache()` helper function to return all categories from cache
  - Modified `getSettings()` to use cache instead of DB query
- **How to discover**: Mackerel MCP unavailable, grep search for `SELECT * FROM categories` found DB query in getSettings despite categoryCache already existing

#### Results After Optimization 15
- **Score: 6150** (small improvement)
- One less DB query per settings request
- Impact limited because getSettings is not called as frequently as other endpoints

### Optimization 16: Optimize getUserItems Query
- **Implementation**: Select only needed columns instead of `SELECT *` in getUserItems
- **Rationale**: `description` TEXT field is not needed in the response but was being retrieved
- **Changes**: `webapp/go/main.go`
  - Changed query to select only: id, seller_id, status, name, price, image_name, category_id, created_at
- **How to discover**: Previous Optimization 11 applied column selection to getNewItems/getNewCategoryItems. Grep search for `SELECT \* FROM.*items.*seller_id` found getUserItems still using SELECT * pattern

#### Results After Optimization 16
- **Score: ~5150-6150** (high variance due to timeouts)
- Raw score improved but timeout penalties cause variance
- Final check failures occur when buy requests timeout under high load

### Optimization 17: Optimize Nginx Configuration
- **Implementation**: Comprehensive nginx optimization
- **Rationale**: Nginx was proxying all requests including static files, adding unnecessary overhead
- **Changes**: `webapp/etc/nginx/conf.d/default.conf`
  - Added upstream block with keepalive connections (32 connections)
  - Enabled gzip compression for text content types
  - Serve static files (css, js, img, upload) directly from nginx
  - Use HTTP/1.1 with keepalive for proxy connections
- **How to discover**: Benchmark showed timeouts across many different endpoints (login, sell, items, new_items, transactions) simultaneously, suggesting infrastructure-level bottleneck rather than specific endpoint. Checked nginx config and found minimal configuration with no static file serving or connection optimization

#### Results After Optimization 17
- **Score: 6550-7460** (significant improvement!)
- **No final check failures** - stability improved
- **Cumulative: 1810 → 7460 (+312%)**
- Static file serving offloaded from Go app to nginx
- Reduced connection overhead with keepalive

### Optimization Attempt 18 (FAILED): Parallel APIShipmentStatus in getTransactions
- **Attempted Implementation**: Parallelize `APIShipmentStatus` API calls in `getTransactions` using goroutines
- **Rationale**: Mackerel HTTP Server Stats showed GET /users/transactions.json with P95 of 986ms (257 requests). Sequential API calls for non-done shippings were identified as a potential bottleneck.
- **Changes**:
  - Collected all shippings that need API calls (status != ShippingsStatusDone)
  - Made all API calls in parallel using goroutines and channels
  - Stored results in a map for later use in the main loop
- **How to discover**: Mackerel HTTP Server Stats showed GET /users/transactions.json as one of the slowest endpoints with P95 986ms

#### Results After Optimization Attempt 18
- **Score: 2950** (raw: 5950, penalty: 3000)
- **6 final check failures**: "購入されたはずなのに記録されていません" (purchases should have been recorded but weren't)
- **Root cause analysis**: Parallel API calls likely caused resource contention or race conditions, leading to transaction failures
- **Action**: Reverted the change

### Optimization 18: Database Connection Pool Settings
- **Implementation**: Add connection pool settings to optimize database connection handling
- **Rationale**: No explicit connection pool settings were configured; system was CPU-bound with loadavg ~3.0
- **Changes**: `webapp/go/main.go`
  - Added `SetMaxOpenConns(50)` - limit maximum open connections
  - Added `SetMaxIdleConns(25)` - keep idle connections for reuse
  - Added `SetConnMaxLifetime(5 * time.Minute)` - prevent stale connections
- **How to discover**: Grep search for connection pool settings found none configured. Mackerel Host Metrics showed CPU usage at 94% during benchmark.

#### Results After Optimization 18
- **Score: 6550** (within variance range of 6550-7560)
- Impact: Neutral to slight improvement (helps with connection reuse under load)
- Stability: No final check failures

### Optimization 19: Use BETWEEN Instead of IN for Category Queries
- **Implementation**: Replace `category_id IN (...)` with `category_id >= ? AND category_id <= ?` for child category queries
- **Rationale**: Child category IDs are consecutive (e.g., parent 1 has children 2,3,4,5,6). Using BETWEEN allows MySQL to perform a single efficient range scan instead of multiple index lookups for each IN value.
- **Changes**: `webapp/go/main.go`
  - Added `getChildCategoryIDRange()` function that returns min/max category IDs for a parent
  - Modified `getNewCategoryItems()` to use BETWEEN clause instead of IN clause
  - Query changed from `category_id IN (2,3,4,5,6)` to `category_id >= 2 AND category_id <= 6`
- **How to discover**: Mackerel DB Query Stats showed `SELECT ... FROM items WHERE status IN (?,?) AND category_id IN (...)` queries with P95 500-600ms. Verified category data showed consecutive IDs for child categories.

#### Results After Optimization 19
- **Score: 6750** (+700, +12%)
- **Cumulative: 1810 → 6750 (+273%)**
- **No final check failures** (previously 2 errors)
- Query efficiency improved by using range scan instead of multiple point lookups
- Stability improved - penalty reduced from 1000 to 0

### Optimization 20: Use UNION for getNewItems to Enable Index Usage
- **Implementation**: Replace `status IN (?,?)` with UNION of two separate queries
- **Rationale**: MySQL query planner chose full table scan (44,000 rows) when using `status IN (?,?)` because merging two sorted index ranges was deemed expensive. By using UNION, each subquery can use the index efficiently and only retrieve LIMIT rows.
- **Performance measured**:
  - Original query: 191ms (full table scan + filesort)
  - UNION query: 2.4ms (index scan, 98 rows max)
  - **80x faster for first page, 22x faster for pagination**
- **Changes**: `webapp/go/main.go`
  - Modified `getNewItems()` to use UNION ALL with separate status queries
  - Each subquery uses index `idx_status_created_id` with backward scan
  - Final UNION result only needs to sort 98 rows (49+49) instead of 44,000
- **How to discover**: EXPLAIN showed `type: ALL` (full table scan) and `Using filesort` for original query. Testing single status showed `ref` type with `Backward index scan`, confirming IN clause caused the issue.

#### Results After Optimization 20
- **Score: 9160** (+2410, +36%)
- **Cumulative: 1810 → 9160 (+406%)**
- **No final check failures**
- Items listing endpoints significantly faster
- System can handle higher load with reduced query time

### Optimization 21: Use UNION for getNewCategoryItems
- **Implementation**: Replace `status IN (?,?)` with UNION of two separate queries in `getNewCategoryItems`
- **Rationale**: Same issue as Optimization 20 - MySQL query planner chose inefficient plan when using `status IN (?,?)`. Each subquery in UNION can use index efficiently.
- **Changes**: `webapp/go/main.go`
  - Modified `getNewCategoryItems()` to use UNION ALL with separate status queries
  - Each subquery includes `category_id >= ? AND category_id <= ?` condition
- **How to discover**: Mackerel DB Query Stats showed `SELECT ... FROM items WHERE status IN (?,?) AND category_id >= ?...` with P95 196ms (1095 executions). This was the slowest query.

#### Results After Optimization 21
- **Score: 13060** (raw: 15060, penalty: 2000)
- **Improvement: 8560 → 13060 (+4500, +53%)**
- **Cumulative: 1810 → 13060 (+621%)**
- Category items queries significantly faster
- Some errors occurred under high load:
  - "/users/transactions.json の商品数が正しくありません" (3 errors)
  - "購入されたはずなのに記録されていません" (1 error - timeout related)

