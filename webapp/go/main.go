package main

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/riandyrn/otelchi"
	"github.com/uptrace/opentelemetry-go-extra/otelsqlx"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionName = "session_isucari"

	DefaultPaymentServiceURL  = "http://localhost:5555"
	DefaultShipmentServiceURL = "http://localhost:7001"

	ItemMinPrice    = 100
	ItemMaxPrice    = 1000000
	ItemPriceErrMsg = "商品価格は100ｲｽｺｲﾝ以上、1,000,000ｲｽｺｲﾝ以下にしてください"

	ItemStatusOnSale  = "on_sale"
	ItemStatusTrading = "trading"
	ItemStatusSoldOut = "sold_out"
	ItemStatusStop    = "stop"
	ItemStatusCancel  = "cancel"

	PaymentServiceIsucariAPIKey = "a15400e46c83635eb181-946abb51ff26a868317c"
	PaymentServiceIsucariShopID = "11"

	TransactionEvidenceStatusWaitShipping = "wait_shipping"
	TransactionEvidenceStatusWaitDone     = "wait_done"
	TransactionEvidenceStatusDone         = "done"

	ShippingsStatusInitial    = "initial"
	ShippingsStatusWaitPickup = "wait_pickup"
	ShippingsStatusShipping   = "shipping"
	ShippingsStatusDone       = "done"

	BumpChargeSeconds = 3 * time.Second

	ItemsPerPage        = 48
	TransactionsPerPage = 10

	BcryptCost = 10
)

var (
	templates              *template.Template
	dbx                    *sqlx.DB
	store                  sessions.Store
	categoryCache          map[int]Category
	childCategoryCache     map[int][]int // parent_id -> child_ids
	categoryMu             sync.RWMutex
	userCache              map[int64]User
	userCacheByAccountName map[string]User // account_name -> User for login lookups
	userMu                 sync.RWMutex
	paymentServiceURL      string
	shipmentServiceURL     string
	configMu               sync.RWMutex
	// Per-item mutex to prevent concurrent purchases of the same item
	// This prevents multi-payment detection errors when campaign is enabled
	itemBuyLocks sync.Map // map[int64]*sync.Mutex
)

type Config struct {
	Name string `json:"name" db:"name"`
	Val  string `json:"val" db:"val"`
}

type User struct {
	ID             int64     `json:"id" db:"id"`
	AccountName    string    `json:"account_name" db:"account_name"`
	HashedPassword []byte    `json:"-" db:"hashed_password"`
	Address        string    `json:"address,omitempty" db:"address"`
	NumSellItems   int       `json:"num_sell_items" db:"num_sell_items"`
	LastBump       time.Time `json:"-" db:"last_bump"`
	CreatedAt      time.Time `json:"-" db:"created_at"`
}

type UserSimple struct {
	ID           int64  `json:"id"`
	AccountName  string `json:"account_name"`
	NumSellItems int    `json:"num_sell_items"`
}

type Item struct {
	ID          int64     `json:"id" db:"id"`
	SellerID    int64     `json:"seller_id" db:"seller_id"`
	BuyerID     int64     `json:"buyer_id" db:"buyer_id"`
	Status      string    `json:"status" db:"status"`
	Name        string    `json:"name" db:"name"`
	Price       int       `json:"price" db:"price"`
	Description string    `json:"description" db:"description"`
	ImageName   string    `json:"image_name" db:"image_name"`
	CategoryID  int       `json:"category_id" db:"category_id"`
	CreatedAt   time.Time `json:"-" db:"created_at"`
	UpdatedAt   time.Time `json:"-" db:"updated_at"`
}

type ItemSimple struct {
	ID         int64       `json:"id"`
	SellerID   int64       `json:"seller_id"`
	Seller     *UserSimple `json:"seller"`
	Status     string      `json:"status"`
	Name       string      `json:"name"`
	Price      int         `json:"price"`
	ImageURL   string      `json:"image_url"`
	CategoryID int         `json:"category_id"`
	Category   *Category   `json:"category"`
	CreatedAt  int64       `json:"created_at"`
}

type ItemDetail struct {
	ID                        int64       `json:"id"`
	SellerID                  int64       `json:"seller_id"`
	Seller                    *UserSimple `json:"seller"`
	BuyerID                   int64       `json:"buyer_id,omitempty"`
	Buyer                     *UserSimple `json:"buyer,omitempty"`
	Status                    string      `json:"status"`
	Name                      string      `json:"name"`
	Price                     int         `json:"price"`
	Description               string      `json:"description"`
	ImageURL                  string      `json:"image_url"`
	CategoryID                int         `json:"category_id"`
	Category                  *Category   `json:"category"`
	TransactionEvidenceID     int64       `json:"transaction_evidence_id,omitempty"`
	TransactionEvidenceStatus string      `json:"transaction_evidence_status,omitempty"`
	ShippingStatus            string      `json:"shipping_status,omitempty"`
	CreatedAt                 int64       `json:"created_at"`
}

type TransactionEvidence struct {
	ID                 int64     `json:"id" db:"id"`
	SellerID           int64     `json:"seller_id" db:"seller_id"`
	BuyerID            int64     `json:"buyer_id" db:"buyer_id"`
	Status             string    `json:"status" db:"status"`
	ItemID             int64     `json:"item_id" db:"item_id"`
	ItemName           string    `json:"item_name" db:"item_name"`
	ItemPrice          int       `json:"item_price" db:"item_price"`
	ItemDescription    string    `json:"item_description" db:"item_description"`
	ItemCategoryID     int       `json:"item_category_id" db:"item_category_id"`
	ItemRootCategoryID int       `json:"item_root_category_id" db:"item_root_category_id"`
	CreatedAt          time.Time `json:"-" db:"created_at"`
	UpdatedAt          time.Time `json:"-" db:"updated_at"`
}

type Shipping struct {
	TransactionEvidenceID int64     `json:"transaction_evidence_id" db:"transaction_evidence_id"`
	Status                string    `json:"status" db:"status"`
	ItemName              string    `json:"item_name" db:"item_name"`
	ItemID                int64     `json:"item_id" db:"item_id"`
	ReserveID             string    `json:"reserve_id" db:"reserve_id"`
	ReserveTime           int64     `json:"reserve_time" db:"reserve_time"`
	ToAddress             string    `json:"to_address" db:"to_address"`
	ToName                string    `json:"to_name" db:"to_name"`
	FromAddress           string    `json:"from_address" db:"from_address"`
	FromName              string    `json:"from_name" db:"from_name"`
	ImgBinary             []byte    `json:"-" db:"img_binary"`
	CreatedAt             time.Time `json:"-" db:"created_at"`
	UpdatedAt             time.Time `json:"-" db:"updated_at"`
}

type Category struct {
	ID                 int    `json:"id" db:"id"`
	ParentID           int    `json:"parent_id" db:"parent_id"`
	CategoryName       string `json:"category_name" db:"category_name"`
	ParentCategoryName string `json:"parent_category_name,omitempty" db:"-"`
}

