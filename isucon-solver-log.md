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

#### Results After Optimization 2
- **Score: 2210 (+200, +10%)**
- **Cumulative: 1810 → 2210 (+22%)**
- User queries eliminated from top 20 (was 43,687 executions, 73,320ms)

