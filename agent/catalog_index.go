package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// CatalogIndex is the semantic catalog of a host's models: the typed view and
// ERP-Card of every model plus a retriever that maps a user request onto the
// few models it is about. It rebuilds itself from the ModelCatalog when the
// catalog changes (checked at most every RefreshInterval). Safe for
// concurrent use.
type CatalogIndex struct {
	cat  ModelCatalog
	opts CatalogOptions

	mu      sync.RWMutex
	checked time.Time
	version string
	models  []CatalogModel
	cards   []string
	byName  map[string]int
	ret     Retriever
}

// CatalogOptions tune a CatalogIndex. Every field is optional.
type CatalogOptions struct {
	// Labels localizes field and enum labels (ops' field labeler).
	Labels CatalogLabeler
	// Synonyms adds curated names per model name ("stock": {"inventario"}).
	Synonyms map[string][]string
	// Retriever replaces the default retriever (lexical, or hybrid when
	// Embedder is set).
	Retriever Retriever
	// Embedder enables hybrid retrieval: lexical BM25 fused with the cosine
	// ranking of the card embeddings (reciprocal rank fusion).
	Embedder Embedder
	// MinSimilarity is the cosine a card needs to enter the semantic ranking
	// (default 0.45), so an unrelated message retrieves nothing.
	MinSimilarity float64
	// RefreshInterval is how often the catalog is re-read to detect changes
	// (default one minute).
	RefreshInterval time.Duration
	// CardFields caps the fields of the cards catalog_search and the prefetch
	// show (default 18; describe_model always shows every field).
	CardFields int
}

// DefaultCatalogCardFields is the field cap of search/prefetch cards.
const DefaultCatalogCardFields = 18

// NewCatalogIndex builds an index over cat. The first use reads the catalog.
func NewCatalogIndex(cat ModelCatalog, opts CatalogOptions) *CatalogIndex {
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = time.Minute
	}
	if opts.CardFields <= 0 {
		opts.CardFields = DefaultCatalogCardFields
	}
	if opts.MinSimilarity <= 0 {
		opts.MinSimilarity = 0.45
	}
	return &CatalogIndex{cat: cat, opts: opts}
}

// Invalidate forces the next use to re-read the catalog.
func (ix *CatalogIndex) Invalidate() {
	ix.mu.Lock()
	ix.checked = time.Time{}
	ix.mu.Unlock()
}

// Version is a hash of every card: it changes exactly when the rendered
// catalog changes.
func (ix *CatalogIndex) Version() string {
	ix.refresh(context.Background())
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.version
}

// Models returns the catalog models sorted by name.
func (ix *CatalogIndex) Models() []CatalogModel {
	ix.refresh(context.Background())
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return append([]CatalogModel(nil), ix.models...)
}

// Model returns the catalog model of a (resolved or raw) key.
func (ix *CatalogIndex) Model(key string) (CatalogModel, bool) {
	ix.refresh(context.Background())
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if i, ok := ix.lookupLocked(key); ok {
		return ix.models[i], true
	}
	return CatalogModel{}, false
}

func (ix *CatalogIndex) lookupLocked(key string) (int, bool) {
	if i, ok := ix.byName[key]; ok {
		return i, true
	}
	if ix.cat != nil {
		if r := ResolveModelKey(ix.cat, key); r != "" {
			if i, ok := ix.byName[r]; ok {
				return i, true
			}
		}
	}
	return 0, false
}

