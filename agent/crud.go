package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CRUDConfig wires a CRUDService to the host through the runtime's ports.
type CRUDConfig struct {
	DB      *gorm.DB
	Catalog ModelCatalog
	// Permissions authorizes every call on behalf of the human the copilot
	// acts for. Required unless DevAllowAll is set.
	Permissions PermissionChecker
	// Gate hides data of modules the org has not enabled. Optional; nil means
	// every model is reachable.
	Gate ModuleGate
	// Index adds semantic search and re-indexing after writes. Optional.
	Index SemanticIndex
	// NormalizeValue lets the host reshape one value of a mutation before it
	// is written (for example turning an LLM "increment" line into a signed
	// delta). Optional; it must return v unchanged when it has nothing to do.
	NormalizeValue func(model, key string, v any) any
	// ColumnPermissions checks the permission a column declares in
	// ModelMeta.ReadPermissions. nil with a model that declares any = those
	// columns are hidden from every agent call (fail closed).
	ColumnPermissions NamedPermissionChecker
	// DevAllowAll skips permission checks. Never enable it in production.
	DevAllowAll bool
}

// CRUDService provides permission-checked, org-scoped data access for agents.
// Every call resolves the model through the host's catalog, checks the module
// gate and the permission checker, then runs parameterized SQL scoped to the
// caller's organization.
type CRUDService struct {
	cfg CRUDConfig
}

// ErrNoPermissionChecker is returned by NewCRUDService when no authorization
// seam was provided.
var ErrNoPermissionChecker = errors.New("agent: CRUDConfig.Permissions is required (or set DevAllowAll)")

// NewCRUDService builds a CRUDService.
func NewCRUDService(cfg CRUDConfig) (*CRUDService, error) {
	if cfg.Catalog == nil {
		return nil, errors.New("agent: CRUDConfig.Catalog is required")
	}
	if cfg.Permissions == nil && !cfg.DevAllowAll {
		return nil, ErrNoPermissionChecker
	}
	return &CRUDService{cfg: cfg}, nil
}

// AgentIdentity is the auth context of a call.
type AgentIdentity struct {
	AgentID        uuid.UUID
	OrganizationID uuid.UUID
	UserID         uuid.UUID // the human the copilot acts for
	RoleID         *uuid.UUID
}

// QueryRequest is a structured data query.
type QueryRequest struct {
	Model   string         `json:"model"`
	Filters map[string]any `json:"filters,omitempty"`
	Search  string         `json:"search,omitempty"`
	OrderBy string         `json:"order_by,omitempty"`
	Limit   int            `json:"limit,omitempty"`
}

// AggregateRequest is an aggregate operation.
type AggregateRequest struct {
	Model    string         `json:"model"`
	Function string         `json:"function"` // count, sum, avg, min, max
	Field    string         `json:"field,omitempty"`
	GroupBy  string         `json:"group_by,omitempty"`
	Filters  map[string]any `json:"filters,omitempty"`
}

// MutateRequest is a create/update/delete operation.
type MutateRequest struct {
	Model    string         `json:"model"`
	RecordID string         `json:"record_id,omitempty"` // update/delete
	Data     map[string]any `json:"data,omitempty"`      // create/update
}

func (s *CRUDService) resolve(model string) string { return ResolveModelKey(s.cfg.Catalog, model) }

// CheckPermission verifies the identity may run action on model: the module
// that owns the model must be enabled for the org, and the user must hold the
// permission. A role's permission bits say nothing about whether the ORG has
// the addon installed, so the gate runs first.
func (s *CRUDService) CheckPermission(identity *AgentIdentity, model, action string) error {
	if s.cfg.Gate != nil {
		if owner := s.cfg.Gate.OwnerOf(model); owner != "" && !s.cfg.Gate.IsEnabled(identity.OrganizationID, owner) {
			return fmt.Errorf("addon %q is not installed for this organization", owner)
		}
	}
	if s.cfg.DevAllowAll {
		return nil
	}
	if !s.cfg.Permissions.Can(identity.OrganizationID, identity.UserID, model, action) {
		return fmt.Errorf("permission denied: %s.%s", model, action)
	}
	return nil
}