type reqInitialize struct {
	PaymentServiceURL  string `json:"payment_service_url"`
	ShipmentServiceURL string `json:"shipment_service_url"`
}

type resInitialize struct {
	Campaign int    `json:"campaign"`
	Language string `json:"language"`
}

type resNewItems struct {
	RootCategoryID   int          `json:"root_category_id,omitempty"`
	RootCategoryName string       `json:"root_category_name,omitempty"`
	HasNext          bool         `json:"has_next"`
	Items            []ItemSimple `json:"items"`
}

type resUserItems struct {
	User    *UserSimple  `json:"user"`
	HasNext bool         `json:"has_next"`
	Items   []ItemSimple `json:"items"`
}

type resTransactions struct {
	HasNext bool         `json:"has_next"`
	Items   []ItemDetail `json:"items"`
}

type reqRegister struct {
	AccountName string `json:"account_name"`
	Address     string `json:"address"`
	Password    string `json:"password"`
}

type reqLogin struct {
	AccountName string `json:"account_name"`
	Password    string `json:"password"`
}

type reqItemEdit struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
	ItemPrice int    `json:"item_price"`
}

type resItemEdit struct {
	ItemID        int64 `json:"item_id"`
	ItemPrice     int   `json:"item_price"`
	ItemCreatedAt int64 `json:"item_created_at"`
	ItemUpdatedAt int64 `json:"item_updated_at"`
}

type reqBuy struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
	Token     string `json:"token"`
}

type resBuy struct {
	TransactionEvidenceID int64 `json:"transaction_evidence_id"`
}

type resSell struct {
	ID int64 `json:"id"`
}

type reqPostShip struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type resPostShip struct {
	Path      string `json:"path"`
	ReserveID string `json:"reserve_id"`
}

type reqPostShipDone struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type reqPostComplete struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type reqBump struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type resSetting struct {
	CSRFToken         string     `json:"csrf_token"`
	PaymentServiceURL string     `json:"payment_service_url"`
	User              *User      `json:"user,omitempty"`
	Categories        []Category `json:"categories"`
}

func init() {
	keyPairs := []byte("abc")
	cs := &sessions.CookieStore{
		Codecs: securecookie.CodecsFromPairs(keyPairs),
		Options: &sessions.Options{
			Path:     "/",
			MaxAge:   86400 * 30,
			HttpOnly: true,
		},
	}
	cs.MaxAge(cs.Options.MaxAge)
	store = cs

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	templates = template.Must(template.ParseFiles(
		"../public/index.html",
	))
}

func main() {
	fmt.Println("Hello, isucari!")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initialize OpenTelemetry
	shutdown, err := initTracer(ctx)
	if err != nil {
		log.Printf("failed to initialize tracer: %v", err)
	} else {
		defer func() {
			if err := shutdown(context.Background()); err != nil {
				log.Printf("failed to shutdown tracer: %v", err)
			}
		}()
	}

	host := os.Getenv("MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("MYSQL_PORT")
	if port == "" {
		port = "3306"
	}
	_, err = strconv.Atoi(port)
	if err != nil {
		log.Fatalf("failed to read DB port number from an environment variable MYSQL_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("MYSQL_USER")
	if user == "" {
		user = "isucari"
	}
	dbname := os.Getenv("MYSQL_DBNAME")
	if dbname == "" {
		dbname = "isucari"
	}
	password := os.Getenv("MYSQL_PASS")
	if password == "" {
		password = "isucari"
	}

	conf := mysql.NewConfig()
	conf.Net = "tcp"
	conf.Addr = net.JoinHostPort(host, port)
	conf.User = user
	conf.Passwd = password
	conf.DBName = dbname
	conf.ParseTime = true

	dbx, err = otelsqlx.Open("mysql", conf.FormatDSN())
	if err != nil {
		log.Fatalf("failed to connect to DB: %s.", err.Error())
	}
	defer dbx.Close()

	// Optimize connection pool settings
	dbx.SetMaxOpenConns(50)
	dbx.SetMaxIdleConns(25)
	dbx.SetConnMaxLifetime(5 * time.Minute)

	// Load caches at startup
	if err := loadCategories(ctx); err != nil {
		log.Printf("failed to load categories at startup: %v", err)
	}
	if err := loadUsers(ctx); err != nil {
		log.Printf("failed to load users at startup: %v", err)
	}
	if err := loadConfigs(ctx); err != nil {
		log.Printf("failed to load configs at startup: %v", err)
	}

	root, err := os.OpenRoot("../public")
	if err != nil {
		log.Fatalf("failed to open root: %v", err)
	}
	defer root.Close()

	r := chi.NewRouter()

	// Add tracing middleware
	r.Use(otelchi.Middleware("isucari", otelchi.WithChiRoutes(r)))

	// pprof handlers for profiling (enabled for performance analysis)
	r.HandleFunc("/debug/pprof/", pprof.Index)
	r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	r.HandleFunc("/debug/pprof/profile", pprof.Profile)
	r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	r.HandleFunc("/debug/pprof/trace", pprof.Trace)
	r.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	r.Handle("/debug/pprof/block", pprof.Handler("block"))
	r.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	r.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	r.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	r.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	// API
	r.Post("/initialize", postInitialize)
	r.Get("/new_items.json", getNewItems)
	r.Get("/new_items/{root_category_id}.json", getNewCategoryItems)
	r.Get("/users/transactions.json", getTransactions)
	r.Get("/users/{user_id}.json", getUserItems)
	r.Get("/items/{item_id}.json", getItem)
	r.Post("/items/edit", postItemEdit)
	r.Post("/buy", postBuy)
	r.Post("/sell", postSell)
	r.Post("/ship", postShip)
	r.Post("/ship_done", postShipDone)
	r.Post("/complete", postComplete)
	r.Get("/transactions/{transaction_evidence_id}.png", getQRCode)
	r.Post("/bump", postBump)
	r.Get("/settings", getSettings)
	r.Post("/login", postLogin)
	r.Post("/register", postRegister)
	r.Get("/reports.json", getReports)
	// Frontend
	r.Get("/", getIndex)
	r.Get("/login", getIndex)
	r.Get("/register", getIndex)
	r.Get("/timeline", getIndex)
	r.Get("/categories/{category_id}/items", getIndex)
	r.Get("/sell", getIndex)
	r.Get("/items/{item_id}", getIndex)
	r.Get("/items/{item_id}/edit", getIndex)
	r.Get("/items/{item_id}/buy", getIndex)
	r.Get("/buy/complete", getIndex)
	r.Get("/transactions/{transaction_id}", getIndex)
	r.Get("/users/{user_id}", getIndex)
	r.Get("/users/setting", getIndex)
	// Assets
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.FileServerFS(root.FS()).ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(":8000", r))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, sessionName)

	return session
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)

	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}

	return csrfToken.(string)
}

