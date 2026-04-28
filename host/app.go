package host

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/auth"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/idempotency"
	"github.com/asteby/metacore-kernel/marketplace"
	kernellog "github.com/asteby/metacore-kernel/log"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/metrics"
	"github.com/asteby/metacore-kernel/migrations"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
	"github.com/asteby/metacore-kernel/push"
	"github.com/asteby/metacore-kernel/vector"
	metacorews "github.com/asteby/metacore-kernel/ws"
	"github.com/asteby/metacore-kernel/webhooks"
)

// AppConfig is the single structure a consumer app needs to boot a complete
// metacore-backed API. Everything has sensible defaults: set DB, JWTSecret,
// and optionally enable Push/Webhooks.
//
//	app := host.NewApp(host.AppConfig{
//	    DB:        db,
//	    JWTSecret: []byte(os.Getenv("JWT_SECRET")),
//	}).MustRegisterModels("products", &models.Product{}, "customers", &models.Customer{})
//	app.Mount(fiberApp.Group("/api"))
type AppConfig struct {
	DB        *gorm.DB
	JWTSecret []byte

	// AuthMiddlewareSkipper lets /auth/login and /auth/register remain public.
	AuthMiddlewareSkipper func(*fiber.Ctx) bool

	// Optional integrations: omit to disable.
	EnablePush     bool
	VAPIDPublic    string
	VAPIDPrivate   string
	VAPIDSubject   string
	EnableWebhooks bool
	WebhookOwner   webhooks.OwnerResolver // typically resolves org_id from JWT

	// Permissions are optional — if nil, dynamic handler skips authz checks.
	PermissionStore permission.PermissionStore

	// Logger is the structured slog logger used by kernel middleware.
	// If nil, a production-ready JSON logger is created automatically.
	Logger *slog.Logger

	// EnableMetrics mounts a Prometheus /metrics endpoint and request-level
	// instrumentation middleware on the Fiber router passed to Mount.
	EnableMetrics bool

	// RunMigrations, when true, runs the versioned SQL migration runner
	// (migrations.Runner) instead of GORM AutoMigrate during NewApp.
	// Recommended for all production deployments.
	RunMigrations bool

	// Translator, when set, localizes any string in TableMetadata /
	// ModalMetadata that starts with `I18nKeyPrefix` (default "models.").
	// The kernel auto-registers metadata transformers and the
	// Accept-Language Fiber middleware. Hosts pick the bundle: see
	// `github.com/asteby/metacore-kernel/i18n` for the contract.
	Translator i18n.Translator

	// I18nDefaultLanguage is the language tag used when the
	// `Accept-Language` header is missing on incoming requests. Defaults
	// to "en". Ignored when Translator is nil.
	I18nDefaultLanguage string

	// I18nKeyPrefix selects which strings the metadata transformers
	// translate. Defaults to "models." — set to "" to translate every
	// string field (rare; usually you want the prefix guard).
	I18nKeyPrefix string

	// EnableVectorStore wires `app.VectorStore` (PGStore by default) and
	// runs `CREATE EXTENSION IF NOT EXISTS vector` on boot. Requires the
	// `pgvector/pgvector` Postgres image; vanilla `postgres:*` will panic
	// on the CREATE EXTENSION call.
	EnableVectorStore bool

	// EnableEmbedder wires `app.Embedder` (RemoteEmbedder by default,
	// reading BGE_EMBEDDING_URL / BGE_EMBEDDING_MODEL env vars). Set
	// independent of EnableVectorStore — apps may have a vector backend
	// (Qdrant, Pinecone) without using the kernel's embedder, or vice versa.
	EnableEmbedder bool

	// EmbeddingURL overrides the embedding endpoint when EnableEmbedder is
	// true. Empty falls back to BGE_EMBEDDING_URL or the BGE-M3 default.
	EmbeddingURL string

	// EmbeddingModel overrides the embedding model name. Empty falls back
	// to BGE_EMBEDDING_MODEL or "bge-m3".
	EmbeddingModel string

	// EnableMarketplace mounts the marketplace install endpoint
	// (`POST /marketplace/install`) that records "user clicked Install"
	// requests from the embedded Hub iframe in a `marketplace_installations`
	// table. Off by default; set to true on apps that ship the Hub
	// marketplace tab.
	EnableMarketplace bool

	// EnableIdempotencyKey wires the `Idempotency-Key` Stripe-style replay
	// middleware on every state-mutating POST handler the kernel mounts
	// (`/dynamic/:model` create, `/dynamic/:model/import`). Clients that
	// retry a failed request with the same header get the original
	// response replayed instead of producing duplicates. Off by default
	// to keep the kernel lean for read-only deployments.
	EnableIdempotencyKey bool

	// IdempotencyStore overrides the backing store. Empty uses an in-memory
	// LRU sized for single-replica apps; multi-replica deployments should
	// drop in a Redis-backed implementation that satisfies idempotency.Store.
	IdempotencyStore idempotency.Store

	// IdempotencyTTL is the replay window. Defaults to 24h (Stripe-aligned).
	IdempotencyTTL time.Duration

	// Overrides
	MetadataCacheTTL time.Duration // default 5m
	JWTExpiry        time.Duration // default 24h
}

