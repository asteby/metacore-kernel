// Package vector exposes the kernel's pluggable vector-store + embedding
// primitives. Apps that need semantic search wire them via host.NewApp:
//
//	app := host.NewApp(host.AppConfig{
//	    DB:                db,
//	    EnableVectorStore: true, // requires pgvector/pgvector image
//	})
//	app.VectorStore.Search("products", queryEmbedding, 10, nil)
//
// Implementations (PGStore, RemoteEmbedder) live in their own files so apps
// can swap them — e.g. an in-memory store for tests, a Cohere/OpenAI
// embedder for production. The interfaces here are stable.
package vector

import "time"

// Point is a single vector + payload pair returned by a Store.
type Point struct {
	ID        string                 `json:"id"`
	Vector    []float32              `json:"vector,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	Timestamp time.Time              `json:"timestamp,omitempty"`
}

// Store is the framework-agnostic vector database surface. Collection names
// map to backend-specific concepts (Postgres tables, Qdrant collections,
// Pinecone indexes); the interface stays identical.
type Store interface {
	// EnsureCollection makes sure the backend is ready to host vectors of
	// the given size for the given collection. For PGStore this validates
	// the `vector` extension is enabled; remote backends typically create
	// the collection on demand.
	EnsureCollection(name string, vectorSize int) error

	// UpsertPoints adds or updates points in the collection.
	UpsertPoints(collection string, points []Point) error

	// Search returns the nearest neighbours of `vector` in `collection`.
	// `filter` is a Qdrant-style map (`{"must": [{"match": {...}}]}`)
	// or a flat `{"key": "value"}` map for equality filters; both are
	// supported so apps can migrate between backends without rewrites.
	Search(collection string, vector []float32, limit int, filter map[string]any) ([]Point, error)

	// DeletePoints removes points matching the filter. PGStore implements
	// this as `UPDATE ... SET embedding = NULL` so the underlying records
	// stay intact (regular CRUD owns deletion).
	DeletePoints(collection string, filter map[string]any) error
}

// Embedder turns text into a vector. Apps use it to vectorize records before
// upsert; SemanticPipeline implementations call it on every write.
type Embedder interface {
	// GenerateEmbedding embeds a single text into a vector.
	GenerateEmbedding(text string) ([]float32, error)

	// GenerateBatchEmbeddings embeds multiple texts. Implementations may
	// batch under the hood or fall back to per-text calls.
	GenerateBatchEmbeddings(texts []string) ([][]float32, error)
}
