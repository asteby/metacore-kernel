package wasm

import (
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// capKind is the capability axis a relation reference is gated against.
// The walker emits read-capability refs for everything reachable from a
// `SELECT` (and for the read-only sources of a DML — UPDATE.FROM, DELETE.USING,
// MERGE source, INSERT.SELECT, RETURNING / WHERE subqueries, CTE bodies that
// are themselves SELECTs). DML targets emit a write-capability ref against
// the relation actually being mutated. A relation that needs both gates
// (e.g. a `RangeFunction` reached from within a mutation statement, where the
// function body may both read and write) emits two refs — one of each Cap.
type capKind string

const (
	capRead  capKind = "db:read"
	capWrite capKind = "db:write"
)

// relationRef is a parsed reference to a relation (table / view / subquery
// /set-returning function) pulled out of a SQL statement's AST. Schema is
// empty for bare names — the host knows those will be resolved by
// `SET LOCAL search_path` against the addon's own schema. Cap encodes whether
// the reference should be gated under `db:read` or `db:write`.
type relationRef struct {
	Schema string
	Table  string
	Cap    capKind
}

// extractRelations parses a SQL statement and returns every relation
// reference reachable from the AST, gated under `db:read`. This is the
// `db_query` entry point: SELECT / WITH … SELECT only.
//
// The function returns a parse error verbatim — callers must reject the
// statement on parse failure instead of degrading to "permit" (§ 9.3).
//
// Note: CTE names are NOT considered relations. A statement like
// `WITH t AS (...) SELECT * FROM t` produces refs for the bodies inside the
// CTEs and the surrounding query, but the bare `t` reference resolves at
// runtime to the CTE rather than a table. Postgres' parser exposes this
// through the AST shape directly — RangeVar nodes for CTE references carry
// no schema, so they collapse into the bare-name allow path (see § 9.3).
func extractRelations(sqlText string) ([]relationRef, error) {
	return extractRelationsForCap(sqlText, capRead)
}

// extractMutationRelations parses a DML statement and returns the relation
// references with each one tagged by the capability it must be gated under:
//
//   - The DML target (the `Relation` field of an INSERT/UPDATE/DELETE/MERGE
//     statement at any nesting depth) emits a `db:write` ref.
//   - Read-only sources reachable from the DML (UPDATE.FROM, DELETE.USING,
//     MERGE.SourceRelation, INSERT.SELECT, RETURNING / WHERE subqueries,
//     CTE bodies that are themselves SELECTs) emit `db:read` refs.
//   - A function-as-table (`RangeFunction`) reached from within a mutation
//     scope is gated under BOTH caps: a `setof`-returning function can read
//     and write state, and the AST gives us no way to tell which. Defence in
//     depth — see docs/wasm-abi.md § 10.3.
//
// This is the `db_exec` entry point. SELECT-only payloads will return early
// at validateMutationOnly and never reach this walker.
func extractMutationRelations(sqlText string) ([]relationRef, error) {
	return extractRelationsForCap(sqlText, capWrite)
}

func extractRelationsForCap(sqlText string, rootCap capKind) ([]relationRef, error) {
	res, err := pg_query.Parse(sqlText)
	if err != nil {
		return nil, fmt.Errorf("sql parse: %w", err)
	}
	if res == nil || len(res.GetStmts()) == 0 {
		return nil, fmt.Errorf("empty statement")
	}
	if len(res.GetStmts()) > 1 {
		return nil, fmt.Errorf("multiple statements not allowed")
	}

	top := res.GetStmts()[0].GetStmt()
	// For db_exec the top-level statement must be a DML (or a WITH wrapping
	// a DML — the parser still gives us InsertStmt / UpdateStmt / DeleteStmt
	// / MergeStmt at the top). A pure SelectStmt at the top of a capWrite
	// extraction means the caller leaked a SELECT past validateMutationOnly
	// (e.g. via a `WITH … SELECT` payload) — reject so db_exec can't be
	// used as a SELECT bypass.
	if rootCap == capWrite {
		switch top.GetNode().(type) {
		case *pg_query.Node_InsertStmt, *pg_query.Node_UpdateStmt,
			*pg_query.Node_DeleteStmt, *pg_query.Node_MergeStmt:
			// ok
		default:
			return nil, fmt.Errorf("only INSERT/UPDATE/DELETE/MERGE allowed as top-level statement")
		}
	}

	seen := make(map[string]struct{})
	var out []relationRef
	add := func(cap capKind, schema, table string) {
		schema = strings.ToLower(strings.TrimSpace(schema))
		table = strings.ToLower(strings.TrimSpace(table))
		if table == "" {
			return
		}
		key := string(cap) + "|" + schema + "." + table
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, relationRef{Schema: schema, Table: table, Cap: cap})
	}

	inMutation := rootCap == capWrite
	walkNodeForRelations(top, rootCap, inMutation, add)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cap != out[j].Cap {
			return out[i].Cap < out[j].Cap
		}
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Table < out[j].Table
	})
	return out, nil
}