// App is the cohesive boot helper. It owns the kernel services and exposes
// Mount() to wire Fiber routes in one call.
type App struct {
	Config AppConfig

	Auth       *auth.Service
	Metadata   *metadata.Service
	Permission *permission.Service
	Dynamic    *dynamic.Service
	Push       *push.Service
	Webhooks   *webhooks.Service
	WSHub      *metacorews.Hub

	// Metrics registry — non-nil when AppConfig.EnableMetrics is true.
	Metrics *metrics.Registry

	// VectorStore is non-nil when AppConfig.EnableVectorStore is true.
	// Apps wire their semantic-search pipelines on top — see kernel/vector.
	VectorStore vector.Store

	// Embedder is non-nil when AppConfig.EnableEmbedder is true.
	Embedder vector.Embedder

	// IdempotencyStore is non-nil when AppConfig.EnableIdempotencyKey is
	// true. Apps can stick their own entries on top via Put — useful for
	// custom POST routes that should also dedupe retries.
	IdempotencyStore idempotency.Store

	idempotencyMW       fiber.Handler
	authHandler         *auth.Handler
	metaHandler         *metadata.Handler
	dynHandler          *dynamic.Handler
	pushHandler         *push.Handler
	marketplaceHandler  *marketplace.Handler
	webhooksHandler *webhooks.Handler
}

