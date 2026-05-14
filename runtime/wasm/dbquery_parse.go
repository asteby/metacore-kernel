package wasm

import (
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// relationRef is a parsed reference to a relation (table / view / subquery)
// pulled out of a SQL statement's AST. Schema is empty for bare names — the
// host knows those will be resolved by `SET LOCAL search_path` against the
// addon's own schema.
type relationRef struct {
	Schema string
	Table  string
}

// extractRelations parses a SQL statement and returns every relation
// reference reachable from the AST: top-level FROM tables, JOIN sides, CTE
// bodies, subqueries (RangeSubselect / SubLink), set-op arms (UNION /
// INTERSECT / EXCEPT) and so on. Duplicates are collapsed.
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

	seen := make(map[string]struct{})
	var out []relationRef
	add := func(schema, table string) {
		schema = strings.ToLower(strings.TrimSpace(schema))
		table = strings.ToLower(strings.TrimSpace(table))
		if table == "" {
			return
		}
		key := schema + "." + table
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, relationRef{Schema: schema, Table: table})
	}

	walkNodeForRelations(res.GetStmts()[0].GetStmt(), add)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Table < out[j].Table
	})
	return out, nil
}

// walkNodeForRelations recursively descends a libpg_query node tree and calls
// add(schema, relname) for every RangeVar it encounters. The recursion is
// kept narrow — it only follows clauses that can legitimately host relation
// references inside a SELECT/WITH … SELECT (the only statement shapes
// validateSelectOnly allows).
func walkNodeForRelations(n *pg_query.Node, add func(schema, table string)) {
	if n == nil {
		return
	}
	switch v := n.Node.(type) {

	case *pg_query.Node_RangeVar:
		add(v.RangeVar.GetSchemaname(), v.RangeVar.GetRelname())

	case *pg_query.Node_SelectStmt:
		s := v.SelectStmt
		for _, t := range s.GetFromClause() {
			walkNodeForRelations(t, add)
		}
		if wc := s.GetWithClause(); wc != nil {
			for _, cte := range wc.GetCtes() {
				walkNodeForRelations(cte, add)
			}
		}
		if s.GetWhereClause() != nil {
			walkNodeForRelations(s.GetWhereClause(), add)
		}
		if s.GetHavingClause() != nil {
			walkNodeForRelations(s.GetHavingClause(), add)
		}
		for _, tl := range s.GetTargetList() {
			walkNodeForRelations(tl, add)
		}
		for _, g := range s.GetGroupClause() {
			walkNodeForRelations(g, add)
		}
		for _, o := range s.GetSortClause() {
			walkNodeForRelations(o, add)
		}
		for _, w := range s.GetWindowClause() {
			walkNodeForRelations(w, add)
		}
		if larg := s.GetLarg(); larg != nil {
			walkNodeForRelations(&pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: larg}}, add)
		}
		if rarg := s.GetRarg(); rarg != nil {
			walkNodeForRelations(&pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: rarg}}, add)
		}
		for _, row := range s.GetValuesLists() {
			walkNodeForRelations(row, add)
		}

	case *pg_query.Node_JoinExpr:
		j := v.JoinExpr
		walkNodeForRelations(j.GetLarg(), add)
		walkNodeForRelations(j.GetRarg(), add)
		walkNodeForRelations(j.GetQuals(), add)

	case *pg_query.Node_RangeSubselect:
		walkNodeForRelations(v.RangeSubselect.GetSubquery(), add)

	case *pg_query.Node_RangeFunction:
		for _, fn := range v.RangeFunction.GetFunctions() {
			walkNodeForRelations(fn, add)
		}

	case *pg_query.Node_CommonTableExpr:
		walkNodeForRelations(v.CommonTableExpr.GetCtequery(), add)

	case *pg_query.Node_SubLink:
		walkNodeForRelations(v.SubLink.GetSubselect(), add)
		walkNodeForRelations(v.SubLink.GetTestexpr(), add)

	case *pg_query.Node_ResTarget:
		walkNodeForRelations(v.ResTarget.GetVal(), add)

	case *pg_query.Node_AExpr:
		walkNodeForRelations(v.AExpr.GetLexpr(), add)
		walkNodeForRelations(v.AExpr.GetRexpr(), add)

	case *pg_query.Node_BoolExpr:
		for _, a := range v.BoolExpr.GetArgs() {
			walkNodeForRelations(a, add)
		}

	case *pg_query.Node_FuncCall:
		for _, a := range v.FuncCall.GetArgs() {
			walkNodeForRelations(a, add)
		}
		walkNodeForRelations(v.FuncCall.GetAggFilter(), add)

	case *pg_query.Node_CaseExpr:
		walkNodeForRelations(v.CaseExpr.GetArg(), add)
		walkNodeForRelations(v.CaseExpr.GetDefresult(), add)
		for _, w := range v.CaseExpr.GetArgs() {
			walkNodeForRelations(w, add)
		}

	case *pg_query.Node_CaseWhen:
		walkNodeForRelations(v.CaseWhen.GetExpr(), add)
		walkNodeForRelations(v.CaseWhen.GetResult(), add)

	case *pg_query.Node_CoalesceExpr:
		for _, a := range v.CoalesceExpr.GetArgs() {
			walkNodeForRelations(a, add)
		}

	case *pg_query.Node_List:
		for _, item := range v.List.GetItems() {
			walkNodeForRelations(item, add)
		}

	case *pg_query.Node_TypeCast:
		walkNodeForRelations(v.TypeCast.GetArg(), add)
	}
}
