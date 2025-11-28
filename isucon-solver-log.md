# ISUCON Solver Log

## 2025-11-27

### Session Start
- Starting ISUCON optimization session
- Initial state: Docker containers not running

### Baseline Benchmark
- **Score: 1810**
- Multiple timeout errors observed on `/users/transactions.json`, `/new_items.json`, `/login`

### Bottleneck Analysis (Mackerel)

#### HTTP Server Stats (Top 5 slowest endpoints)
| Endpoint | Avg (ms) | P95 (ms) | Requests |
|---|---|---|---|
| GET /users/transactions.json | 3188 | 5668 | 124 |
| POST /initialize | 4238 | 4238 | 1 |
| POST /buy | 1593 | 1803 | 25 |
| GET /new_items.json | 994 | 1594 | 58 |
| GET /new_items/{root_category_id}.json | 719 | 1217 | 234 |

#### DB Query Stats (Critical N+1 Problems)
| Query | Executions | Total (ms) | Issue |
|---|---|---|---|
| SELECT * FROM categories WHERE id = ? | 105,857 | 116,718 | **N+1 problem** |
| SELECT * FROM users WHERE id = ? | 49,397 | 55,424 | **N+1 problem** |

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
- **Score: ~4750-5050 (average ~4850, +300, +7%)**
- **Cumulative: 1810 → ~4850 (+168%)**
- Score variance observed due to timeout penalties
- POST /buy latency reduced by eliminating sequential API wait times