// ListModels returns the models the identity may read. Models registered under
// both a key and a table name are deduped on table and exposed by table.
func (s *CRUDService) ListModels(identity *AgentIdentity) ([]map[string]any, error) {
	var result []map[string]any
	seen := map[string]struct{}{}
	for _, m := range s.cfg.Catalog.All() {
		if err := s.CheckPermission(identity, m.Key, "index"); err != nil {
			continue
		}
		listKey := m.Key
		if m.Table != "" {
			listKey = m.Table
		}
		dedupe := strings.ToLower(listKey)
		if _, ok := seen[dedupe]; ok {
			continue
		}
		seen[dedupe] = struct{}{}

		entry := map[string]any{
			"model": listKey,
			// Human title: LLMs copy raw i18n keys as model=.
			"title": ModelLabel(s.cfg.Catalog, listKey),
		}
		if cols := s.readMask(identity, m).allowed(m.Columns); len(cols) > 0 {
			entry["columns"] = cols
		}
		if m.ModelKey != "" && m.ModelKey != listKey {
			entry["model_key"] = m.ModelKey
		}
		if m.VectorEnabled {
			entry["vectorizable"] = true
		}
		result = append(result, entry)
	}
	return result, nil
}

// DescribeModel returns the schema of a model (permission-checked).
func (s *CRUDService) DescribeModel(identity *AgentIdentity, modelKey string) (map[string]any, error) {
	modelKey = s.resolve(modelKey)
	if err := s.CheckPermission(identity, modelKey, "index"); err != nil {
		return nil, err
	}
	m, ok := s.cfg.Catalog.Lookup(modelKey)
	if !ok {
		return nil, fmt.Errorf("model '%s' not found", modelKey)
	}
	result := map[string]any{"model": modelKey, "title": ModelLabel(s.cfg.Catalog, modelKey)}
	mask := s.readMask(identity, m)
	for k, v := range m.Schema {
		result[k] = mask.pruneSchemaValue(k, v)
	}
	if m.VectorEnabled {
		result["vectorizable"] = true
		result["vector_fields"] = m.VectorFields
	}
	return result, nil
}

// Query performs a structured query with exact filters.
func (s *CRUDService) Query(identity *AgentIdentity, req QueryRequest) (*QueryResult, error) {
	raw := strings.TrimSpace(req.Model)
	req.Model = s.resolve(req.Model)
	if err := guardResolvedModel(raw, req.Model); err != nil {
		return nil, err
	}
	if err := s.CheckPermission(identity, req.Model, "index"); err != nil {
		return nil, err
	}
	table := s.tableName(req.Model)
	if table == "" {
		return nil, fmt.Errorf("model '%s' not found", req.Model)
	}
	mask := s.readMaskFor(identity, req.Model)
	for k := range req.Filters {
		if err := mask.check(req.Model, k, "filter"); err != nil {
			return nil, err
		}
	}
	if f := strings.Fields(req.OrderBy); len(f) > 0 {
		if err := mask.check(req.Model, f[0], "order"); err != nil {
			return nil, err
		}
	}

	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	q := s.cfg.DB.Table(table).
		Where("organization_id = ?", identity.OrganizationID).
		Where("deleted_at IS NULL").
		Limit(limit)

	for k, v := range req.Filters {
		if !isValidField(k) {
			continue
		}
		q = q.Where(fmt.Sprintf("%s = ?", k), v)
	}

	if req.Search != "" {
		if cols := mask.allowed(s.searchColumns(req.Model)); len(cols) > 0 {
			var conds []string
			var args []any
			for _, col := range cols {
				if !isValidField(col) {
					continue
				}
				conds = append(conds, fmt.Sprintf("%s ILIKE ?", col))
				args = append(args, "%"+req.Search+"%")
			}
			if len(conds) > 0 {
				q = q.Where(strings.Join(conds, " OR "), args...)
			}
		}
	}

	if req.OrderBy != "" && isValidOrderBy(req.OrderBy) {
		q = q.Order(req.OrderBy)
	} else {
		q = q.Order("created_at DESC")
	}

	var records []map[string]any
	if err := q.Find(&records).Error; err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	for i := range records {
		stripSensitive(records[i])
		mask.redact(records[i])
	}
	return &QueryResult{Model: req.Model, Count: len(records), Records: records}, nil
}

// GetRecord loads one record by id (permission-checked).
func (s *CRUDService) GetRecord(identity *AgentIdentity, model, recordID string) (map[string]any, error) {
	model = s.resolve(model)
	if err := s.CheckPermission(identity, model, "index"); err != nil {
		return nil, err
	}
	table := s.tableName(model)
	if table == "" {
		return nil, fmt.Errorf("model '%s' not found", model)
	}
	var row map[string]any
	if err := s.cfg.DB.Table(table).
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", recordID, identity.OrganizationID).
		Take(&row).Error; err != nil {
		return nil, fmt.Errorf("record not found: %w", err)
	}
	stripSensitive(row)
	s.readMaskFor(identity, model).redact(row)
	return row, nil
}