// NewApp wires the default stack: auth + metadata + dynamic (+ optional
// permission, push, webhooks). Panics on missing required config.
func NewApp(cfg AppConfig) *App {
	if cfg.DB == nil {
		panic("host: AppConfig.DB is required")
	}
	if len(cfg.JWTSecret) == 0 {
		panic("host: AppConfig.JWTSecret is required")
	}
	if cfg.MetadataCacheTTL == 0 {
		cfg.MetadataCacheTTL = 5 * time.Minute
	}
	if cfg.JWTExpiry == 0 {
		cfg.JWTExpiry = 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = kernellog.Default()
	}

	if cfg.RunMigrations {
		// Versioned migration runner — safe for production. Tracks applied
		// versions in the goose_db_version table and is idempotent.
		runner := migrations.Runner{}
		if err := runner.Up(context.Background(), cfg.DB); err != nil {
			panic("host: migration runner failed: " + err.Error())
		}
	} else {
		// Deprecated: AutoMigrate is retained for local development and
		// backward compatibility. Use AppConfig.RunMigrations=true in all
		// production deployments to get versioned, auditable schema changes.
		_ = cfg.DB.AutoMigrate(&modelbase.BaseUser{}, &modelbase.BaseOrganization{})
		if cfg.EnableWebhooks {
			_ = cfg.DB.AutoMigrate(&webhooks.Webhook{}, &webhooks.WebhookDelivery{})
		}
		if cfg.EnablePush {
			_ = cfg.DB.AutoMigrate(&push.PushSubscription{})
		}
	}

	authSvc := auth.New(cfg.DB, auth.Config{
		JWTSecret: cfg.JWTSecret,
		JWTExpiry: cfg.JWTExpiry,
	}).WithUserModel(func() modelbase.AuthUser {
		return &modelbase.BaseUser{}
	}).WithOrgModel(func() modelbase.AuthOrg {
		return &modelbase.BaseOrganization{}
	})

	metaSvc := metadata.New(metadata.Config{CacheTTL: cfg.MetadataCacheTTL})

	// Localized metadata: when a Translator is configured the kernel
	// transparently rewrites every "models.*" string in TableMetadata /
	// ModalMetadata before the response leaves the wire.
	if cfg.Translator != nil {
		prefix := cfg.I18nKeyPrefix
		if t := metadata.NewLocalizedTableTransformer(cfg.Translator, prefix); t != nil {
			metaSvc.WithTableTransformer(t)
		}
		if t := metadata.NewLocalizedModalTransformer(cfg.Translator, prefix); t != nil {
			metaSvc.WithModalTransformer(t)
		}
	}

	var permSvc *permission.Service
	if cfg.PermissionStore != nil {
		permSvc = permission.New(permission.Config{Store: cfg.PermissionStore})
	}

	dynSvc := dynamic.New(dynamic.Config{
		DB:          cfg.DB,
		Metadata:    metaSvc,
		Permissions: permSvc,
	})

	a := &App{
		Config:     cfg,
		Auth:       authSvc,
		Metadata:   metaSvc,
		Permission: permSvc,
		Dynamic:    dynSvc,
	}

	if cfg.EnableMetrics {
		a.Metrics = metrics.NewRegistry()
	}

	if cfg.EnableVectorStore {
		// pgvector lives in an extension that must be created before any
		// table tries to declare a `vector(N)` column. Idempotent.
		if err := cfg.DB.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
			panic("host: failed to enable pgvector extension: " + err.Error())
		}
		a.VectorStore = vector.NewPGStore(cfg.DB)
	}
	if cfg.EnableEmbedder {
		a.Embedder = vector.NewRemoteEmbedder(vector.RemoteEmbedderConfig{
			BaseURL: cfg.EmbeddingURL,
			Model:   cfg.EmbeddingModel,
		})
	}

	if cfg.EnableIdempotencyKey {
		store := cfg.IdempotencyStore
		if store == nil {
			store = idempotency.NewInMemoryStore(0)
		}
		a.IdempotencyStore = store
		a.idempotencyMW = idempotency.Middleware(idempotency.Config{
			Store: store,
			TTL:   cfg.IdempotencyTTL,
			UserKey: func(c *fiber.Ctx) string {
				if uid := auth.GetUserID(c); uid != uuid.Nil {
					return uid.String()
				}
				return c.IP()
			},
		})
	}

	a.authHandler = auth.NewHandler(authSvc)
	a.metaHandler = metadata.NewHandler(metaSvc)
	a.dynHandler = dynamic.NewHandler(dynSvc, func(c *fiber.Ctx) modelbase.AuthUser {
		uid := auth.GetUserID(c)
		orgID := auth.GetOrganizationID(c)
		role := auth.GetRole(c)
		email := auth.GetEmail(c)
		u := &modelbase.BaseUser{}
		u.ID = uid
		u.OrganizationID = orgID
		u.Role = string(role)
		u.Email = email
		return u
	})

	if cfg.EnablePush {
		a.Push = push.New(push.Config{
			DB:           cfg.DB,
			VAPIDPublic:  cfg.VAPIDPublic,
			VAPIDPrivate: cfg.VAPIDPrivate,
			VAPIDSubject: cfg.VAPIDSubject,
		})
		a.pushHandler = push.NewHandler(a.Push, func(c *fiber.Ctx) uuid.UUID {
			return auth.GetUserID(c)
		})
	}

	if cfg.EnableWebhooks {
		a.Webhooks = webhooks.New(webhooks.Config{DB: cfg.DB})
		resolver := cfg.WebhookOwner
		if resolver == nil {
			// default: org-scoped via JWT claims
			resolver = func(c *fiber.Ctx) (string, uuid.UUID, error) {
				return "organization", auth.GetOrganizationID(c), nil
			}
		}
		a.webhooksHandler = webhooks.NewHandler(a.Webhooks, resolver)
	}

	if cfg.EnableMarketplace {
		mh, err := marketplace.NewHandler(cfg.DB)
		if err != nil {
			panic("host: marketplace.NewHandler: " + err.Error())
		}
		a.marketplaceHandler = mh
	}

	// WebSocket hub — always available
	a.WSHub = metacorews.NewHub()
	go a.WSHub.Run()

	return a
}

