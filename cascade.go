package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const fkViolation = "23503"

// pred is a WHERE clause with its bind parameters. Every predicate produced by
// the planner is a copy of the same root parameters nested deeper, so the args
// never have to be renumbered.
type pred struct {
	sql  string
	args []any
}

type fkEdge struct {
	constraint string // conname
	child      string // schema.table of the referencing table
	parent     string // schema.table of the referenced table
	childCols  []string
	parentCols []string
	action     string // confdeltype: a=NO ACTION, r=RESTRICT, c=CASCADE, n=SET NULL, d=SET DEFAULT
}

// inScope: NO ACTION/RESTRICT block the parent delete and CASCADE removes child
// rows, so both must be part of the plan. SET NULL/DEFAULT leave child rows (and
// their subtree) alive, so they are not touched.
func (e fkEdge) inScope() bool { return e.action == "a" || e.action == "r" || e.action == "c" }

type fkKey struct{ constraint, child string }

type deletePlan struct {
	order []string        // children first, root last
	preds map[string]pred // WHERE clause selecting the rows to delete per table
}

type cascader struct {
	tx     pgx.Tx
	budget int
	counts map[string]int64
	fks    map[fkKey]fkEdge
}

func (c *cascader) run(ctx context.Context, p deletePlan) error {
	for _, table := range p.order {
		if err := c.delete(ctx, table, p.preds[table]); err != nil {
			return err
		}
	}
	return nil
}

func (c *cascader) delete(ctx context.Context, table string, p pred) error {
	if c.budget <= 0 {
		return errors.New("statement budget exhausted, possible foreign key cycle (raise -max-statements if the data is only deep)")
	}
	c.budget--

	// Savepoint: a failed DELETE aborts the whole tx, but we still need to run
	// the follow-up statements on it.
	sp, err := c.tx.Begin(ctx)
	if err != nil {
		return err
	}
	tag, err := sp.Exec(ctx, "DELETE FROM "+quoteIdent(table)+" WHERE "+p.sql, p.args...)
	if err == nil {
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		n := tag.RowsAffected()
		c.counts[table] += n
		fmt.Fprintf(os.Stderr, "DELETE FROM %s WHERE %s %v -> %d\n", quoteIdent(table), p.sql, p.args, n)
		return nil
	}
	if err := sp.Rollback(ctx); err != nil {
		return err
	}

	// Row-level fallback: self-referencing trees, and anything the plan could
	// not foresee. Clear the referencing rows, then retry.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != fkViolation || pgErr.ConstraintName == "" {
		return err
	}
	fk, ok := c.fks[fkLookupKey(pgErr.ConstraintName, pgErr.TableName)]
	if !ok {
		return fmt.Errorf("%w (constraint %s not found in the startup snapshot)", err, pgErr.ConstraintName)
	}

	childPred := pred{
		sql: "(" + quoteList(fk.childCols) + ") IN (SELECT " + quoteList(fk.parentCols) +
			" FROM " + quoteIdent(fk.parent) + " WHERE " + p.sql + ")",
		args: p.args,
	}
	fmt.Fprintf(os.Stderr, "fk %s: clearing %s before retrying %s\n", fk.constraint, fk.child, table)
	if err := c.delete(ctx, fk.child, childPred); err != nil {
		return err
	}
	return c.delete(ctx, table, p)
}