// VectorSearch performs semantic similarity search.
func (s *CRUDService) VectorSearch(identity *AgentIdentity, model, query string, limit int) (any, error) {
	model = s.resolve(model)
	if err := s.CheckPermission(identity, model, "index"); err != nil {
		return nil, err
	}
	if s.cfg.Index == nil {
		return nil, fmt.Errorf("semantic pipeline not available")
	}
	// The embedding already contains every vector field: when one of them is
	// a column the caller may not read, the similarity ranking itself would
	// leak it, so the search is refused instead of only redacting the rows.
	mask := s.readMaskFor(identity, model)
	if m, ok := s.cfg.Catalog.Lookup(model); ok {
		for _, f := range m.VectorFields {
			if err := mask.check(model, f, "vector search"); err != nil {
				return nil, err
			}
		}
	}
	results, err := s.cfg.Index.VectorSearch(model, query, identity.OrganizationID, limit)
	if err != nil {
		return nil, err
	}
	for i := range results {
		mask.redact(results[i].Data)
	}
	return results, nil
}

// Aggregate performs count/sum/avg/min/max (permission-checked).
func (s *CRUDService) Aggregate(identity *AgentIdentity, req AggregateRequest) (any, error) {
	raw := strings.TrimSpace(req.Model)
	req.Model = s.resolve(req.Model)
	if err := guardResolvedModel(raw, req.Model); err != nil {
		return nil, err
	}
	if err := s.CheckPermission(identity, req.Model, "index"); err != nil {
		return nil, err
	}
	table := s.tableName(req.Model)
	if table == "" {
		return nil, fmt.Errorf("model '%s' not found", req.Model)
	}

	mask := s.readMaskFor(identity, req.Model)
	for k := range req.Filters {
		if err := mask.check(req.Model, k, "filter"); err != nil {
			return nil, err
		}
	}
	if err := mask.check(req.Model, req.Field, "aggregate"); err != nil {
		return nil, err
	}
	if err := mask.check(req.Model, req.GroupBy, "group_by"); err != nil {
		return nil, err
	}

	q := s.cfg.DB.Table(table).
		Where("organization_id = ?", identity.OrganizationID).
		Where("deleted_at IS NULL")
	for k, v := range req.Filters {
		if !isValidField(k) {
			continue
		}
		q = q.Where(fmt.Sprintf("%s = ?", k), v)
	}

	var selectExpr string
	switch req.Function {
	case "count":
		selectExpr = "COUNT(*) as value"
	case "sum", "avg", "min", "max":
		if req.Field == "" || !isValidField(req.Field) {
			return nil, fmt.Errorf("field required for %s", req.Function)
		}
		selectExpr = fmt.Sprintf("%s(%s) as value", strings.ToUpper(req.Function), req.Field)
	default:
		return nil, fmt.Errorf("unsupported function: %s", req.Function)
	}

	if req.GroupBy != "" && isValidField(req.GroupBy) {
		var results []map[string]any
		if err := q.Select(fmt.Sprintf("%s, %s", req.GroupBy, selectExpr)).Group(req.GroupBy).Find(&results).Error; err != nil {
			return nil, fmt.Errorf("aggregate failed: %w", err)
		}
		return results, nil
	}
	var result map[string]any
	if err := q.Select(selectExpr).Find(&result).Error; err != nil {
		return nil, fmt.Errorf("aggregate failed: %w", err)
	}
	return result, nil
}

// Create inserts a record (permission-checked) and queues re-indexing.
func (s *CRUDService) Create(identity *AgentIdentity, req MutateRequest) (map[string]any, error) {
	return s.CreateContext(context.Background(), identity, req)
}

// CreateContext is Create bound to ctx: a provenance carried by ctx
// (provenance.WithContext) is stamped on the new record.
func (s *CRUDService) CreateContext(ctx context.Context, identity *AgentIdentity, req MutateRequest) (map[string]any, error) {
	raw := strings.TrimSpace(req.Model)
	req.Model = s.resolve(req.Model)
	if err := guardResolvedModel(raw, req.Model); err != nil {
		return nil, err
	}
	if err := s.CheckPermission(identity, req.Model, "create"); err != nil {
		return nil, err
	}
	table := s.tableName(req.Model)
	if table == "" {
		return nil, fmt.Errorf("model '%s' not found", req.Model)
	}

	req.Data = s.sanitizeMutationData(req.Model, req.Data, "create")
	encodeJSONColumns(req.Data, s.jsonColumns(req.Model))
	req.Data["organization_id"] = identity.OrganizationID
	id := uuid.New()
	req.Data["id"] = id

	if err := s.cfg.DB.WithContext(ctx).Table(table).Create(req.Data).Error; err != nil {
		return nil, fmt.Errorf("create failed: %w", err)
	}
	if s.cfg.Index != nil {
		go s.cfg.Index.EnqueueRecord(req.Model, id)
	}
	log.Printf("Agent CRUD: created %s/%v by agent %s", req.Model, id, identity.AgentID)
	return req.Data, nil
}