// RegisterModel adds a domain model to the metadata registry. Call for every
// model that should have dynamic CRUD endpoints.
func (a *App) RegisterModel(key string, factory func() modelbase.ModelDefiner) *App {
	modelbase.Register(key, factory)
	return a
}

// Mount wires every enabled handler onto the given base router. Apps usually
// mount under /api.
//
//	authenticated := app.Mount(fiber.Group("/api"))
//
// Returns the authenticated sub-router so apps can add their own routes
// (including the dynamic CRUD handler once that package is implemented).
func (a *App) Mount(r fiber.Router) fiber.Router {
	// Structured logging — injects request_id and logs every request.
	r.Use(kernellog.FiberMiddleware(a.Config.Logger))

	// Accept-Language extraction so metadata transformers (and any
	// app-level handler that calls i18n.LanguageFromContext) get the
	// caller's preferred language out of the box.
	if a.Config.Translator != nil {
		def := a.Config.I18nDefaultLanguage
		if def == "" {
			def = "en"
		}
		r.Use(i18n.FiberMiddleware(def))
	}

	// Prometheus metrics — increments counters and observes latency.
	if a.Metrics != nil {
		r.Use(metrics.FiberMiddleware(a.Metrics))
		r.Get("/metrics", metrics.Handler(a.Metrics))
	}

	mw := auth.Middleware(auth.MiddlewareConfig{
		Secret:  a.Config.JWTSecret,
		Skipper: a.Config.AuthMiddlewareSkipper,
	})

	// Public auth endpoints (login, register are skipper-exempt inside auth).
	a.authHandler.Mount(r.Group("/auth"), mw)

	// Authenticated endpoints
	api := r.Group("", mw)
	a.metaHandler.Mount(api.Group("/metadata"))

	// Wire the optional Idempotency-Key middleware over POST /create and
	// POST /import — the two state-mutating endpoints the kernel hosts.
	dynOpts := dynamic.MountOpts{}
	if a.idempotencyMW != nil {
		dynOpts.MutationMiddleware = []fiber.Handler{a.idempotencyMW}
	}
	a.dynHandler.MountWith(dynOpts)(api.Group(""))

	if a.pushHandler != nil {
		a.pushHandler.Mount(api.Group("/push"))
	}
	if a.marketplaceHandler != nil {
		a.marketplaceHandler.Mount(api)
	}
	if a.webhooksHandler != nil {
		a.webhooksHandler.Mount(api.Group("/webhooks"))
	}

	// WebSocket — mounted with query-string auth (token in ?token=)
	metacorews.Mount(r, a.WSHub, auth.Middleware(auth.MiddlewareConfig{
		Secret: a.Config.JWTSecret,
	}), "user_id")

	return api
}

// Stop gracefully shuts down background workers (webhooks dispatcher).
func (a *App) Stop() error {
	if a.Webhooks != nil {
		return a.Webhooks.Stop()
	}
	return nil
}

// MustGetenv is a tiny helper apps can use in their main.go to surface missing env.
func MustGetenv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("host: missing env " + key)
	}
	return v
}
