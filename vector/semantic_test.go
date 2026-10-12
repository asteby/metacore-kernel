package vector

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakeSource struct {
	cfg   map[string]ModelConfig
	table map[string]string
}

func (f fakeSource) VectorConfig(k string) (ModelConfig, bool) { c, ok := f.cfg[k]; return c, ok }
func (f fakeSource) AllVectorConfigs() map[string]ModelConfig  { return f.cfg }
func (f fakeSource) TableName(k string) string                 { return f.table[k] }

type countEmbedder struct{ calls int }

func (c *countEmbedder) GenerateEmbedding(string) ([]float32, error) {
	c.calls++
	return []float32{1, 0, 0}, nil
}
func (c *countEmbedder) GenerateBatchEmbeddings(t []string) ([][]float32, error) {
	out := make([][]float32, len(t))
	for i := range t {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func newTestPipeline(t *testing.T) (*Pipeline, *gorm.DB, *countEmbedder) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE customers (id text PRIMARY KEY, name text)`,
		`CREATE TABLE products (id text PRIMARY KEY, organization_id text, name text, notes text, customer_id text)`,
		`CREATE TABLE embeddings (id text PRIMARY KEY, organization_id text, entity_type text, entity_id text, embedding text, fields_hash text, model_version text, created_at datetime, updated_at datetime)`,
		`CREATE UNIQUE INDEX u ON embeddings (entity_type, entity_id)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	src := fakeSource{
		cfg: map[string]ModelConfig{
			"products": {Enabled: true, Fields: []string{"name", "notes"}, Relations: []RelationField{{ForeignKey: "customer_id", Table: "customers", DisplayField: "name", Label: "customer"}}},
			"off":      {Enabled: false},
		},
		table: map[string]string{"products": "products"},
	}
	emb := &countEmbedder{}
	return NewPipeline(db, emb, src, PipelineOptions{NoWorkers: true}), db, emb
}

func TestBuildTextFieldsAndRelations(t *testing.T) {
	p, db, _ := newTestPipeline(t)
	cid := uuid.NewString()
	db.Exec(`INSERT INTO customers VALUES (?, 'Acme')`, cid)
	got := p.BuildText(p.source.(fakeSource).cfg["products"], map[string]any{"name": "Napa", "notes": nil, "customer_id": cid})
	if got != "name: Napa, customer: Acme" {
		t.Fatalf("got %q", got)
	}
	if p.BuildText(ModelConfig{Fields: []string{"x"}}, map[string]any{}) != "" {
		t.Fatal("empty record must give empty text")
	}
}

func drain(p *Pipeline) int {
	n := 0
	for {
		select {
		case job := <-p.queue:
			p.processJob(job)
			n++
		default:
			return n
		}
	}
}

func TestEnqueueRecordIndexesOnceAndSkipsUnchanged(t *testing.T) {
	p, db, emb := newTestPipeline(t)
	org, id := uuid.New(), uuid.New()
	db.Exec(`INSERT INTO products (id, organization_id, name) VALUES (?, ?, 'Napa')`, id.String(), org.String())

	p.EnqueueRecord("products", id)
	if drain(p) != 1 || emb.calls != 1 {
		t.Fatalf("first index: calls=%d", emb.calls)
	}
	p.EnqueueRecord("products", id)
	drain(p)
	if emb.calls != 1 {
		t.Fatalf("unchanged content must not re-embed, calls=%d", emb.calls)
	}
	db.Exec(`UPDATE products SET name='Napa fina' WHERE id=?`, id.String())
	p.EnqueueRecord("products", id)
	drain(p)
	if emb.calls != 2 {
		t.Fatalf("changed content must re-embed, calls=%d", emb.calls)
	}
	var n int64
	db.Table("embeddings").Count(&n)
	if n != 1 {
		t.Fatalf("upsert must keep one row, got %d", n)
	}
}

func TestEnqueueIgnoresDisabledAndUnknown(t *testing.T) {
	p, _, _ := newTestPipeline(t)
	p.EnqueueRecord("off", uuid.New())
	p.EnqueueRecord("nope", uuid.New())
	if drain(p) != 0 {
		t.Fatal("nothing should be queued")
	}
}

func TestBackfillQueuesOnlyMissing(t *testing.T) {
	p, db, _ := newTestPipeline(t)
	org := uuid.New()
	a, b := uuid.New(), uuid.New()
	db.Exec(`INSERT INTO products (id, organization_id, name) VALUES (?, ?, 'A'), (?, ?, 'B')`, a.String(), org.String(), b.String(), org.String())
	if got := p.BackfillAll(); got["products"] != 2 {
		t.Fatalf("%v", got)
	}
	drain(p)
	if got := p.BackfillModel("products"); got != 0 {
		t.Fatalf("already embedded, got %d", got)
	}
	if k := p.VectorizableModels(); len(k) != 1 || k[0] != "products" {
		t.Fatalf("%v", k)
	}
}

func TestVectorSearchGuards(t *testing.T) {
	p, _, _ := newTestPipeline(t)
	if _, err := p.VectorSearch("off", "q", uuid.New(), 5); err == nil {
		t.Fatal("non-vectorizable model must error")
	}
	if _, err := p.VectorSearch("products", "q", uuid.Nil, 5); err == nil {
		t.Fatal("missing org must error")
	}
}

func TestQueueFullDropsInsteadOfBlocking(t *testing.T) {
	db, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	p := NewPipeline(db, &countEmbedder{}, fakeSource{}, PipelineOptions{NoWorkers: true, QueueSize: 1})
	if !p.enqueue(embeddingJob{}) || p.enqueue(embeddingJob{}) {
		t.Fatal("second enqueue must be dropped when the queue is full")
	}
}