// Update modifies a record (permission-checked, org-scoped).
func (s *CRUDService) Update(identity *AgentIdentity, req MutateRequest) error {
	return s.UpdateContext(context.Background(), identity, req)
}

// UpdateContext is Update bound to ctx (provenance, cancellation).
func (s *CRUDService) UpdateContext(ctx context.Context, identity *AgentIdentity, req MutateRequest) error {
	raw := strings.TrimSpace(req.Model)
	req.Model = s.resolve(req.Model)
	if err := guardResolvedModel(raw, req.Model); err != nil {
		return err
	}
	if err := s.CheckPermission(identity, req.Model, "update"); err != nil {
		return err
	}
	table := s.tableName(req.Model)
	if table == "" {
		return fmt.Errorf("model '%s' not found", req.Model)
	}
	if req.RecordID == "" {
		return fmt.Errorf("record_id required for update")
	}

	req.Data = s.sanitizeMutationData(req.Model, req.Data, "update")
	encodeJSONColumns(req.Data, s.jsonColumns(req.Model))
	if len(req.Data) == 0 {
		return fmt.Errorf("no writable fields to update")
	}

	res := s.cfg.DB.WithContext(ctx).Table(table).
		Where("id = ? AND organization_id = ?", req.RecordID, identity.OrganizationID).
		Updates(req.Data)
	if res.Error != nil {
		return fmt.Errorf("update failed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record not found or access denied")
	}
	recordChange(ctx, s.cfg.DB, identity.OrganizationID, table, req.RecordID, provenance.ActionUpdated)
	if rid, err := uuid.Parse(req.RecordID); err == nil && s.cfg.Index != nil {
		go s.cfg.Index.EnqueueRecord(req.Model, rid)
	}
	log.Printf("Agent CRUD: updated %s/%s by agent %s", req.Model, req.RecordID, identity.AgentID)
	return nil
}

// Delete soft-deletes a record (permission-checked, org-scoped).
func (s *CRUDService) Delete(identity *AgentIdentity, req MutateRequest) error {
	return s.DeleteContext(context.Background(), identity, req)
}

// DeleteContext is Delete bound to ctx (provenance, cancellation).
func (s *CRUDService) DeleteContext(ctx context.Context, identity *AgentIdentity, req MutateRequest) error {
	raw := strings.TrimSpace(req.Model)
	req.Model = s.resolve(req.Model)
	if err := guardResolvedModel(raw, req.Model); err != nil {
		return err
	}
	if err := s.CheckPermission(identity, req.Model, "delete"); err != nil {
		return err
	}
	table := s.tableName(req.Model)
	if table == "" {
		return fmt.Errorf("model '%s' not found", req.Model)
	}
	if req.RecordID == "" {
		return fmt.Errorf("record_id required for delete")
	}

	res := s.cfg.DB.WithContext(ctx).Table(table).
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", req.RecordID, identity.OrganizationID).
		Update("deleted_at", time.Now().UTC())
	if res.Error != nil {
		return fmt.Errorf("delete failed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record not found or access denied")
	}
	recordChange(ctx, s.cfg.DB, identity.OrganizationID, table, req.RecordID, provenance.ActionDeleted)
	log.Printf("Agent CRUD: deleted %s/%s by agent %s", req.Model, req.RecordID, identity.AgentID)
	return nil
}

// --- helpers ---

// recordChange stores the provenance of an update/delete made by condition
// (the GORM hook cannot see the record id of a Where(...).Updates). Creates
// carry their id and are stamped by the hook. No provenance in ctx → no-op.
func recordChange(ctx context.Context, db *gorm.DB, orgID uuid.UUID, table, recordID, action string) {
	p, ok := provenance.FromContext(ctx)
	if !ok || db == nil || !provenance.Registered(db) {
		return
	}
	if err := provenance.NewStore(db).Record(ctx, provenance.NewEntry(orgID, table, recordID, action, p, time.Now())); err != nil {
		log.Printf("Agent CRUD: provenance %s %s/%s: %v", action, table, recordID, err)
	}
}