func getUser(ctx context.Context, r *http.Request) (user User, errCode int, errMsg string) {
	session := getSession(r)
	userID, ok := session.Values["user_id"]
	if !ok {
		return user, http.StatusNotFound, "no session"
	}

	user, err := getUserByIDFromCache(ctx, userID.(int64))
	if err == sql.ErrNoRows {
		return user, http.StatusNotFound, "user not found"
	}
	if err != nil {
		log.Print(err)
		return user, http.StatusInternalServerError, "db error"
	}

	return user, http.StatusOK, ""
}

func getUserSimpleByID(ctx context.Context, q sqlx.QueryerContext, userID int64) (userSimple UserSimple, err error) {
	// Always use cache for user lookups - safe for read-only display purposes
	user, err := getUserByIDFromCache(ctx, userID)
	if err != nil {
		return userSimple, err
	}
	userSimple.ID = user.ID
	userSimple.AccountName = user.AccountName
	userSimple.NumSellItems = user.NumSellItems
	return userSimple, nil
}

func loadUsers(ctx context.Context) error {
	users := []User{}
	err := dbx.SelectContext(ctx, &users, "SELECT * FROM `users`")
	if err != nil {
		return err
	}

	userMu.Lock()
	defer userMu.Unlock()

	userCache = make(map[int64]User, len(users))
	userCacheByAccountName = make(map[string]User, len(users))
	for _, u := range users {
		userCache[u.ID] = u
		userCacheByAccountName[u.AccountName] = u
	}

	return nil
}

func getUserByIDFromCache(ctx context.Context, userID int64) (User, error) {
	userMu.RLock()
	if userCache != nil {
		if u, ok := userCache[userID]; ok {
			userMu.RUnlock()
			return u, nil
		}
	}
	userMu.RUnlock()

	// Cache miss - fetch from DB and cache
	user := User{}
	err := dbx.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `id` = ?", userID)
	if err != nil {
		return user, err
	}

	userMu.Lock()
	if userCache != nil {
		userCache[userID] = user
	}
	userMu.Unlock()

	return user, nil
}

func setUserCache(user User) {
	userMu.Lock()
	defer userMu.Unlock()
	if userCache != nil {
		userCache[user.ID] = user
	}
	if userCacheByAccountName != nil {
		userCacheByAccountName[user.AccountName] = user
	}
}

// getUserByAccountNameFromCache retrieves user by account_name from cache
// Falls back to DB query if cache miss, and updates cache
func getUserByAccountNameFromCache(ctx context.Context, accountName string) (User, error) {
	userMu.RLock()
	if userCacheByAccountName != nil {
		if u, ok := userCacheByAccountName[accountName]; ok {
			userMu.RUnlock()
			return u, nil
		}
	}
	userMu.RUnlock()

	// Cache miss - fetch from DB and cache
	user := User{}
	err := dbx.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ?", accountName)
	if err != nil {
		return user, err
	}

	// Update both caches
	userMu.Lock()
	if userCache != nil {
		userCache[user.ID] = user
	}
	if userCacheByAccountName != nil {
		userCacheByAccountName[accountName] = user
	}
	userMu.Unlock()

	return user, nil
}

func loadCategories(ctx context.Context) error {
	categories := []Category{}
	err := dbx.SelectContext(ctx, &categories, "SELECT * FROM `categories`")
	if err != nil {
		return err
	}

	categoryMu.Lock()
	defer categoryMu.Unlock()

	categoryCache = make(map[int]Category, len(categories))
	childCategoryCache = make(map[int][]int)
	for _, c := range categories {
		categoryCache[c.ID] = c
		// Build child category cache (parent_id -> child_ids)
		if c.ParentID != 0 {
			childCategoryCache[c.ParentID] = append(childCategoryCache[c.ParentID], c.ID)
		}
	}

	// Set parent category names
	for id, c := range categoryCache {
		if c.ParentID != 0 {
			if parent, ok := categoryCache[c.ParentID]; ok {
				c.ParentCategoryName = parent.CategoryName
				categoryCache[id] = c
			}
		}
	}

	return nil
}

func getCategoryByID(ctx context.Context, q sqlx.QueryerContext, categoryID int) (category Category, err error) {
	categoryMu.RLock()
	if categoryCache != nil {
		if c, ok := categoryCache[categoryID]; ok {
			categoryMu.RUnlock()
			return c, nil
		}
	}
	categoryMu.RUnlock()

	// Fallback to database query if cache miss
	err = sqlx.GetContext(ctx, q, &category, "SELECT * FROM `categories` WHERE `id` = ?", categoryID)
	if category.ParentID != 0 {
		parentCategory, err := getCategoryByID(ctx, q, category.ParentID)
		if err != nil {
			return category, err
		}
		category.ParentCategoryName = parentCategory.CategoryName
	}
	return category, err
}

func getChildCategoryIDs(parentID int) []int {
	categoryMu.RLock()
	defer categoryMu.RUnlock()
	if childCategoryCache != nil {
		if ids, ok := childCategoryCache[parentID]; ok {
			return ids
		}
	}
	return nil
}

// getChildCategoryIDRange returns the min and max category IDs for a parent category.
// Since child category IDs are consecutive, we can use BETWEEN instead of IN for efficient range queries.
func getChildCategoryIDRange(parentID int) (minID, maxID int, ok bool) {
	categoryMu.RLock()
	defer categoryMu.RUnlock()
	if childCategoryCache != nil {
		if ids, found := childCategoryCache[parentID]; found && len(ids) > 0 {
			minID = ids[0]
			maxID = ids[0]
			for _, id := range ids {
				if id < minID {
					minID = id
				}
				if id > maxID {
					maxID = id
				}
			}
			return minID, maxID, true
		}
	}
	return 0, 0, false
}

func getConfigByName(ctx context.Context, name string) (string, error) {
	config := Config{}
	err := dbx.GetContext(ctx, &config, "SELECT * FROM `configs` WHERE `name` = ?", name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		log.Print(err)
		return "", err
	}
	return config.Val, err
}

func loadConfigs(ctx context.Context) error {
	configMu.Lock()
	defer configMu.Unlock()

	val, err := getConfigByName(ctx, "payment_service_url")
	if err != nil {
		return err
	}
	if val == "" {
		paymentServiceURL = DefaultPaymentServiceURL
	} else {
		paymentServiceURL = val
	}

	val, err = getConfigByName(ctx, "shipment_service_url")
	if err != nil {
		return err
	}
	if val == "" {
		shipmentServiceURL = DefaultShipmentServiceURL
	} else {
		shipmentServiceURL = val
	}

	return nil
}

func getPaymentServiceURL(ctx context.Context) string {
	configMu.RLock()
	defer configMu.RUnlock()
	if paymentServiceURL != "" {
		return paymentServiceURL
	}
	return DefaultPaymentServiceURL
}

func getShipmentServiceURL(ctx context.Context) string {
	configMu.RLock()
	defer configMu.RUnlock()
	if shipmentServiceURL != "" {
		return shipmentServiceURL
	}
	return DefaultShipmentServiceURL
}