// refresh re-reads the catalog when the refresh interval has passed and
// rebuilds models, cards and retriever if anything changed.
func (ix *CatalogIndex) refresh(ctx context.Context) {
	ix.mu.RLock()
	fresh := !ix.checked.IsZero() && time.Since(ix.checked) < ix.opts.RefreshInterval
	ix.mu.RUnlock()
	if fresh {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.checked.IsZero() && time.Since(ix.checked) < ix.opts.RefreshInterval {
		return
	}
	ix.checked = time.Now()
	models := ix.buildModels()
	cards := make([]string, len(models))
	h := sha256.New()
	for i, m := range models {
		cards[i] = RenderCard(m, CardOptions{})
		h.Write([]byte(cards[i]))
		h.Write([]byte{0})
		h.Write([]byte(strings.Join(m.Synonyms, ",") + "\x00" + m.Description + "\x00"))
	}
	version := hex.EncodeToString(h.Sum(nil))[:16]
	if version == ix.version && ix.ret != nil {
		return
	}
	ix.version, ix.models, ix.cards = version, models, cards
	ix.byName = make(map[string]int, len(models)*2)
	for i, m := range models {
		for _, k := range []string{m.Name, m.Key, m.ModelKey} {
			if k == "" {
				continue
			}
			if _, dup := ix.byName[k]; !dup {
				ix.byName[k] = i
			}
		}
	}
	ret := ix.opts.Retriever
	if ret == nil {
		if ix.opts.Embedder != nil {
			ret = NewHybridRetriever(ix.opts.Embedder, ix.opts.MinSimilarity)
		} else {
			ret = NewLexicalRetriever()
		}
	}
	docs := make([]CatalogDoc, len(models))
	for i, m := range models {
		docs[i] = CatalogDoc{Model: m, Card: cards[i]}
	}
	if err := ret.Index(ctx, docs); err != nil {
		log.Printf("agent catalog: retriever index failed, using lexical: %v", err)
		ret = NewLexicalRetriever()
		_ = ret.Index(ctx, docs)
	}
	ix.ret = ret
}

// buildModels reads the host catalog, deduped by model name and sorted.
func (ix *CatalogIndex) buildModels() []CatalogModel {
	if ix.cat == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []CatalogModel
	for _, meta := range ix.cat.All() {
		cm := BuildCatalogModel(ix.cat, meta, ix.opts.Labels)
		key := strings.ToLower(cm.Name)
		if cm.Name == "" || seen[key] || strings.Contains(cm.Name, ".") {
			continue
		}
		seen[key] = true
		if extra := ix.opts.Synonyms[cm.Name]; len(extra) > 0 {
			cm.Synonyms = mergeSynonyms(extra, cm.Synonyms, cm)
		}
		out = append(out, cm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func mergeSynonyms(first, rest []string, cm CatalogModel) []string {
	seen := map[string]bool{foldText(cm.Name): true, foldText(cm.Label): true}
	var out []string
	for _, s := range append(append([]string(nil), first...), rest...) {
		f := foldText(strings.TrimSpace(s))
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// CatalogHit is one retrieved model.
type CatalogHit struct {
	Model CatalogModel
	Score float64
}

// Search returns up to k models relevant to query, best first. allow, when
// set, filters out models the caller may not see (checked before cutting at
// k, so hidden models never use a slot).
func (ix *CatalogIndex) Search(ctx context.Context, query string, k int, allow func(CatalogModel) bool) []CatalogHit {
	ix.refresh(ctx)
	ix.mu.RLock()
	ret := ix.ret
	ix.mu.RUnlock()
	if ret == nil || strings.TrimSpace(query) == "" || k <= 0 {
		return nil
	}
	ranked, err := ret.Retrieve(ctx, query, k*4+8)
	if err != nil {
		log.Printf("agent catalog: retrieve failed: %v", err)
		return nil
	}
	var out []CatalogHit
	for _, h := range ranked {
		if allow != nil && !allow(h.Model) {
			continue
		}
		out = append(out, h)
		if len(out) == k {
			break
		}
	}
	return out
}

// CatalogDoc is one model handed to a Retriever: its typed view and its full
// ERP-Card text.
type CatalogDoc struct {
	Model CatalogModel
	Card  string
}

// Retriever ranks catalog models for a request. Index is called whenever
// the catalog changes; Retrieve must be safe for concurrent use.
type Retriever interface {
	Index(ctx context.Context, docs []CatalogDoc) error
	Retrieve(ctx context.Context, query string, k int) ([]CatalogHit, error)
}

// ---------------------------------------------------------------------------
// Lexical retriever: BM25 over weighted name/label/synonym/field terms.
// ---------------------------------------------------------------------------

// LexicalRetriever is the default retriever: accent-insensitive Spanish
// tokenization, a light stemmer and BM25 scoring where the model name, label
// and synonyms weigh more than field names and labels.
type LexicalRetriever struct {
	mu    sync.RWMutex
	docs  []CatalogDoc
	tf    []map[string]float64
	lens  []float64
	avg   float64
	df    map[string]int
	exact []map[string]bool
}

// NewLexicalRetriever returns an empty lexical retriever.
func NewLexicalRetriever() *LexicalRetriever { return &LexicalRetriever{} }

// Term weights of the lexical document.
const (
	lexWeightName    = 3.0
	lexWeightLabel   = 3.0
	lexWeightSynonym = 2.5
	lexWeightField   = 0.6
	lexWeightDesc    = 0.8
)

// Index implements Retriever.
func (r *LexicalRetriever) Index(_ context.Context, docs []CatalogDoc) error {
	tf := make([]map[string]float64, len(docs))
	lens := make([]float64, len(docs))
	exact := make([]map[string]bool, len(docs))
	df := map[string]int{}
	total := 0.0
	for i, d := range docs {
		m := map[string]float64{}
		addTerms := func(s string, w float64) {
			for _, t := range catalogStems(s) {
				m[t] += w
			}
		}
		addTerms(d.Model.Name, lexWeightName)
		if d.Model.ModelKey != "" && d.Model.ModelKey != d.Model.Name {
			addTerms(d.Model.ModelKey, lexWeightName/2)
		}
		addTerms(d.Model.Label, lexWeightLabel)
		for _, s := range d.Model.Synonyms {
			addTerms(s, lexWeightSynonym)
		}
		addTerms(d.Model.Description, lexWeightDesc)
		for _, f := range d.Model.Fields {
			addTerms(f.Name, lexWeightField)
			addTerms(f.Label, lexWeightField)
		}
		for t := range m {
			df[t]++
		}
		l := 0.0
		for _, w := range m {
			l += w
		}
		tf[i], lens[i] = m, l
		total += l
		ex := map[string]bool{}
		for _, s := range append([]string{d.Model.Name, d.Model.Key, d.Model.ModelKey, d.Model.Label}, d.Model.Synonyms...) {
			if s != "" {
				ex[foldText(strings.TrimSpace(s))] = true
			}
		}
		exact[i] = ex
	}
	avg := 1.0
	if len(docs) > 0 && total > 0 {
		avg = total / float64(len(docs))
	}
	r.mu.Lock()
	r.docs, r.tf, r.lens, r.avg, r.df, r.exact = docs, tf, lens, avg, df, exact
	r.mu.Unlock()
	return nil
}

// Retrieve implements Retriever. Only models sharing a term with the query
// are returned.
func (r *LexicalRetriever) Retrieve(_ context.Context, query string, k int) ([]CatalogHit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	qterms := catalogStems(query)
	if len(qterms) == 0 || len(r.docs) == 0 {
		return nil, nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(r.docs))
	uniq := map[string]bool{}
	for _, t := range qterms {
		uniq[t] = true
	}
	foldedQuery := foldText(strings.TrimSpace(query))
	var hits []CatalogHit
	for i, d := range r.docs {
		score := 0.0
		for t := range uniq {
			f := r.tf[i][t]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(r.df[t])+0.5)/(float64(r.df[t])+0.5))
			score += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*r.lens[i]/r.avg))
		}
		if score <= 0 {
			continue
		}
		if r.exact[i][foldedQuery] {
			score *= 1.5
		}
		hits = append(hits, CatalogHit{Model: d.Model, Score: score})
	}
	sortHits(hits)
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func sortHits(hits []CatalogHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Model.Name < hits[j].Model.Name
	})
}

// ---------------------------------------------------------------------------
// Hybrid retriever: lexical + embeddings, fused with reciprocal rank fusion.
// ---------------------------------------------------------------------------

// Embedder turns texts into embedding vectors (one per text, same order).
// Hosts plug their model here (OllamaEmbedder for bge-m3).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HybridRetriever fuses the lexical ranking with the cosine ranking of card
// embeddings (RRF, k=60). Card embeddings are cached in memory by the hash of
// the card text, so a catalog change only embeds the cards that changed. If
// the embedder fails the lexical ranking is returned alone.
type HybridRetriever struct {
	lex      *LexicalRetriever
	embedder Embedder
	minSim   float64

	mu      sync.Mutex
	docs    []CatalogDoc
	hashes  []string
	vectors map[string][]float32 // card hash -> embedding
	queries map[string][]float32 // query -> embedding (bounded)
}

// rrfK is the reciprocal rank fusion constant.
const rrfK = 60.0

// NewHybridRetriever builds a hybrid retriever over embedder. minSim is the
// cosine a card needs to enter the semantic ranking (<= 0 means 0.45).
func NewHybridRetriever(embedder Embedder, minSim float64) *HybridRetriever {
	if minSim <= 0 {
		minSim = 0.45
	}
	return &HybridRetriever{lex: NewLexicalRetriever(), embedder: embedder, minSim: minSim, vectors: map[string][]float32{}, queries: map[string][]float32{}}
}

func cardHash(card string) string {
	sum := sha256.Sum256([]byte(card))
	return hex.EncodeToString(sum[:])
}

// Index implements Retriever. Embeddings are computed lazily, on the first
// Retrieve, so building the index never waits on the network.
func (h *HybridRetriever) Index(ctx context.Context, docs []CatalogDoc) error {
	if err := h.lex.Index(ctx, docs); err != nil {
		return err
	}
	hashes := make([]string, len(docs))
	for i, d := range docs {
		hashes[i] = cardHash(embedText(d))
	}
	h.mu.Lock()
	h.docs, h.hashes = docs, hashes
	h.mu.Unlock()
	return nil
}

// embedText is what gets embedded for a model: its card plus synonyms and
// description (the card alone may omit them).
func embedText(d CatalogDoc) string {
	s := d.Card
	if len(d.Model.Synonyms) > 0 {
		s += "\nSinónimos: " + strings.Join(d.Model.Synonyms, ", ")
	}
	if d.Model.Description != "" {
		s += "\n" + d.Model.Description
	}
	return s
}

// ensureVectors embeds the cards missing from the cache.
func (h *HybridRetriever) ensureVectors(ctx context.Context) ([]CatalogDoc, [][]float32, error) {
	h.mu.Lock()
	docs, hashes := h.docs, h.hashes
	var missing []int
	for i, hs := range hashes {
		if _, ok := h.vectors[hs]; !ok {
			missing = append(missing, i)
		}
	}
	h.mu.Unlock()
	if len(missing) > 0 {
		texts := make([]string, len(missing))
		for j, i := range missing {
			texts[j] = embedText(docs[i])
		}
		vecs, err := h.embedder.Embed(ctx, texts)
		if err != nil {
			return nil, nil, err
		}
		if len(vecs) != len(texts) {
			return nil, nil, errEmbedCount
		}
		h.mu.Lock()
		for j, i := range missing {
			h.vectors[hashes[i]] = vecs[j]
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([][]float32, len(docs))
	for i, hs := range hashes {
		out[i] = h.vectors[hs]
	}
	return docs, out, nil
}

type embedError string

func (e embedError) Error() string { return string(e) }

const errEmbedCount = embedError("agent catalog: embedder returned a different number of vectors")

func (h *HybridRetriever) queryVector(ctx context.Context, query string) ([]float32, error) {
	key := foldText(strings.TrimSpace(query))
	h.mu.Lock()
	if v, ok := h.queries[key]; ok {
		h.mu.Unlock()
		return v, nil
	}
	h.mu.Unlock()
	vecs, err := h.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errEmbedCount
	}
	h.mu.Lock()
	if len(h.queries) >= 512 {
		h.queries = map[string][]float32{}
	}
	h.queries[key] = vecs[0]
	h.mu.Unlock()
	return vecs[0], nil
}

// Retrieve implements Retriever.
func (h *HybridRetriever) Retrieve(ctx context.Context, query string, k int) ([]CatalogHit, error) {
	lex, err := h.lex.Retrieve(ctx, query, 0)
	if err != nil {
		return nil, err
	}
	docs, vecs, err := h.ensureVectors(ctx)
	var qv []float32
	if err == nil {
		qv, err = h.queryVector(ctx, query)
	}
	if err != nil {
		log.Printf("agent catalog: embeddings unavailable, lexical only: %v", err)
		if k > 0 && len(lex) > k {
			lex = lex[:k]
		}
		return lex, nil
	}
	var sem []CatalogHit
	for i, v := range vecs {
		if s := cosine(qv, v); s >= h.minSim {
			sem = append(sem, CatalogHit{Model: docs[i].Model, Score: s})
		}
	}
	sortHits(sem)
	if len(sem) > 10 {
		sem = sem[:10]
	}
	fused := FuseRRF(lex, sem)
	if k > 0 && len(fused) > k {
		fused = fused[:k]
	}
	return fused, nil
}

// FuseRRF merges rankings with reciprocal rank fusion: each model scores
// sum(1 / (60 + rank)) over the rankings it appears in.
func FuseRRF(rankings ...[]CatalogHit) []CatalogHit {
	score := map[string]float64{}
	model := map[string]CatalogModel{}
	for _, r := range rankings {
		for i, h := range r {
			score[h.Model.Name] += 1 / (rrfK + float64(i+1))
			model[h.Model.Name] = h.Model
		}
	}
	out := make([]CatalogHit, 0, len(score))
	for name, s := range score {
		out = append(out, CatalogHit{Model: model[name], Score: s})
	}
	sortHits(out)
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