// loadFKs reads the whole foreign key graph once per run.
func loadFKs(ctx context.Context, tx pgx.Tx) (map[string][]fkEdge, map[fkKey]fkEdge, error) {
	rows, err := tx.Query(ctx, `
SELECT con.oid,
       con.conname,
       con.confdeltype::text,
       src_ns.nspname || '.' || src.relname,
       tgt_ns.nspname || '.' || tgt.relname,
       src_att.attname,
       tgt_att.attname
FROM pg_constraint con
JOIN pg_class src ON src.oid = con.conrelid
JOIN pg_class tgt ON tgt.oid = con.confrelid
JOIN pg_namespace src_ns ON src_ns.oid = src.relnamespace
JOIN pg_namespace tgt_ns ON tgt_ns.oid = tgt.relnamespace
JOIN unnest(con.conkey) WITH ORDINALITY AS cols(attnum, ord) ON true
JOIN unnest(con.confkey) WITH ORDINALITY AS refcols(attnum, ord) ON cols.ord = refcols.ord
JOIN pg_attribute src_att ON src_att.attrelid = src.oid AND src_att.attnum = cols.attnum
JOIN pg_attribute tgt_att ON tgt_att.attrelid = tgt.oid AND tgt_att.attnum = refcols.attnum
WHERE con.contype = 'f'
ORDER BY con.oid, cols.ord`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	byParent := map[string][]fkEdge{}
	index := map[fkKey]fkEdge{}
	var cur fkEdge
	var curOID uint32
	flush := func() {
		if cur.child == "" {
			return
		}
		byParent[cur.parent] = append(byParent[cur.parent], cur)
		// shortcut: constraint names are only unique per table, so two schemas
		// with the same table and constraint names collapse to one entry.
		index[fkLookupKey(cur.constraint, cur.child)] = cur
	}
	for rows.Next() {
		var oid uint32
		var conname, action, child, parent, childCol, parentCol string
		if err := rows.Scan(&oid, &conname, &action, &child, &parent, &childCol, &parentCol); err != nil {
			return nil, nil, err
		}
		if oid != curOID {
			flush()
			curOID = oid
			cur = fkEdge{constraint: conname, child: child, parent: parent, action: action}
		}
		cur.childCols = append(cur.childCols, childCol)
		cur.parentCols = append(cur.parentCols, parentCol)
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return byParent, index, nil
}

func fkLookupKey(constraint, table string) fkKey {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		table = table[i+1:]
	}
	return fkKey{constraint: constraint, child: table}
}

// buildPlan collects every table reachable from root through the FK graph,
// orders them children-first and derives one DELETE predicate per table.
func buildPlan(root, column, value string, edgesByParent map[string][]fkEdge) (deletePlan, error) {
	rootPred := pred{sql: quoteIdent(column) + " = $1", args: []any{value}}
	args := rootPred.args

	relevant := map[string]bool{root: true}
	incoming := map[string][]fkEdge{} // child table -> edges coming from relevant parents
	for queue := []string{root}; len(queue) > 0; queue = queue[1:] {
		parent := queue[0]
		for _, e := range edgesByParent[parent] {
			if !e.inScope() {
				continue
			}
			incoming[e.child] = append(incoming[e.child], e)
			if e.child != parent && !relevant[e.child] {
				relevant[e.child] = true
				queue = append(queue, e.child)
			}
		}
	}

	// Kahn on "child before parent"; self edges are left to the fallback.
	parentsOf := map[string]map[string]bool{}
	outdeg := map[string]int{}
	for table := range relevant {
		outdeg[table] = 0
	}
	for child, edges := range incoming {
		for _, e := range edges {
			if e.child == e.parent {
				continue
			}
			if parentsOf[child] == nil {
				parentsOf[child] = map[string]bool{}
			}
			if !parentsOf[child][e.parent] {
				parentsOf[child][e.parent] = true
				outdeg[e.parent]++
			}
		}
	}

	var ready []string
	for table := range relevant {
		if outdeg[table] == 0 {
			ready = append(ready, table)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(relevant))
	for len(ready) > 0 {
		child := ready[0]
		ready = ready[1:]
		order = append(order, child)
		for parent := range parentsOf[child] {
			outdeg[parent]--
			if outdeg[parent] == 0 {
				ready = append(ready, parent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(relevant) {
		var stuck []string
		for table, deg := range outdeg {
			if deg > 0 {
				stuck = append(stuck, table)
			}
		}
		sort.Strings(stuck)
		return deletePlan{}, fmt.Errorf("foreign key cycle between %s, cannot order the deletes", strings.Join(stuck, ", "))
	}

	// Parents are later in the order, so build predicates back to front.
	preds := map[string]pred{root: rootPred}
	for i := len(order) - 1; i >= 0; i-- {
		table := order[i]
		if table == root {
			continue
		}
		var branches []string
		for _, e := range incoming[table] {
			if e.child == e.parent {
				continue
			}
			parentPred, ok := preds[e.parent]
			if !ok {
				return deletePlan{}, fmt.Errorf("internal error: no predicate for %s", e.parent)
			}
			branches = append(branches, "("+quoteList(e.childCols)+") IN (SELECT "+quoteList(e.parentCols)+
				" FROM "+quoteIdent(e.parent)+" WHERE "+parentPred.sql+")")
		}
		if len(branches) == 0 {
			return deletePlan{}, fmt.Errorf("internal error: no predicate for %s", table)
		}
		preds[table] = pred{sql: strings.Join(branches, " OR "), args: args}
	}
	return deletePlan{order: order, preds: preds}, nil
}

func quoteIdent(name string) string {
	return pgx.Identifier(strings.Split(name, ".")).Sanitize()
}

func quoteList(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	return strings.Join(quoted, ", ")
}