// tableName returns the SQL table of a resolved model, or "" when it has none
// or the key looks like an i18n key.
func (s *CRUDService) tableName(model string) string {
	m, ok := s.cfg.Catalog.Lookup(model)
	if !ok {
		return ""
	}
	table := m.Table
	if table == "" {
		table = model
	}
	if table == "" || strings.Contains(table, ".") {
		return ""
	}
	return table
}

func (s *CRUDService) searchColumns(model string) []string {
	if m, ok := s.cfg.Catalog.Lookup(model); ok {
		return m.SearchIn
	}
	return nil
}

func (s *CRUDService) jsonColumns(model string) map[string]struct{} {
	out := map[string]struct{}{}
	if m, ok := s.cfg.Catalog.Lookup(s.resolve(model)); ok {
		for _, c := range m.JSONColumns {
			out[c] = struct{}{}
		}
	}
	return out
}

// guardResolvedModel rejects empty / dotted keys before any SQL hits Postgres.
func guardResolvedModel(raw, resolved string) error {
	if resolved == "" || strings.Contains(resolved, ".") {
		return fmt.Errorf(
			"modelo %q no reconocido (parece clave i18n). Usa list_models y pasa el campo model exacto (ej. stock, products, customers)",
			raw,
		)
	}
	return nil
}

func isValidField(name string) bool {
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return len(name) > 0
}

func isValidOrderBy(s string) bool {
	parts := strings.Fields(s)
	if len(parts) == 0 || len(parts) > 2 || !isValidField(parts[0]) {
		return false
	}
	if len(parts) > 1 {
		dir := strings.ToLower(parts[1])
		return dir == "asc" || dir == "desc"
	}
	return true
}

func stripSensitive(record map[string]any) {
	for _, s := range []string{"password_hash", "password", "token", "secret", "api_key", "embedding"} {
		delete(record, s)
	}
}

// systemColumns are never writable by an agent.
var systemColumns = []string{"id", "organization_id", "created_at", "updated_at", "deleted_at", "created_by_id"}

// sanitizeMutationData drops system / generated / nested relation payloads so
// Create/Update never hit Postgres with e.g. a GENERATED ALWAYS column or a
// resolved FK object. Nested arrays are kept on create (order lines).
func (s *CRUDService) sanitizeMutationData(model string, data map[string]any, action string) map[string]any {
	if data == nil {
		return map[string]any{}
	}
	generated := map[string]struct{}{}
	if m, ok := s.cfg.Catalog.Lookup(s.resolve(model)); ok {
		for _, c := range m.GeneratedColumns {
			generated[c] = struct{}{}
		}
	}
	for _, k := range systemColumns {
		generated[k] = struct{}{}
	}
	jsonCols := s.jsonColumns(model)

	out := make(map[string]any, len(data))
	for k, v := range data {
		if _, skip := generated[k]; skip {
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			// Resolved relation chip / embed: not a writable column.
			continue
		case []any, []map[string]any:
			if action == "update" {
				if _, ok := jsonCols[k]; !ok {
					continue
				}
			}
			out[k] = s.normalize(model, k, val)
		case nil:
			continue
		default:
			out[k] = s.normalize(model, k, v)
		}
	}
	return out
}

func (s *CRUDService) normalize(model, key string, v any) any {
	if s.cfg.NormalizeValue != nil {
		return s.cfg.NormalizeValue(model, key, v)
	}
	return v
}

// encodeJSONColumns marshals nested slices/maps so Postgres jsonb columns
// don't get a composite `record` (SQLSTATE 42804). A Go string still binds as
// text and can 42804 against jsonb, so values go through `?::jsonb`.
func encodeJSONColumns(data map[string]any, jsonCols map[string]struct{}) {
	for k, v := range data {
		if !shouldJSONEncode(k, v, jsonCols) {
			continue
		}
		switch v.(type) {
		case string, []byte, json.RawMessage:
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		data[k] = gorm.Expr("?::jsonb", string(b))
	}
}

func shouldJSONEncode(k string, v any, jsonCols map[string]struct{}) bool {
	if _, ok := jsonCols[k]; ok {
		return true
	}
	switch k {
	case "lines", "items", "line_items", "details", "order_items", "product_lines", "entries":
		switch v.(type) {
		case []any, []map[string]any:
			return true
		}
	}
	return false
}
