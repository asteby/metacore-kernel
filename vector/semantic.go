package vector

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EmbeddingsTable is the polymorphic table the Pipeline reads and writes: one
// row per (entity_type, entity_id), so a model becomes searchable by declaring
// a ModelConfig, with no per-table schema change.
const EmbeddingsTable = "embeddings"

// EmbeddingDim is the dimension of the embeddings column the pipeline expects
// (bge-m3). Hosts create the table with MigrateSQL.
const EmbeddingDim = 1024

// MigrateSQL creates the polymorphic embeddings table and its indexes. It is
// idempotent and requires the pgvector extension.
const MigrateSQL = `
CREATE TABLE IF NOT EXISTS embeddings (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	organization_id uuid NOT NULL,
	entity_type varchar(100) NOT NULL,
	entity_id uuid NOT NULL,
	embedding vector(1024) NOT NULL,
	fields_hash varchar(64),
	model_version varchar(50) DEFAULT 'bge-m3-v1',
	created_at timestamptz DEFAULT now(),
	updated_at timestamptz DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_emb_entity_unique ON embeddings (entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_emb_org_type ON embeddings (organization_id, entity_type);
`

// Embedding is the row model of EmbeddingsTable.
type Embedding struct {
	ID             uuid.UUID       `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	OrganizationID uuid.UUID       `gorm:"type:uuid;not null;index:idx_emb_org_type"`
	EntityType     string          `gorm:"size:100;not null;index:idx_emb_org_type;uniqueIndex:idx_emb_entity_unique"`
	EntityID       uuid.UUID       `gorm:"type:uuid;not null;uniqueIndex:idx_emb_entity_unique"`
	Embedding      pgvector.Vector `gorm:"type:vector(1024);not null"`
	FieldsHash     string          `gorm:"size:64"`
	ModelVersion   string          `gorm:"size:50;default:'bge-m3-v1'"`
	CreatedAt      time.Time       `gorm:"autoCreateTime"`
	UpdatedAt      time.Time       `gorm:"autoUpdateTime"`
}

// TableName binds Embedding to EmbeddingsTable.
func (Embedding) TableName() string { return EmbeddingsTable }

// RelationField resolves a foreign key to a readable value for richer text.
type RelationField struct {
	ForeignKey   string // e.g. "customer_id"
	Table        string // e.g. "customers"
	DisplayField string // e.g. "name"
	Label        string // prefix in the text: "customer"
}

// ModelConfig declares how one model is vectorized.
type ModelConfig struct {
	Enabled   bool
	Fields    []string // columns concatenated into the embedding text
	Relations []RelationField
}

// ModelSource is what the Pipeline needs to know about the host's models.
type ModelSource interface {
	// VectorConfig returns the vectorization config of a model.
	VectorConfig(modelKey string) (ModelConfig, bool)
	// AllVectorConfigs lists every vectorizable model.
	AllVectorConfigs() map[string]ModelConfig
	// TableName resolves a model key to its SQL table ("" when unknown).
	TableName(modelKey string) string
}

// Pipeline generates and stores embeddings for any model with a ModelConfig
// and answers similarity queries over them. It implements the agent runtime's
// SemanticIndex port.
type Pipeline struct {
	db           *gorm.DB
	embedder     Embedder
	source       ModelSource
	queue        chan embeddingJob
	modelVersion string
}

type embeddingJob struct {
	OrgID      uuid.UUID
	EntityType string
	EntityID   uuid.UUID
	Text       string
}

// PipelineOptions tunes a Pipeline.
type PipelineOptions struct {
	// Workers is the number of background embedders (default 3). Zero or
	// negative with NoWorkers set starts none (tests drive ProcessJob).
	Workers int
	// NoWorkers disables background workers.
	NoWorkers bool
	// QueueSize bounds the pending jobs (default 1000).
	QueueSize int
	// ModelVersion tags stored vectors (default "bge-m3-v1").
	ModelVersion string
}

// NewPipeline builds a Pipeline and starts its workers.
func NewPipeline(db *gorm.DB, embedder Embedder, source ModelSource, opts PipelineOptions) *Pipeline {
	if opts.Workers <= 0 {
		opts.Workers = 3
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1000
	}
	if opts.ModelVersion == "" {
		opts.ModelVersion = "bge-m3-v1"
	}
	p := &Pipeline{
		db:           db,
		embedder:     embedder,
		source:       source,
		queue:        make(chan embeddingJob, opts.QueueSize),
		modelVersion: opts.ModelVersion,
	}
	if !opts.NoWorkers {
		for range opts.Workers {
			go p.worker()
		}
	}
	return p
}

func (p *Pipeline) worker() {
	for job := range p.queue {
		p.processJob(job)
	}
}

// Close stops the workers after the queue drains. The Pipeline must not be
// used afterwards.
func (p *Pipeline) Close() { close(p.queue) }

func (p *Pipeline) enqueue(job embeddingJob) bool {
	select {
	case p.queue <- job:
		return true
	default:
		log.Printf("vector pipeline: queue full, dropping %s/%s", job.EntityType, job.EntityID)
		return false
	}
}

func (p *Pipeline) processJob(job embeddingJob) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(job.Text)))

	var existing Embedding
	if err := p.db.Where("entity_type = ? AND entity_id = ? AND fields_hash = ?",
		job.EntityType, job.EntityID, hash).First(&existing).Error; err == nil {
		return // already embedded with the same content
	}

	vec, err := p.embedder.GenerateEmbedding(job.Text)
	if err != nil {
		log.Printf("vector pipeline: embedding error %s/%s: %v", job.EntityType, job.EntityID, err)
		return
	}

	emb := Embedding{
		ID:             uuid.New(),
		OrganizationID: job.OrgID,
		EntityType:     job.EntityType,
		EntityID:       job.EntityID,
		Embedding:      pgvector.NewVector(vec),
		FieldsHash:     hash,
		ModelVersion:   p.modelVersion,
	}
	if err := p.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"embedding", "fields_hash", "model_version", "updated_at"}),
	}).Create(&emb).Error; err != nil {
		log.Printf("vector pipeline: store error %s/%s: %v", job.EntityType, job.EntityID, err)
	}
}

// EnqueueRecord (re)indexes one record of a vectorizable model. Called after
// create/update; it never blocks (a full queue drops the job).
func (p *Pipeline) EnqueueRecord(modelKey string, recordID uuid.UUID) {
	cfg, ok := p.source.VectorConfig(modelKey)
	if !ok || !cfg.Enabled {
		return
	}
	table := p.source.TableName(modelKey)
	if table == "" {
		return
	}
	var record map[string]any
	if err := p.db.Table(table).Where("id = ?", recordID).Take(&record).Error; err != nil {
		return
	}
	text := p.BuildText(cfg, record)
	if text == "" {
		return
	}
	p.enqueue(embeddingJob{OrgID: uuidOf(record["organization_id"]), EntityType: modelKey, EntityID: recordID, Text: text})
}

// BuildText constructs the embedding text from a config and a record.
func (p *Pipeline) BuildText(cfg ModelConfig, record map[string]any) string {
	var parts []string
	for _, field := range cfg.Fields {
		if v, ok := record[field]; ok && v != nil {
			if s := fmt.Sprintf("%v", v); s != "" && s != "<nil>" {
				parts = append(parts, fmt.Sprintf("%s: %s", field, s))
			}
		}
	}
	for _, rel := range cfg.Relations {
		fk, ok := record[rel.ForeignKey]
		if !ok || fk == nil {
			continue
		}
		var display string
		p.db.Table(rel.Table).Where("id = ?", fk).Pluck(rel.DisplayField, &display)
		if display != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", rel.Label, display))
		}
	}
	return strings.Join(parts, ", ")
}

// VectorSearch finds the records of a model nearest to query, scoped to orgID
// (cosine similarity over the embeddings table).
func (p *Pipeline) VectorSearch(modelKey, query string, orgID uuid.UUID, limit int) ([]SearchResult, error) {
	cfg, ok := p.source.VectorConfig(modelKey)
	if !ok || !cfg.Enabled {
		return nil, fmt.Errorf("model '%s' is not vectorizable", modelKey)
	}
	if orgID == uuid.Nil {
		return nil, errors.New("organization required for vector search")
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	qv, err := p.embedder.GenerateEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("embedding generation failed: %w", err)
	}

	var hits []struct {
		EntityID   uuid.UUID `gorm:"column:entity_id"`
		Similarity float64   `gorm:"column:similarity"`
	}
	if err := p.db.Table(EmbeddingsTable).
		Select("entity_id, 1 - (embedding <=> ?) as similarity", pgvector.NewVector(qv)).
		Where("entity_type = ? AND organization_id = ?", modelKey, orgID).
		Order("similarity DESC").Limit(limit).Scan(&hits).Error; err != nil {
		return nil, fmt.Errorf("vector search failed: %w", err)
	}
	if len(hits) == 0 {
		return []SearchResult{}, nil
	}

	ids := make([]uuid.UUID, len(hits))
	sim := make(map[string]float64, len(hits))
	for i, h := range hits {
		ids[i] = h.EntityID
		sim[h.EntityID.String()] = h.Similarity
	}
	table := p.source.TableName(modelKey)
	if table == "" {
		return nil, fmt.Errorf("model '%s' has no table", modelKey)
	}
	var records []map[string]any
	if err := p.db.Table(table).Where("id IN ? AND organization_id = ?", ids, orgID).Find(&records).Error; err != nil {
		return nil, fmt.Errorf("record lookup failed: %w", err)
	}
	out := make([]SearchResult, 0, len(records))
	for _, rec := range records {
		id := fmt.Sprintf("%v", rec["id"])
		delete(rec, "embedding")
		delete(rec, "password_hash")
		out = append(out, SearchResult{ID: id, Similarity: sim[id], Data: rec})
	}
	// Keep the similarity order the index returned.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Similarity > out[j-1].Similarity; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// BackfillModel queues the records of a model that have no embedding yet
// (up to 500 per call) and returns how many it queued.
func (p *Pipeline) BackfillModel(modelKey string) int {
	cfg, ok := p.source.VectorConfig(modelKey)
	if !ok || !cfg.Enabled {
		return 0
	}
	table := p.source.TableName(modelKey)
	if table == "" {
		return 0
	}
	var records []map[string]any
	sub := p.db.Table(EmbeddingsTable).Select("entity_id").Where("entity_type = ?", modelKey)
	p.db.Table(table).Where("id NOT IN (?)", sub).Limit(500).Find(&records)

	n := 0
	for _, rec := range records {
		text := p.BuildText(cfg, rec)
		id := uuidOf(rec["id"])
		if text == "" || id == uuid.Nil {
			continue
		}
		if p.enqueue(embeddingJob{OrgID: uuidOf(rec["organization_id"]), EntityType: modelKey, EntityID: id, Text: text}) {
			n++
		}
	}
	return n
}

// BackfillAll backfills every vectorizable model.
func (p *Pipeline) BackfillAll() map[string]int {
	out := map[string]int{}
	for key := range p.source.AllVectorConfigs() {
		if n := p.BackfillModel(key); n > 0 {
			out[key] = n
		}
	}
	return out
}

// VectorizableModels lists the models with vectorization enabled.
func (p *Pipeline) VectorizableModels() []string {
	cfgs := p.source.AllVectorConfigs()
	keys := make([]string, 0, len(cfgs))
	for k, c := range cfgs {
		if c.Enabled {
			keys = append(keys, k)
		}
	}
	return keys
}

// ReembedAll re-queues records embedded with an older model version (up to
// 1000 per call) and switches new vectors to newModelVersion.
func (p *Pipeline) ReembedAll(newModelVersion string) int {
	p.modelVersion = newModelVersion
	var outdated []Embedding
	p.db.Where("model_version != ?", newModelVersion).Limit(1000).Find(&outdated)
	for _, e := range outdated {
		p.EnqueueRecord(e.EntityType, e.EntityID)
	}
	return len(outdated)
}

func uuidOf(v any) uuid.UUID {
	switch t := v.(type) {
	case uuid.UUID:
		return t
	case string:
		id, _ := uuid.Parse(t)
		return id
	case []byte:
		id, _ := uuid.ParseBytes(t)
		return id
	}
	return uuid.Nil
}
