package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string (default: $DATABASE_URL)")
	dryRun := flag.Bool("dry-run", false, "run the whole chain in a transaction, then roll it back")
	maxStatements := flag.Int("max-statements", 1000, "hard cap on executed DELETE statements, guards against foreign key cycles")
	strategy := flag.String("strategy", "plan", "cascade strategy: plan (topological FK snapshot) or detail (parse the FK error detail, like the original TS script)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <table> <column> <value>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 3 || *dsn == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *strategy != "plan" && *strategy != "detail" {
		fmt.Fprintf(os.Stderr, "unknown -strategy %q, want plan or detail\n", *strategy)
		os.Exit(2)
	}

	err := run(context.Background(), *dsn, flag.Arg(0), flag.Arg(1), flag.Arg(2), *strategy, *dryRun, *maxStatements)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn, table, column, value, strategy string, dryRun bool, maxStatements int) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op once committed

	edges, index, err := loadFKs(ctx, tx)
	if err != nil {
		return err
	}
	c := &cascader{tx: tx, budget: maxStatements, counts: map[string]int64{}, fks: index}
	if strategy == "detail" {
		fmt.Fprintln(os.Stderr, "strategy: detail")
		if err := c.deleteByDetail(ctx, table, column, value); err != nil {
			return err
		}
	} else {
		p, err := buildPlan(table, column, value, edges)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "delete order: %s\n", strings.Join(p.order, ", "))
		if err := c.run(ctx, p); err != nil {
			return err
		}
	}

	var total int64
	tables := make([]string, 0, len(c.counts))
	for t, n := range c.counts {
		tables = append(tables, t)
		total += n
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Printf("%-40s %d\n", t, c.counts[t])
	}

	if dryRun {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		fmt.Printf("dry run: rolled back, %d rows would be deleted\n", total)
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("deleted %d rows\n", total)
	return nil
}