// walkNodeForRelations recursively descends a libpg_query node tree and calls
// add(cap, schema, relname) for every relation it encounters. Two pieces of
// context are threaded through the walk:
//
//   - `ctxCap` is the capability the current relation reference should be
//     gated under. SELECT subtrees force capRead; the DML root nodes emit
//     their target Relation under capWrite then descend into read-only
//     sub-clauses under capRead. JoinExpr / RangeSubselect / SubLink / CTE
//     pass ctxCap through unchanged so the immediate caller wins.
//
//   - `inMutation` is true for the entire walk of a statement whose top-
//     level node is a DML (or a `WITH … <DML>`). It is the signal a
//     RangeFunction node uses to decide whether to emit the conservative
//     capWrite gate on the function name in addition to the contextual
//     capRead gate — see docs/wasm-abi.md § 10.3.
func walkNodeForRelations(n *pg_query.Node, ctxCap capKind, inMutation bool, add func(cap capKind, schema, table string)) {
	if n == nil {
		return
	}
	switch v := n.Node.(type) {

	case *pg_query.Node_RangeVar:
		add(ctxCap, v.RangeVar.GetSchemaname(), v.RangeVar.GetRelname())

	case *pg_query.Node_SelectStmt:
		s := v.SelectStmt
		// A SELECT body is always read-only.
		readCap := capRead
		for _, t := range s.GetFromClause() {
			walkNodeForRelations(t, readCap, inMutation, add)
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, readCap, inMutation, add)
			}
		}
		if s.GetWhereClause() != nil {
			walkNodeForRelations(s.GetWhereClause(), readCap, inMutation, add)
		}
		if s.GetHavingClause() != nil {
			walkNodeForRelations(s.GetHavingClause(), readCap, inMutation, add)
		}
		for _, tl := range s.GetTargetList() {
			walkNodeForRelations(tl, readCap, inMutation, add)
		}
		for _, g := range s.GetGroupClause() {
			walkNodeForRelations(g, readCap, inMutation, add)
		}
		for _, o := range s.GetSortClause() {
			walkNodeForRelations(o, readCap, inMutation, add)
		}
		for _, w := range s.GetWindowClause() {
			walkNodeForRelations(w, readCap, inMutation, add)
		}
		if larg := s.GetLarg(); larg != nil {
			walkNodeForRelations(&pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: larg}}, readCap, inMutation, add)
		}
		if rarg := s.GetRarg(); rarg != nil {
			walkNodeForRelations(&pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: rarg}}, readCap, inMutation, add)
		}
		for _, row := range s.GetValuesLists() {
			walkNodeForRelations(row, readCap, inMutation, add)
		}

	case *pg_query.Node_InsertStmt:
		s := v.InsertStmt
		if rel := s.GetRelation(); rel != nil {
			add(capWrite, rel.GetSchemaname(), rel.GetRelname())
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, capRead, true, add)
			}
		}
		// INSERT … SELECT — the source SELECT walks under capRead inside the
		// SelectStmt branch, but the entry node itself may be a VALUES list
		// which carries no relations of its own.
		if sel := s.GetSelectStmt(); sel != nil {
			walkNodeForRelations(sel, capRead, true, add)
		}
		for _, r := range s.GetReturningList() {
			walkNodeForRelations(r, capRead, true, add)
		}
		if oc := s.GetOnConflictClause(); oc != nil {
			walkNodeForRelations(oc.GetWhereClause(), capRead, true, add)
			for _, t := range oc.GetTargetList() {
				walkNodeForRelations(t, capRead, true, add)
			}
		}

	case *pg_query.Node_UpdateStmt:
		s := v.UpdateStmt
		if rel := s.GetRelation(); rel != nil {
			add(capWrite, rel.GetSchemaname(), rel.GetRelname())
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, capRead, true, add)
			}
		}
		// UPDATE … SET col = expr — the expressions live in target_list and
		// can reference other tables via correlated subqueries.
		for _, t := range s.GetTargetList() {
			walkNodeForRelations(t, capRead, true, add)
		}
		// UPDATE … FROM source_table — read-only sources.
		for _, f := range s.GetFromClause() {
			walkNodeForRelations(f, capRead, true, add)
		}
		if s.GetWhereClause() != nil {
			walkNodeForRelations(s.GetWhereClause(), capRead, true, add)
		}
		for _, r := range s.GetReturningList() {
			walkNodeForRelations(r, capRead, true, add)
		}

	case *pg_query.Node_DeleteStmt:
		s := v.DeleteStmt
		if rel := s.GetRelation(); rel != nil {
			add(capWrite, rel.GetSchemaname(), rel.GetRelname())
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, capRead, true, add)
			}
		}
		// DELETE … USING source_table — read-only sources.
		for _, u := range s.GetUsingClause() {
			walkNodeForRelations(u, capRead, true, add)
		}
		if s.GetWhereClause() != nil {
			walkNodeForRelations(s.GetWhereClause(), capRead, true, add)
		}
		for _, r := range s.GetReturningList() {
			walkNodeForRelations(r, capRead, true, add)
		}

	case *pg_query.Node_MergeStmt:
		s := v.MergeStmt
		if rel := s.GetRelation(); rel != nil {
			add(capWrite, rel.GetSchemaname(), rel.GetRelname())
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, capRead, true, add)
			}
		}
		walkNodeForRelations(s.GetSourceRelation(), capRead, true, add)
		walkNodeForRelations(s.GetJoinCondition(), capRead, true, add)
		for _, w := range s.GetMergeWhenClauses() {
			walkNodeForRelations(w, capRead, true, add)
		}
		for _, r := range s.GetReturningList() {
			walkNodeForRelations(r, capRead, true, add)
		}

	case *pg_query.Node_JoinExpr:
		j := v.JoinExpr
		walkNodeForRelations(j.GetLarg(), ctxCap, inMutation, add)
		walkNodeForRelations(j.GetRarg(), ctxCap, inMutation, add)
		walkNodeForRelations(j.GetQuals(), ctxCap, inMutation, add)

	case *pg_query.Node_RangeSubselect:
		walkNodeForRelations(v.RangeSubselect.GetSubquery(), ctxCap, inMutation, add)

	case *pg_query.Node_RangeFunction:
		// function-as-table — `SELECT * FROM other.fn(args)`. libpg_query
		// nests the FuncCall(s) inside Functions[], each item being a List
		// node whose first child is the FuncCall (a second nil entry is the
		// optional column definition). We pull the function name out and
		// gate it as a relation; any nested arg subqueries are walked too.
		for _, fnNode := range v.RangeFunction.GetFunctions() {
			fc := findFuncCall(fnNode)
			schema, name := funcCallTarget(fc)
			if name != "" {
				add(ctxCap, schema, name)
				// Within a mutation scope a setof-returning function may
				// read AND write state, and the AST gives us no way to
				// tell which. Defensively gate both axes — declared
				// capabilities for the cross-schema function must cover
				// both `db:read` and `db:write` to let the call through.
				if inMutation {
					other := capWrite
					if ctxCap == capWrite {
						other = capRead
					}
					add(other, schema, name)
				}
			}
			// Walk the FuncCall body for args that themselves contain
			// subqueries (correlated function-as-table arguments).
			walkNodeForRelations(fnNode, ctxCap, inMutation, add)
		}

	case *pg_query.Node_CommonTableExpr:
		// A CTE body inherits the caller's ctxCap — a `WITH x AS (UPDATE …)`
		// inside a DML statement keeps capWrite for the UPDATE target. The
		// dedicated InsertStmt / UpdateStmt / DeleteStmt / MergeStmt cases
		// above re-anchor the cap once we land on the actual DML node.
		walkNodeForRelations(v.CommonTableExpr.GetCtequery(), ctxCap, inMutation, add)

	case *pg_query.Node_SubLink:
		walkNodeForRelations(v.SubLink.GetSubselect(), ctxCap, inMutation, add)
		walkNodeForRelations(v.SubLink.GetTestexpr(), ctxCap, inMutation, add)

	case *pg_query.Node_ResTarget:
		walkNodeForRelations(v.ResTarget.GetVal(), ctxCap, inMutation, add)

	case *pg_query.Node_AExpr:
		walkNodeForRelations(v.AExpr.GetLexpr(), ctxCap, inMutation, add)
		walkNodeForRelations(v.AExpr.GetRexpr(), ctxCap, inMutation, add)

	case *pg_query.Node_BoolExpr:
		for _, a := range v.BoolExpr.GetArgs() {
			walkNodeForRelations(a, ctxCap, inMutation, add)
		}

	case *pg_query.Node_FuncCall:
		// A bare FuncCall inside an expression — its args may carry
		// subqueries / RangeFunctions. Note: the function name itself is
		// only gated when the call sits in a RangeFunction (function-as-
		// table) above; an expression-position call like `now()` or
		// `lower(col)` resolves at runtime through the search_path the same
		// way bare RangeVar names do, and bare names ride the implicit
		// own-schema grant per § 9.3.
		for _, a := range v.FuncCall.GetArgs() {
			walkNodeForRelations(a, ctxCap, inMutation, add)
		}
		walkNodeForRelations(v.FuncCall.GetAggFilter(), ctxCap, inMutation, add)

	case *pg_query.Node_CaseExpr:
		walkNodeForRelations(v.CaseExpr.GetArg(), ctxCap, inMutation, add)
		walkNodeForRelations(v.CaseExpr.GetDefresult(), ctxCap, inMutation, add)
		for _, w := range v.CaseExpr.GetArgs() {
			walkNodeForRelations(w, ctxCap, inMutation, add)
		}

	case *pg_query.Node_CaseWhen:
		walkNodeForRelations(v.CaseWhen.GetExpr(), ctxCap, inMutation, add)
		walkNodeForRelations(v.CaseWhen.GetResult(), ctxCap, inMutation, add)

	case *pg_query.Node_CoalesceExpr:
		for _, a := range v.CoalesceExpr.GetArgs() {
			walkNodeForRelations(a, ctxCap, inMutation, add)
		}

	case *pg_query.Node_List:
		for _, item := range v.List.GetItems() {
			walkNodeForRelations(item, ctxCap, inMutation, add)
		}

	case *pg_query.Node_TypeCast:
		walkNodeForRelations(v.TypeCast.GetArg(), ctxCap, inMutation, add)
	}
}

