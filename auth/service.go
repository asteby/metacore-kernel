package auth

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Config holds runtime configuration for the auth Service.
// Defaults: JWTExpiry=24h, BcryptCost=DefaultBcryptCost(10).
type Config struct {
	JWTSecret  []byte
	JWTIssuer  string
	JWTExpiry  time.Duration
	BcryptCost int
}

// UserFactory returns a new (empty) instance of the app-specific User model.
// The returned value MUST be a pointer so GORM can populate it via Preload/Find.
// The concrete type is expected to embed modelbase.BaseUser and satisfy modelbase.AuthUser.
type UserFactory func() modelbase.AuthUser

// OrgFactory returns a new empty organization model instance. Optional —
// apps that don't have a separate org concept can omit it.
type OrgFactory func() modelbase.AuthOrg

// Hook runs after a successful login or register. It receives the authenticated
// user and the signed token so app-specific concerns (audit log, branch
// selection, extra claims) can plug in without touching the core flow.
type Hook func(ctx context.Context, user modelbase.AuthUser, org modelbase.AuthOrg, token string) error

// LoginInput is the input payload for Service.Login.
type LoginInput struct {
	Email    string
	Password string
}

// RegisterInput is the input payload for Service.Register. Apps may want
// additional fields (country, org logo, …) — those belong in the handler
// layer on top of this generic shape.
type RegisterInput struct {
	Name             string
	Email            string
	Password         string
	Role             string
	OrganizationName string
	// Extra is a free-form map apps can pass to hooks for app-specific setup
	// (e.g. country, plan slug, org logo). Not persisted by core.
	Extra map[string]any
}

// LoginResult is the shared success payload for Login/Register. Organization
// may be nil interface when the app doesn't use orgs.
type LoginResult struct {
	User         modelbase.AuthUser
	Organization modelbase.AuthOrg
	Token        string
	ExpiresAt    time.Time
}

// Service is the auth core. Construct via New and optionally configure with
// WithUserModel / WithOrgModel / WithPostLoginHook / WithPostRegisterHook.
type Service struct {
	db     *gorm.DB
	config Config

	userFactory UserFactory
	orgFactory  OrgFactory

	postLogin    Hook
	postRegister Hook

	// preloads are GORM association names to Preload on every user load.
	// Populated automatically when the user factory exposes an "Organization"
	// field, so the common case Just Works, but callers can override via
	// WithPreloads when their model names the relation differently.
	preloads []string
}

// New constructs a Service with the given DB and Config. A UserFactory MUST
// be attached via WithUserModel before calling Login/Register/Me.
func New(db *gorm.DB, config Config) *Service {
	if config.JWTExpiry <= 0 {
		config.JWTExpiry = 24 * time.Hour
	}
	if config.BcryptCost <= 0 {
		config.BcryptCost = DefaultBcryptCost
	}
	return &Service{db: db, config: config}
}

// WithUserModel attaches the factory producing the app-specific User model.
// The factory's return value must satisfy modelbase.AuthUser.
func (s *Service) WithUserModel(f UserFactory) *Service {
	s.userFactory = f
	// Auto-detect an Organization association to Preload on reads.
	if f != nil && s.preloads == nil {
		sample := f()
		if sample != nil {
			t := reflect.TypeOf(sample)
			for t.Kind() == reflect.Ptr {
				t = t.Elem()
			}
			if t.Kind() == reflect.Struct {
				if _, ok := t.FieldByName("Organization"); ok {
					s.preloads = []string{"Organization"}
				}
			}
		}
	}
	return s
}

// WithPreloads overrides the GORM associations loaded on every user fetch.
// Pass nil to disable preloading.
func (s *Service) WithPreloads(preloads []string) *Service {
	s.preloads = preloads
	return s
}

// WithOrgModel attaches the factory producing the app-specific Org model.
func (s *Service) WithOrgModel(f OrgFactory) *Service {
	s.orgFactory = f
	return s
}

// WithPostLoginHook registers a hook to run after Login succeeds (before the
// token is returned to the client). Errors from the hook are returned and
// abort the login.
func (s *Service) WithPostLoginHook(h Hook) *Service {
	s.postLogin = h
	return s
}

// WithPostRegisterHook registers a hook to run after Register succeeds.
func (s *Service) WithPostRegisterHook(h Hook) *Service {
	s.postRegister = h
	return s
}

// DB exposes the underlying gorm.DB for apps that need to perform related
// operations (e.g. seed roles, run migrations) in the same session.
func (s *Service) DB() *gorm.DB { return s.db }

// Config returns a copy of the effective runtime config.
func (s *Service) Config() Config { return s.config }

