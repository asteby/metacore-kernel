package vector

import (
	"fmt"
	"log"

	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
)

// PGStore implements Store on top of PostgreSQL + pgvector. The "collection"
// argument maps to a regular table that has an `embedding vector(N)` column
// and a string `id`. Apps add the column to their existing models — the
// store just runs UPDATE/SELECT against it.
type PGStore struct {
	db *gorm.DB
}

// NewPGStore builds a PGStore against an already-open *gorm.DB. Caller is
// responsible for `CREATE EXTENSION IF NOT EXISTS vector` (host.NewApp does
// this when EnableVectorStore is set).
func NewPGStore(db *gorm.DB) *PGStore { return &PGStore{db: db} }

// EnsureCollection verifies that the pgvector extension is loaded. It does
// not create the table — that lives with the model's regular AutoMigrate.
func (s *PGStore) EnsureCollection(name string, vectorSize int) error {
	var exists bool
	s.db.Raw("SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'vector')").Scan(&exists)
	if !exists {
		return fmt.Errorf("vector: pgvector extension is not enabled (collection=%s)", name)
	}
	return nil
}

// UpsertPoints writes the vector of every point into the `embedding` column
// of the matching row in `collection`. Points whose ID does not exist are
// silently skipped (GORM Update's behaviour). For batch performance the
// caller can chunk on its side.
func (s *PGStore) UpsertPoints(collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	for _, point := range points {
		v := pgvector.NewVector(point.Vector)
		if err := s.db.Table(collection).
			Where("id = ?", point.ID).
			Update("embedding", v).Error; err != nil {
			log.Printf("vector: upsert %s/%s: %v", collection, point.ID, err)
			return err
		}
	}
	return nil
}

// Search returns the rows nearest to `vector` ordered by cosine similarity
// (`1 - (embedding <=> ?)`). `filter` accepts either a flat map or a
// Qdrant-style `{must: [{match: {...}}]}` shape so apps can ride the same
// call across backends.
func (s *PGStore) Search(collection string, vector []float32, limit int, filter map[string]any) ([]Point, error) {
	var results []map[string]any
	searchVector := pgvector.NewVector(vector)

	query := s.db.Table(collection).Limit(limit)
	query = applyFilter(query, filter)

	query = query.Where("embedding IS NOT NULL").
		Select("*, 1 - (embedding <=> ?) as similarity", searchVector).
		Order("similarity DESC")

	if err := query.Scan(&results).Error; err != nil {
		return nil, err
	}

	points := make([]Point, 0, len(results))
	for _, r := range results {
		id := fmt.Sprintf("%v", r["id"])
		payload := make(map[string]any, len(r))
		for k, v := range r {
			if k == "embedding" || k == "similarity" {
				continue
			}
			payload[k] = v
		}
		payload["_similarity"] = r["similarity"]
		points = append(points, Point{ID: id, Payload: payload})
	}
	return points, nil
}

// DeletePoints sets `embedding = NULL` on every matching row. Apps that want
// to drop the underlying record use their normal CRUD path; this only
// removes points from semantic search consideration.
func (s *PGStore) DeletePoints(collection string, filter map[string]any) error {
	query := s.db.Table(collection)
	query = applyFilter(query, filter)
	return query.Update("embedding", nil).Error
}

// applyFilter handles both the flat `{key: value}` map and the Qdrant-style
// `{must: [{match: {...}}]}` shape.
func applyFilter(q *gorm.DB, filter map[string]any) *gorm.DB {
	if filter == nil {
		return q
	}
	if must, ok := filter["must"].([]map[string]any); ok {
		for _, cond := range must {
			for k, v := range cond {
				switch k {
				case "match":
					if matchMap, ok := v.(map[string]any); ok {
						for mk, mv := range matchMap {
							q = q.Where(fmt.Sprintf("%s = ?", mk), mv)
						}
					}
				case "has_id":
					if ids, ok := v.([]string); ok {
						q = q.Where("id IN ?", ids)
					}
				default:
					q = q.Where(fmt.Sprintf("%s = ?", k), v)
				}
			}
		}
		return q
	}
	for k, v := range filter {
		q = q.Where(fmt.Sprintf("%s = ?", k), v)
	}
	return q
}