// findFuncCall digs through a libpg_query RangeFunction child node to find
// the underlying FuncCall. The grammar wraps each function in a `List` node
// whose first item is the FuncCall and whose second item is the optional
// column definition (usually nil). This helper accepts either shape — a
// bare FuncCall node or a List wrapping it — so the walker doesn't need to
// know the surrounding nesting.
func findFuncCall(n *pg_query.Node) *pg_query.FuncCall {
	if n == nil {
		return nil
	}
	switch v := n.Node.(type) {
	case *pg_query.Node_FuncCall:
		return v.FuncCall
	case *pg_query.Node_List:
		for _, item := range v.List.GetItems() {
			if fc := findFuncCall(item); fc != nil {
				return fc
			}
		}
	}
	return nil
}

// funcCallTarget extracts the schema-qualified name of the function invoked
// by a libpg_query FuncCall. Returns ("", "") when the FuncCall is nil or
// the name list is empty / malformed. Postgres' parser exposes Funcname as
// a list of String nodes:
//
//   - `[fn]`              → bare name (no schema)
//   - `[schema, fn]`      → schema-qualified
//   - `[catalog, schema, fn]` → cross-database (we treat the last two parts
//     as schema.fn — addons never legitimately reach across catalogs).
func funcCallTarget(fc *pg_query.FuncCall) (schema, name string) {
	if fc == nil {
		return "", ""
	}
	parts := fc.GetFuncname()
	if len(parts) == 0 {
		return "", ""
	}
	stringOf := func(node *pg_query.Node) string {
		if node == nil {
			return ""
		}
		if s, ok := node.Node.(*pg_query.Node_String_); ok && s.String_ != nil {
			return s.String_.GetSval()
		}
		return ""
	}
	switch len(parts) {
	case 1:
		return "", stringOf(parts[0])
	case 2:
		return stringOf(parts[0]), stringOf(parts[1])
	default:
		// catalog.schema.fn — collapse to schema.fn (last two parts).
		return stringOf(parts[len(parts)-2]), stringOf(parts[len(parts)-1])
	}
}
