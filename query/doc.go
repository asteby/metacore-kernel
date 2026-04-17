// Package query builds filtered, sorted, searchable, paginated GORM queries
// from HTTP-style request parameters (page, per_page, sortBy, order, search,
// f_<column>=op:value).
//
// The package is intentionally a thin, transport-agnostic wrapper around
// *gorm.DB. It presumes:
//
//   - the target model has already produced a TableMetadata via
//     modelbase.ModelDefiner (used for column whitelisting and search column
//     discovery);
//   - the caller has produced a *gorm.DB rooted at the table/model it wants
//     to query (builder does not know which model you are listing).
//
// App-specific concerns (branch scoping, fiscal JSON traversal, addon
// column injection, relation search) are explicitly NOT in the kernel.
// Apps layer those on by calling builder.Apply first and then applying
// their own .Where / .Joins clauses on the returned *gorm.DB.
//
// # Quick start
//
//	meta := modelbase.MustGet("invoices").DefineTable()
//	qb := query.New(&meta)
//
//	params, err := query.ParseFiber(c)
//	if err != nil { return err }
//
//	q := qb.Apply(db.Model(&Invoice{}), params)
//
//	total, err := qb.Count(q, params)
//	if err != nil { return err }
//
//	var rows []Invoice
//	if err := qb.Paginate(q, params).Find(&rows).Error; err != nil {
//	    return err
//	}
//
//	pageMeta := qb.PageMeta(total, params)
//
// # Parameters accepted
//
//	page              int     1-indexed, default 1
//	per_page          int     default 15, clamped to [1, 200]
//	sortBy            string  must match a whitelisted ColumnDef.Key
//	order             string  asc | desc, default desc
//	search            string  matched against meta.SearchColumns via ILIKE
//	f_<col>=<op>:<v>  filter: op ∈ {eq, ilike, in, gte, lte, range}
//	f_<col>=<v>       shorthand for eq
//
// # Filter operators
//
//	eq      f_status=eq:active          WHERE status = 'active'
//	ilike   f_name=ilike:widget         WHERE name ILIKE '%widget%'
//	in      f_status=in:a,b,c           WHERE status IN ('a','b','c')
//	gte     f_amount=gte:100            WHERE amount >= 100
//	lte     f_amount=lte:100            WHERE amount <= 100
//	range   f_created_at=range:a|b      WHERE created_at BETWEEN 'a' AND 'b'
//
// # Security
//
// Column identifiers are whitelisted against TableMetadata.Columns and
// additionally validated against the regex [a-zA-Z_][a-zA-Z0-9_]* before any
// raw-SQL interpolation. Unknown sort columns and unknown filter keys are
// dropped silently so that a malicious client cannot probe for column
// existence via error responses.
package query