// Login authenticates a user by email + password.
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	if s.userFactory == nil {
		return nil, ErrUserModelNotSet
	}
	if in.Email == "" || in.Password == "" {
		return nil, ErrInvalidCredentials
	}

	user := s.userFactory()
	db := s.db.WithContext(ctx)
	q := s.applyPreloads(db)
	if err := q.Where("email = ?", in.Email).First(user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if !CheckPassword(user.GetPasswordHash(), in.Password) {
		return nil, ErrInvalidCredentials
	}

	return s.buildResult(ctx, user, s.postLogin)
}

// Register creates a new user (and optionally a new organization) and
// returns a fresh LoginResult. Register is intentionally minimal — apps
// that need plans, trials, or country-specific setup should layer that via
// a post-register hook.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*LoginResult, error) {
	if s.userFactory == nil {
		return nil, ErrUserModelNotSet
	}
	if in.Name == "" || in.Email == "" || in.Password == "" {
		return nil, errors.New("auth: name, email and password are required")
	}

	db := s.db.WithContext(ctx)

	// Check for existing user.
	existing := s.userFactory()
	if err := db.Where("email = ?", in.Email).Limit(1).Find(existing).Error; err == nil {
		if existing.GetID() != uuid.Nil {
			return nil, ErrUserExists
		}
	}

	hashed, err := HashPassword(in.Password, s.config.BcryptCost)
	if err != nil {
		return nil, err
	}

	tx := db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}

	var org modelbase.AuthOrg
	var orgID uuid.UUID
	if s.orgFactory != nil {
		org = s.orgFactory()
		orgName := in.OrganizationName
		if orgName == "" {
			orgName = in.Name + "'s Organization"
		}
		org.SetName(orgName)
		if err := tx.Create(org).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		orgID = org.GetID()
	}

	user := s.userFactory()
	user.SetEmail(in.Email)
	user.SetName(in.Name)
	user.SetPasswordHash(hashed)
	role := in.Role
	if role == "" {
		role = "owner"
	}
	user.SetRole(role)
	if orgID != uuid.Nil {
		user.SetOrganizationID(orgID)
	}

	if err := tx.Create(user).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}

	// Reload with preloaded relations for a complete response.
	reloaded := s.userFactory()
	if err := s.applyPreloads(db).First(reloaded, "id = ?", user.GetID()).Error; err == nil {
		user = reloaded
	}

	return s.buildResult(ctx, user, s.postRegister)
}

// Me returns the current user loaded from the DB. The returned value is
// the app-specific User type exposed through modelbase.AuthUser.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (modelbase.AuthUser, error) {
	if s.userFactory == nil {
		return nil, ErrUserModelNotSet
	}
	user := s.userFactory()
	db := s.db.WithContext(ctx)
	if err := s.applyPreloads(db).First(user, "id = ?", userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return user, nil
}

// applyPreloads chains the configured Preload() calls onto a query.
func (s *Service) applyPreloads(tx *gorm.DB) *gorm.DB {
	for _, p := range s.preloads {
		tx = tx.Preload(p)
	}
	return tx
}

// IssueToken issues a new JWT for an already-authenticated user. Useful for
// refresh-token flows that live on top of this package.
func (s *Service) IssueToken(user modelbase.AuthUser) (string, time.Time, error) {
	claims := Claims{
		UserID:         user.GetID(),
		OrganizationID: user.GetOrganizationID(),
		Email:          user.GetEmail(),
		Role:           user.GetRole(),
	}
	if s.config.JWTIssuer != "" {
		claims.Issuer = s.config.JWTIssuer
	}
	return GenerateToken(claims, s.config.JWTSecret, s.config.JWTExpiry)
}

// ValidateToken is a thin wrapper so middleware / handlers can validate
// without importing the package-level helper separately.
func (s *Service) ValidateToken(tokenStr string) (*Claims, error) {
	return ValidateToken(tokenStr, s.config.JWTSecret)
}

// buildResult centralizes the post-auth flow: sign token, pluck org, invoke hook.
func (s *Service) buildResult(ctx context.Context, user modelbase.AuthUser, hook Hook) (*LoginResult, error) {
	token, expiresAt, err := s.IssueToken(user)
	if err != nil {
		return nil, err
	}

	var org modelbase.AuthOrg
	// TODO: requires modelbase — BaseUser is expected to expose a GetOrganization()
	// accessor returning modelbase.AuthOrg (or nil) loaded via Preload above.
	if withOrg, ok := user.(interface{ GetOrganization() modelbase.AuthOrg }); ok {
		org = withOrg.GetOrganization()
	}

	if hook != nil {
		if err := hook(ctx, user, org, token); err != nil {
			return nil, err
		}
	}

	return &LoginResult{
		User:         user,
		Organization: org,
		Token:        token,
		ExpiresAt:    expiresAt,
	}, nil
}