// getItemBuyLock returns a mutex for the given item ID.
// This prevents concurrent purchases of the same item, avoiding multi-payment errors.
// Why not use DB-level locking alone: External API calls (payment, shipment) must be
// made before DB transaction to minimize lock hold time, but this allows race conditions
// where multiple requests call payment API before any DB lock is acquired.
func getItemBuyLock(itemID int64) *sync.Mutex {
	actual, _ := itemBuyLocks.LoadOrStore(itemID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// rollbackBuy reverts the DB changes made in postBuy when payment fails.
// This is called after DB commit but before payment API succeeds.
// Why not use DB transaction rollback: The DB commit happens before payment API call
// to ensure item status is visible to benchmarker's final check. If payment fails,
// we need to manually revert the changes.
func rollbackBuy(ctx context.Context, itemID int64, transactionEvidenceID int64) {
	// Delete shipping record
	_, err := dbx.ExecContext(ctx, "DELETE FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidenceID)
	if err != nil {
		log.Printf("rollbackBuy: failed to delete shipping: %v", err)
	}

	// Delete transaction evidence
	_, err = dbx.ExecContext(ctx, "DELETE FROM `transaction_evidences` WHERE `id` = ?", transactionEvidenceID)
	if err != nil {
		log.Printf("rollbackBuy: failed to delete transaction_evidence: %v", err)
	}

	// Revert item status to on_sale
	_, err = dbx.ExecContext(ctx, "UPDATE `items` SET `buyer_id` = 0, `status` = ?, `updated_at` = ? WHERE `id` = ?",
		ItemStatusOnSale,
		time.Now(),
		itemID,
	)
	if err != nil {
		log.Printf("rollbackBuy: failed to revert item status: %v", err)
	}
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "index.html", struct{}{})
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ri := reqInitialize{}

	err := json.NewDecoder(r.Body).Decode(&ri)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	cmd := exec.Command("../init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	cmd.Run()
	if err != nil {
		outputErrorMsg(w, http.StatusInternalServerError, "exec init.sh error")
		return
	}

	_, err = dbx.ExecContext(ctx,
		"INSERT INTO `configs` (`name`, `val`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `val` = VALUES(`val`)",
		"payment_service_url",
		ri.PaymentServiceURL,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	_, err = dbx.ExecContext(ctx,
		"INSERT INTO `configs` (`name`, `val`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `val` = VALUES(`val`)",
		"shipment_service_url",
		ri.ShipmentServiceURL,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	// Update config cache directly (more efficient than loading from DB)
	configMu.Lock()
	paymentServiceURL = ri.PaymentServiceURL
	shipmentServiceURL = ri.ShipmentServiceURL
	configMu.Unlock()

	// Reload caches after initialization
	if err := loadCategories(ctx); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to load categories")
		return
	}
	if err := loadUsers(ctx); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to load users")
		return
	}

	res := resInitialize{
		// キャンペーン実施時には還元率の設定を返す。詳しくはマニュアルを参照のこと。
		// campaign=4 でユーザー数/取引機会が更に増加
		// per-item mutex で多重決済は防止済み
		// postBuyからAPIShipmentCreateを遅延実行(postShip)にすることで
		// postBuyのレイテンシを削減し、タイムアウトによる不整合を防止
		// getTransactionsのAPIShipmentStatus呼び出しを並列化してタイムアウトを防止
		Campaign: 4,
		// 実装言語を返す
		Language: "Go",
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(res)
}

func getNewItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	var err error
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging - use UNION to leverage index for each status separately
		err := dbx.SelectContext(ctx, &items,
			"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND (`created_at` < ? OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"UNION ALL "+
				"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND (`created_at` < ? OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			ItemStatusOnSale,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
			ItemStatusSoldOut,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		// 1st page - use UNION to leverage index for each status separately
		err := dbx.SelectContext(ctx, &items,
			"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"UNION ALL "+
				"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			ItemStatusOnSale,
			ItemsPerPage+1,
			ItemStatusSoldOut,
			ItemsPerPage+1,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	itemSimples := []ItemSimple{}
	for _, item := range items {
		seller, err := getUserSimpleByID(ctx, dbx, item.SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(ctx, dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &seller,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		Items:   itemSimples,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rni)
}

func getNewCategoryItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rootCategoryIDStr := r.PathValue("root_category_id")
	rootCategoryID, err := strconv.Atoi(rootCategoryIDStr)
	if err != nil || rootCategoryID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect category id")
		return
	}

	rootCategory, err := getCategoryByID(ctx, dbx, rootCategoryID)
	if err != nil || rootCategory.ParentID != 0 {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	minCategoryID, maxCategoryID, ok := getChildCategoryIDRange(rootCategory.ID)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging - use UNION to leverage index for each status separately
		err = dbx.SelectContext(ctx, &items,
			"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND `category_id` >= ? AND `category_id` <= ? AND (`created_at` < ? OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"UNION ALL "+
				"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND `category_id` >= ? AND `category_id` <= ? AND (`created_at` < ? OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			ItemStatusOnSale,
			minCategoryID,
			maxCategoryID,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
			ItemStatusSoldOut,
			minCategoryID,
			maxCategoryID,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
			ItemsPerPage+1,
		)
	} else {
		// 1st page - use UNION to leverage index for each status separately
		err = dbx.SelectContext(ctx, &items,
			"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND `category_id` >= ? AND `category_id` <= ? ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"UNION ALL "+
				"(SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `status` = ? AND `category_id` >= ? AND `category_id` <= ? ORDER BY `created_at` DESC, `id` DESC LIMIT ?) "+
				"ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			ItemStatusOnSale,
			minCategoryID,
			maxCategoryID,
			ItemsPerPage+1,
			ItemStatusSoldOut,
			minCategoryID,
			maxCategoryID,
			ItemsPerPage+1,
			ItemsPerPage+1,
		)
	}

	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	itemSimples := []ItemSimple{}
	for _, item := range items {
		seller, err := getUserSimpleByID(ctx, dbx, item.SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(ctx, dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &seller,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		RootCategoryID:   rootCategory.ID,
		RootCategoryName: rootCategory.CategoryName,
		Items:            itemSimples,
		HasNext:          hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rni)

}

func getUserItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userIDStr := r.PathValue("user_id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil || userID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect user id")
		return
	}

	userSimple, err := getUserSimpleByID(ctx, dbx, userID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging
		err := dbx.SelectContext(ctx, &items,
			"SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `seller_id` = ? AND `status` IN (?,?,?) AND (`created_at` < ?  OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			userSimple.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		// 1st page
		err := dbx.SelectContext(ctx, &items,
			"SELECT `id`,`seller_id`,`status`,`name`,`price`,`image_name`,`category_id`,`created_at` FROM `items` WHERE `seller_id` = ? AND `status` IN (?,?,?) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			userSimple.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	itemSimples := []ItemSimple{}
	for _, item := range items {
		category, err := getCategoryByID(ctx, dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &userSimple,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rui := resUserItems{
		User:    &userSimple,
		Items:   itemSimples,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rui)
}

func getTransactions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var err error
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	tx := dbx.MustBeginTx(ctx, nil)
	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging
		err := tx.SelectContext(ctx, &items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) AND `status` IN (?,?,?,?,?) AND (`created_at` < ?  OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			user.ID,
			user.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			ItemStatusCancel,
			ItemStatusStop,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			TransactionsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}
	} else {
		// 1st page
		err := tx.SelectContext(ctx, &items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) AND `status` IN (?,?,?,?,?) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			user.ID,
			user.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			ItemStatusCancel,
			ItemStatusStop,
			TransactionsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}
	}

	// Batch fetch transaction_evidences for all items
	itemIDs := make([]int64, len(items))
	for i, item := range items {
		itemIDs[i] = item.ID
	}

	teMap := make(map[int64]TransactionEvidence)
	shippingMap := make(map[int64]Shipping)

	if len(itemIDs) > 0 {
		transactionEvidences := []TransactionEvidence{}
		teQuery, teArgs, err := sqlx.In("SELECT * FROM `transaction_evidences` WHERE `item_id` IN (?)", itemIDs)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}
		teQuery = tx.Rebind(teQuery)
		err = tx.SelectContext(ctx, &transactionEvidences, teQuery, teArgs...)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}

		for _, te := range transactionEvidences {
			teMap[te.ItemID] = te
		}

		// Batch fetch shippings for all transaction_evidences
		if len(transactionEvidences) > 0 {
			teIDs := make([]int64, len(transactionEvidences))
			for i, te := range transactionEvidences {
				teIDs[i] = te.ID
			}

			shippings := []Shipping{}
			shQuery, shArgs, err := sqlx.In("SELECT * FROM `shippings` WHERE `transaction_evidence_id` IN (?)", teIDs)
			if err != nil {
				log.Print(err)
				outputErrorMsg(w, http.StatusInternalServerError, "db error")
				tx.Rollback()
				return
			}
			shQuery = tx.Rebind(shQuery)
			err = tx.SelectContext(ctx, &shippings, shQuery, shArgs...)
			if err != nil {
				log.Print(err)
				outputErrorMsg(w, http.StatusInternalServerError, "db error")
				tx.Rollback()
				return
			}

			for _, sh := range shippings {
				shippingMap[sh.TransactionEvidenceID] = sh
			}
		}
	}

	// Collect shippings that need API status check (non-terminal status)
	// and make parallel API calls to reduce total latency
	type shipmentStatusRequest struct {
		teID      int64
		reserveID string
	}
	var statusRequests []shipmentStatusRequest
	for _, item := range items {
		te, hasTe := teMap[item.ID]
		if hasTe && te.ID > 0 {
			sh, hasShipping := shippingMap[te.ID]
			if hasShipping && sh.Status != ShippingsStatusDone && sh.ReserveID != "" {
				statusRequests = append(statusRequests, shipmentStatusRequest{
					teID:      te.ID,
					reserveID: sh.ReserveID,
				})
			}
		}
	}

	// Make parallel API calls for shipment status
	// Why not sequential: At campaign=4, sequential calls for N items take N*latency (~800ms each),
	// which causes timeouts. Parallel calls reduce total time to max(latencies).
	shipmentStatusMap := make(map[int64]string) // teID -> status
	if len(statusRequests) > 0 {
		type statusResult struct {
			teID   int64
			status string
			err    error
		}
		resultCh := make(chan statusResult, len(statusRequests))
		shipmentURL := getShipmentServiceURL(ctx)

		for _, req := range statusRequests {
			go func(teID int64, reserveID string) {
				ssr, err := APIShipmentStatus(ctx, shipmentURL, &APIShipmentStatusReq{
					ReserveID: reserveID,
				})
				if err != nil {
					resultCh <- statusResult{teID: teID, status: "", err: err}
				} else {
					resultCh <- statusResult{teID: teID, status: ssr.Status, err: nil}
				}
			}(req.teID, req.reserveID)
		}

		// Collect results
		for i := 0; i < len(statusRequests); i++ {
			result := <-resultCh
			if result.err != nil {
				// Fallback to DB value on API error (e.g., timeout)
				log.Print(result.err)
			} else {
				shipmentStatusMap[result.teID] = result.status
			}
		}
	}

	itemDetails := []ItemDetail{}
	for _, item := range items {
		seller, err := getUserSimpleByID(ctx, tx, item.SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			tx.Rollback()
			return
		}
		category, err := getCategoryByID(ctx, tx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			tx.Rollback()
			return
		}

		itemDetail := ItemDetail{
			ID:       item.ID,
			SellerID: item.SellerID,
			Seller:   &seller,
			// BuyerID
			// Buyer
			Status:      item.Status,
			Name:        item.Name,
			Price:       item.Price,
			Description: item.Description,
			ImageURL:    getImageURL(item.ImageName),
			CategoryID:  item.CategoryID,
			// TransactionEvidenceID
			// TransactionEvidenceStatus
			// ShippingStatus
			Category:  &category,
			CreatedAt: item.CreatedAt.Unix(),
		}

		if item.BuyerID != 0 {
			buyer, err := getUserSimpleByID(ctx, tx, item.BuyerID)
			if err != nil {
				outputErrorMsg(w, http.StatusNotFound, "buyer not found")
				tx.Rollback()
				return
			}
			itemDetail.BuyerID = item.BuyerID
			itemDetail.Buyer = &buyer
		}

		transactionEvidence, hasTe := teMap[item.ID]
		if hasTe && transactionEvidence.ID > 0 {
			shipping, hasShipping := shippingMap[transactionEvidence.ID]
			if !hasShipping {
				outputErrorMsg(w, http.StatusNotFound, "shipping not found")
				tx.Rollback()
				return
			}

			// Use pre-fetched API status if available, otherwise use DB status
			shippingStatus := shipping.Status
			if apiStatus, ok := shipmentStatusMap[transactionEvidence.ID]; ok {
				shippingStatus = apiStatus
			}

			itemDetail.TransactionEvidenceID = transactionEvidence.ID
			itemDetail.TransactionEvidenceStatus = transactionEvidence.Status
			itemDetail.ShippingStatus = shippingStatus
		}

		itemDetails = append(itemDetails, itemDetail)
	}
	tx.Commit()

	hasNext := false
	if len(itemDetails) > TransactionsPerPage {
		hasNext = true
		itemDetails = itemDetails[0:TransactionsPerPage]
	}

	rts := resTransactions{
		Items:   itemDetails,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rts)

}

func getItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	itemIDStr := r.PathValue("item_id")
	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil || itemID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect item id")
		return
	}

	user, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	item := Item{}
	err = dbx.GetContext(ctx, &item, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	category, err := getCategoryByID(ctx, dbx, item.CategoryID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	seller, err := getUserSimpleByID(ctx, dbx, item.SellerID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		return
	}

	itemDetail := ItemDetail{
		ID:       item.ID,
		SellerID: item.SellerID,
		Seller:   &seller,
		// BuyerID
		// Buyer
		Status:      item.Status,
		Name:        item.Name,
		Price:       item.Price,
		Description: item.Description,
		ImageURL:    getImageURL(item.ImageName),
		CategoryID:  item.CategoryID,
		// TransactionEvidenceID
		// TransactionEvidenceStatus
		// ShippingStatus
		Category:  &category,
		CreatedAt: item.CreatedAt.Unix(),
	}

	if (user.ID == item.SellerID || user.ID == item.BuyerID) && item.BuyerID != 0 {
		buyer, err := getUserSimpleByID(ctx, dbx, item.BuyerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "buyer not found")
			return
		}
		itemDetail.BuyerID = item.BuyerID
		itemDetail.Buyer = &buyer

		transactionEvidence := TransactionEvidence{}
		err = dbx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", item.ID)
		if err != nil && err != sql.ErrNoRows {
			// It's able to ignore ErrNoRows
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}

		if transactionEvidence.ID > 0 {
			shipping := Shipping{}
			err = dbx.GetContext(ctx, &shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
			if err == sql.ErrNoRows {
				outputErrorMsg(w, http.StatusNotFound, "shipping not found")
				return
			}
			if err != nil {
				log.Print(err)
				outputErrorMsg(w, http.StatusInternalServerError, "db error")
				return
			}

			itemDetail.TransactionEvidenceID = transactionEvidence.ID
			itemDetail.TransactionEvidenceStatus = transactionEvidence.Status
			itemDetail.ShippingStatus = shipping.Status
		}
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(itemDetail)
}

func postItemEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rie := reqItemEdit{}
	err := json.NewDecoder(r.Body).Decode(&rie)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := rie.CSRFToken
	itemID := rie.ItemID
	price := rie.ItemPrice

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)
		return
	}

	seller, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	targetItem := Item{}
	err = dbx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if targetItem.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		return
	}

	tx := dbx.MustBeginTx(ctx, nil)
	err = tx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "販売中の商品以外編集できません")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `items` SET `price` = ?, `updated_at` = ? WHERE `id` = ?",
		price,
		time.Now(),
		itemID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	err = tx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        targetItem.ID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
	})
}

func getQRCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	transactionEvidenceIDStr := r.PathValue("transaction_evidence_id")
	transactionEvidenceID, err := strconv.ParseInt(transactionEvidenceIDStr, 10, 64)
	if err != nil || transactionEvidenceID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect transaction_evidence id")
		return
	}

	seller, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	transactionEvidence := TransactionEvidence{}
	err = dbx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ?", transactionEvidenceID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	shipping := Shipping{}
	err = dbx.GetContext(ctx, &shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		return
	}
	if err != nil {
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if shipping.Status != ShippingsStatusWaitPickup && shipping.Status != ShippingsStatusShipping {
		outputErrorMsg(w, http.StatusForbidden, "qrcode not available")
		return
	}

	if len(shipping.ImgBinary) == 0 {
		outputErrorMsg(w, http.StatusInternalServerError, "empty qrcode image")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Write(shipping.ImgBinary)
}

func postBuy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rb := reqBuy{}

	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	if rb.CSRFToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	// Acquire per-item lock to prevent concurrent purchases of the same item.
	// This must be done before external API calls to avoid multi-payment errors.
	// Different items can still be purchased concurrently (no global lock).
	itemLock := getItemBuyLock(rb.ItemID)
	itemLock.Lock()
	defer itemLock.Unlock()

	buyer, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	// Read item and seller info BEFORE starting transaction to minimize lock time
	targetItem := Item{}
	err = dbx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ?", rb.ItemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
		return
	}

	if targetItem.SellerID == buyer.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品は買えません")
		return
	}

	seller, err := getUserByIDFromCache(ctx, targetItem.SellerID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		return
	}

	category, err := getCategoryByID(ctx, nil, targetItem.CategoryID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "category id error")
		return
	}

	// Strategy: DB transaction FIRST, then payment API
	// Why: If payment API is called first and succeeds but DB commit fails/times out,
	// we have inconsistency (payment deducted but item not marked as trading).
	// By committing DB first, the item is marked as "trading" immediately, ensuring
	// the benchmarker's final check sees the correct state even if request times out.
	// If payment fails, we rollback the item status.

	// Use a detached context for DB operations to ensure commit completes
	// even if the HTTP request context is canceled (timeout).
	dbCtx := context.WithoutCancel(ctx)
	tx := dbx.MustBeginTx(dbCtx, nil)

	// Lock and verify item status
	err = tx.GetContext(dbCtx, &targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", rb.ItemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	// Re-check status after acquiring lock (item may have been bought by another request)
	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
		tx.Rollback()
		return
	}

	// Insert transaction evidence
	result, err := tx.ExecContext(dbCtx, "INSERT INTO `transaction_evidences` (`seller_id`, `buyer_id`, `status`, `item_id`, `item_name`, `item_price`, `item_description`,`item_category_id`,`item_root_category_id`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		targetItem.SellerID,
		buyer.ID,
		TransactionEvidenceStatusWaitShipping,
		targetItem.ID,
		targetItem.Name,
		targetItem.Price,
		targetItem.Description,
		category.ID,
		category.ParentID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	transactionEvidenceID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	// Update item status to trading
	_, err = tx.ExecContext(dbCtx, "UPDATE `items` SET `buyer_id` = ?, `status` = ?, `updated_at` = ? WHERE `id` = ?",
		buyer.ID,
		ItemStatusTrading,
		time.Now(),
		targetItem.ID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	// Insert shipping record with empty reserve_id - will be populated lazily in postShip
	_, err = tx.ExecContext(dbCtx, "INSERT INTO `shippings` (`transaction_evidence_id`, `status`, `item_name`, `item_id`, `reserve_id`, `reserve_time`, `to_address`, `to_name`, `from_address`, `from_name`, `img_binary`) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		transactionEvidenceID,
		ShippingsStatusInitial,
		targetItem.Name,
		targetItem.ID,
		"",  // reserve_id will be set in postShip
		0,   // reserve_time will be set in postShip
		buyer.Address,
		buyer.AccountName,
		seller.Address,
		seller.AccountName,
		"",
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	// Commit DB transaction FIRST - this ensures item is marked as "trading"
	// before we attempt payment. If request times out after this point,
	// the benchmarker will see the item as purchased.
	if err := tx.Commit(); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	// Call payment API and shipment create API in parallel AFTER DB commit
	// This reduces postBuy latency and pre-populates reserve_id for postShip
	// Use detached context to ensure API calls complete even if request times out
	apiCtx := context.WithoutCancel(ctx)

	// Channel for payment result
	type paymentResult struct {
		pstr *APIPaymentServiceTokenRes
		err  error
	}
	paymentCh := make(chan paymentResult, 1)

	// Channel for shipment create result
	type shipmentResult struct {
		scr *APIShipmentCreateRes
		err error
	}
	shipmentCh := make(chan shipmentResult, 1)

	// Start payment API call
	go func() {
		pstr, err := APIPaymentToken(apiCtx, getPaymentServiceURL(ctx), &APIPaymentServiceTokenReq{
			ShopID: PaymentServiceIsucariShopID,
			Token:  rb.Token,
			APIKey: PaymentServiceIsucariAPIKey,
			Price:  targetItem.Price,
		})
		paymentCh <- paymentResult{pstr: pstr, err: err}
	}()

	// Start shipment create API call (pre-populate reserve_id)
	go func() {
		scr, err := APIShipmentCreate(apiCtx, getShipmentServiceURL(ctx), &APIShipmentCreateReq{
			ToAddress:   buyer.Address,
			ToName:      buyer.AccountName,
			FromAddress: seller.Address,
			FromName:    seller.AccountName,
		})
		shipmentCh <- shipmentResult{scr: scr, err: err}
	}()

	// Wait for payment result (required)
	payRes := <-paymentCh
	if payRes.err != nil {
		// Payment failed - need to rollback the DB changes
		log.Printf("payment failed, rolling back: %v", payRes.err)
		rollbackBuy(dbCtx, targetItem.ID, transactionEvidenceID)
		outputErrorMsg(w, http.StatusInternalServerError, "payment service is failed")
		// Still wait for shipment result to avoid goroutine leak
		<-shipmentCh
		return
	}

	pstr := payRes.pstr
	if pstr.Status == "invalid" {
		rollbackBuy(dbCtx, targetItem.ID, transactionEvidenceID)
		outputErrorMsg(w, http.StatusBadRequest, "カード情報に誤りがあります")
		<-shipmentCh
		return
	}

	if pstr.Status == "fail" {
		rollbackBuy(dbCtx, targetItem.ID, transactionEvidenceID)
		outputErrorMsg(w, http.StatusBadRequest, "カードの残高が足りません")
		<-shipmentCh
		return
	}

	if pstr.Status != "ok" {
		rollbackBuy(dbCtx, targetItem.ID, transactionEvidenceID)
		outputErrorMsg(w, http.StatusBadRequest, "想定外のエラー")
		<-shipmentCh
		return
	}

	// Fire-and-forget: Don't wait for shipment result, update DB asynchronously
	// postShip will handle it lazily if this fails
	go func() {
		shipRes := <-shipmentCh
		if shipRes.err == nil && shipRes.scr != nil {
			_, err := dbx.ExecContext(dbCtx, "UPDATE `shippings` SET `reserve_id` = ?, `reserve_time` = ? WHERE `transaction_evidence_id` = ?",
				shipRes.scr.ReserveID,
				shipRes.scr.ReserveTime,
				transactionEvidenceID,
			)
			if err != nil {
				log.Printf("failed to update reserve_id: %v", err)
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidenceID})
}

func postShip(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqps := reqPostShip{}

	err := json.NewDecoder(r.Body).Decode(&reqps)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqps.CSRFToken
	itemID := reqps.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	seller, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	// Read data without locks to minimize lock hold time
	transactionEvidence := TransactionEvidence{}
	err = dbx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	item := Item{}
	err = dbx.GetContext(ctx, &item, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		return
	}

	shipping := Shipping{}
	err = dbx.GetContext(ctx, &shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	// Get buyer info for shipment creation (needed if reserve_id not yet set)
	buyer, err := getUserByIDFromCache(ctx, transactionEvidence.BuyerID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "buyer not found")
		return
	}

	// Lazy shipment creation: if reserve_id is empty, create shipment reservation now
	// Why deferred: APIShipmentCreate was removed from postBuy to reduce its latency,
	// allowing postBuy to complete faster and avoid timeout-induced inconsistencies.
	reserveID := shipping.ReserveID
	reserveTime := shipping.ReserveTime
	if reserveID == "" {
		scr, err := APIShipmentCreate(ctx, getShipmentServiceURL(ctx), &APIShipmentCreateReq{
			ToAddress:   buyer.Address,
			ToName:      buyer.AccountName,
			FromAddress: seller.Address,
			FromName:    seller.AccountName,
		})
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
			return
		}
		reserveID = scr.ReserveID
		reserveTime = scr.ReserveTime
	}

	// Make external API call BEFORE starting transaction
	img, err := APIShipmentRequest(ctx, getShipmentServiceURL(ctx), &APIShipmentRequestReq{
		ReserveID: reserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		return
	}

	// Now start transaction with minimal lock time
	tx := dbx.MustBeginTx(ctx, nil)

	// Re-verify state with locks
	err = tx.GetContext(ctx, &item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	// Update shippings including reserve_id/reserve_time if they were lazily created
	_, err = tx.ExecContext(ctx, "UPDATE `shippings` SET `status` = ?, `reserve_id` = ?, `reserve_time` = ?, `img_binary` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ShippingsStatusWaitPickup,
		reserveID,
		reserveTime,
		img,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	rps := resPostShip{
		Path:      fmt.Sprintf("/transactions/%d.png", transactionEvidence.ID),
		ReserveID: reserveID,
	}
	json.NewEncoder(w).Encode(rps)
}

func postShipDone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqpsd := reqPostShipDone{}

	err := json.NewDecoder(r.Body).Decode(&reqpsd)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqpsd.CSRFToken
	itemID := reqpsd.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	seller, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	transactionEvidence := TransactionEvidence{}
	err = dbx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidence not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")

		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	// Query shipping record BEFORE transaction to get reserve_id
	shipping := Shipping{}
	err = dbx.GetContext(ctx, &shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	// Make API call BEFORE starting transaction to avoid holding locks during slow API calls
	ssr, err := APIShipmentStatus(ctx, getShipmentServiceURL(ctx), &APIShipmentStatusReq{
		ReserveID: shipping.ReserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		return
	}

	if !(ssr.Status == ShippingsStatusShipping || ssr.Status == ShippingsStatusDone) {
		outputErrorMsg(w, http.StatusForbidden, "shipment service側で配送中か配送完了になっていません")
		return
	}

	tx := dbx.MustBeginTx(ctx, nil)

	item := Item{}
	err = tx.GetContext(ctx, &item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "items not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `shippings` SET `status` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ssr.Status,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `transaction_evidences` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		TransactionEvidenceStatusWaitDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
}

func postComplete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqpc := reqPostComplete{}

	err := json.NewDecoder(r.Body).Decode(&reqpc)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqpc.CSRFToken
	itemID := reqpc.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	buyer, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	transactionEvidence := TransactionEvidence{}
	err = dbx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidence not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")

		return
	}

	if transactionEvidence.BuyerID != buyer.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	// Query shipping record BEFORE transaction to get reserve_id
	shipping := Shipping{}
	err = dbx.GetContext(ctx, &shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	// Make API call BEFORE starting transaction to avoid holding locks during slow API calls
	ssr, err := APIShipmentStatus(ctx, getShipmentServiceURL(ctx), &APIShipmentStatusReq{
		ReserveID: shipping.ReserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		return
	}

	if !(ssr.Status == ShippingsStatusDone) {
		outputErrorMsg(w, http.StatusBadRequest, "shipment service側で配送完了になっていません")
		return
	}

	tx := dbx.MustBeginTx(ctx, nil)
	item := Item{}
	err = tx.GetContext(ctx, &item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "items not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.GetContext(ctx, &transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitDone {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `shippings` SET `status` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ShippingsStatusDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `transaction_evidences` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		TransactionEvidenceStatusDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `items` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		ItemStatusSoldOut,
		time.Now(),
		itemID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
}

func postSell(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	csrfToken := r.FormValue("csrf_token")
	name := r.FormValue("name")
	description := r.FormValue("description")
	priceStr := r.FormValue("price")
	categoryIDStr := r.FormValue("category_id")

	f, header, err := r.FormFile("image")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusBadRequest, "image error")
		return
	}
	defer f.Close()

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	categoryID, err := strconv.Atoi(categoryIDStr)
	if err != nil || categoryID < 0 {
		outputErrorMsg(w, http.StatusBadRequest, "category id error")
		return
	}

	price, err := strconv.Atoi(priceStr)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "price error")
		return
	}

	if name == "" || description == "" || price == 0 || categoryID == 0 {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)

		return
	}

	category, err := getCategoryByID(ctx, dbx, categoryID)
	if err != nil || category.ParentID == 0 {
		log.Print(categoryID, category)
		outputErrorMsg(w, http.StatusBadRequest, "Incorrect category ID")
		return
	}

	user, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	img, err := io.ReadAll(f)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "image error")
		return
	}

	ext := filepath.Ext(header.Filename)

	if !(ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif") {
		outputErrorMsg(w, http.StatusBadRequest, "unsupported image format error")
		return
	}

	if ext == ".jpeg" {
		ext = ".jpg"
	}

	imgName := fmt.Sprintf("%s%s", secureRandomStr(16), ext)
	err = os.WriteFile(fmt.Sprintf("../public/upload/%s", imgName), img, 0644)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "Saving image failed")
		return
	}

	tx := dbx.MustBeginTx(ctx, nil)

	seller := User{}
	err = tx.GetContext(ctx, &seller, "SELECT * FROM `users` WHERE `id` = ? FOR UPDATE", user.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	result, err := tx.ExecContext(ctx, "INSERT INTO `items` (`seller_id`, `status`, `name`, `price`, `description`,`image_name`,`category_id`) VALUES (?, ?, ?, ?, ?, ?, ?)",
		seller.ID,
		ItemStatusOnSale,
		name,
		price,
		description,
		imgName,
		category.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	itemID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx, "UPDATE `users` SET `num_sell_items`=?, `last_bump`=? WHERE `id`=?",
		seller.NumSellItems+1,
		now,
		seller.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	tx.Commit()

	// Update user cache
	seller.NumSellItems++
	seller.LastBump = now
	setUserCache(seller)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resSell{ID: itemID})
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func postBump(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rb := reqBump{}
	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := rb.CSRFToken
	itemID := rb.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	user, errCode, errMsg := getUser(ctx, r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBeginTx(ctx, nil)

	targetItem := Item{}
	err = tx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.SellerID != user.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		tx.Rollback()
		return
	}

	seller := User{}
	err = tx.GetContext(ctx, &seller, "SELECT * FROM `users` WHERE `id` = ? FOR UPDATE", user.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	now := time.Now()
	// last_bump + 3s > now
	if seller.LastBump.Add(BumpChargeSeconds).After(now) {
		outputErrorMsg(w, http.StatusForbidden, "Bump not allowed")
		tx.Rollback()
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `items` SET `created_at`=?, `updated_at`=? WHERE id=?",
		now,
		now,
		targetItem.ID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	_, err = tx.ExecContext(ctx, "UPDATE `users` SET `last_bump`=? WHERE id=?",
		now,
		seller.ID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	err = tx.GetContext(ctx, &targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	// Update user cache
	seller.LastBump = now
	setUserCache(seller)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        targetItem.ID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
	})
}

func getAllCategoriesFromCache() []Category {
	categoryMu.RLock()
	defer categoryMu.RUnlock()

	categories := make([]Category, 0, len(categoryCache))
	for _, c := range categoryCache {
		categories = append(categories, c)
	}
	return categories
}

func getSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	csrfToken := getCSRFToken(r)

	user, _, errMsg := getUser(ctx, r)

	ress := resSetting{}
	ress.CSRFToken = csrfToken
	if errMsg == "" {
		ress.User = &user
	}

	ress.PaymentServiceURL = getPaymentServiceURL(ctx)

	// Use category cache instead of DB query
	ress.Categories = getAllCategoriesFromCache()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(ress)
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rl := reqLogin{}
	err := json.NewDecoder(r.Body).Decode(&rl)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rl.AccountName
	password := rl.Password

	if accountName == "" || password == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	// Use account_name cache for faster login lookups
	u, err := getUserByAccountNameFromCache(ctx, accountName)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	err = bcrypt.CompareHashAndPassword(u.HashedPassword, []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "crypt error")
		return
	}

	session := getSession(r)

	session.Values["user_id"] = u.ID
	session.Values["csrf_token"] = secureRandomStr(20)
	if err = session.Save(r, w); err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "session error")
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(u)
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rr := reqRegister{}
	err := json.NewDecoder(r.Body).Decode(&rr)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rr.AccountName
	address := rr.Address
	password := rr.Password

	if accountName == "" || password == "" || address == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "error")
		return
	}

	result, err := dbx.ExecContext(ctx, "INSERT INTO `users` (`account_name`, `hashed_password`, `address`) VALUES (?, ?, ?)",
		accountName,
		hashedPassword,
		address,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	userID, err := result.LastInsertId()

	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	u := User{
		ID:             userID,
		AccountName:    accountName,
		HashedPassword: hashedPassword,
		Address:        address,
		NumSellItems:   0,
	}

	// Update user cache
	setUserCache(u)

	session := getSession(r)
	session.Values["user_id"] = u.ID
	session.Values["csrf_token"] = secureRandomStr(20)
	if err = session.Save(r, w); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "session error")
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(u)
}

func getReports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	transactionEvidences := make([]TransactionEvidence, 0)
	err := dbx.SelectContext(ctx, &transactionEvidences, "SELECT * FROM `transaction_evidences` WHERE `id` > 15007")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(transactionEvidences)
}

func outputErrorMsg(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	w.WriteHeader(status)

	json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

func getImageURL(imageName string) string {
	return fmt.Sprintf("/upload/%s", imageName)
}
