package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const fkViolation = "23503"

// pred is a WHERE clause with its bind parameters. Recursion keeps the args of
// the original predicate and only nests it deeper, so $1 stays the innermost one.
type pred struct {
	sql  string
	args []any
}

type cascader struct {
	tx     pgx.Tx
	budget int
	counts map[string]int64
}

func (c *cascader) delete(ctx context.Context, table string, p pred) error {
	if c.budget <= 0 {
		return errors.New("statement budget exhausted, possible foreign key cycle (raise -max-statements if the data is only deep)")
	}
	c.budget--

	tag, err := c.tx.Exec(ctx, "DELETE FROM "+quoteIdent(table)+" WHERE "+p.sql, p.args...)
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != fkViolation || pgErr.ConstraintName == "" {
			return err
		}
		fk, fkErr := lookupFK(ctx, c.tx, pgErr)
		if fkErr != nil {
			return fmt.Errorf("%w (resolving constraint %s: %v)", err, pgErr.ConstraintName, fkErr)
		}

		// Delete the referencing rows of exactly the parent rows in p, then retry.
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

	n := tag.RowsAffected()
	c.counts[table] += n
	fmt.Fprintf(os.Stderr, "DELETE FROM %s WHERE %s %v -> %d\n", quoteIdent(table), p.sql, p.args, n)
	return nil
}

type fkInfo struct {
	constraint string
	child      string
	parent     string
	childCols  []string
	parentCols []string
}

func lookupFK(ctx context.Context, tx pgx.Tx, pgErr *pgconn.PgError) (fkInfo, error) {
	rows, err := tx.Query(ctx, `
SELECT con.conname,
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
  AND con.conname = $1
  AND ($2 = '' OR src.relname = $2)
ORDER BY src_ns.nspname, src.relname, cols.ord`,
		pgErr.ConstraintName, pgErr.TableName)
	if err != nil {
		return fkInfo{}, err
	}
	defer rows.Close()

	var fk fkInfo
	for rows.Next() {
		var conname, child, parent, childCol, parentCol string
		if err := rows.Scan(&conname, &child, &parent, &childCol, &parentCol); err != nil {
			return fkInfo{}, err
		}
		if fk.constraint == "" {
			fk = fkInfo{constraint: conname, child: child, parent: parent}
		} else if fk.child != child || fk.parent != parent {
			return fkInfo{}, fmt.Errorf("constraint name %q exists on more than one table", conname)
		}
		fk.childCols = append(fk.childCols, childCol)
		fk.parentCols = append(fk.parentCols, parentCol)
	}
	if err := rows.Err(); err != nil {
		return fkInfo{}, err
	}
	if fk.constraint == "" {
		return fkInfo{}, fmt.Errorf("constraint %q not found in pg_constraint", pgErr.ConstraintName)
	}
	return fk, nil
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
