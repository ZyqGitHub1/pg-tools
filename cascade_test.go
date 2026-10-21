package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Integration test against a real database; set PGTOOLS_TEST_DSN to run it.
func TestCascadeDelete(t *testing.T) {
	dsn := os.Getenv("PGTOOLS_TEST_DSN")
	if dsn == "" {
		t.Skip("PGTOOLS_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	schema := newTestSchema(t, ctx, conn)
	defer conn.Exec(ctx, "DROP SCHEMA "+quoteIdent(schema)+" CASCADE")

	mustExec(t, ctx, conn, `
CREATE TABLE `+schema+`.users (id int PRIMARY KEY);
CREATE TABLE `+schema+`.orders (id int PRIMARY KEY, user_id int REFERENCES `+schema+`.users(id));
CREATE TABLE `+schema+`.order_items (id int PRIMARY KEY, order_id int REFERENCES `+schema+`.orders(id));
CREATE TABLE `+schema+`.pairs (a int, b int, PRIMARY KEY (a, b));
CREATE TABLE `+schema+`.pair_refs (pa int, pb int, FOREIGN KEY (pa, pb) REFERENCES `+schema+`.pairs(a, b));
INSERT INTO `+schema+`.users VALUES (1), (2);
INSERT INTO `+schema+`.orders VALUES (10, 1), (11, 2);
INSERT INTO `+schema+`.order_items VALUES (100, 10), (101, 11);
INSERT INTO `+schema+`.pairs VALUES (1, 1), (1, 2), (2, 1);
INSERT INTO `+schema+`.pair_refs VALUES (1, 1), (1, 2), (2, 1);
`)

	users, orders, items := schema+".users", schema+".orders", schema+".order_items"

	if err := run(ctx, dsn, users, "id", "1", "plan", true, 1000); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertCount(t, ctx, conn, users, 2)
	assertCount(t, ctx, conn, orders, 2)
	assertCount(t, ctx, conn, items, 2)

	if err := run(ctx, dsn, users, "id", "1", "plan", false, 1000); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	assertCount(t, ctx, conn, users, 1)
	assertCount(t, ctx, conn, orders, 1)
	assertCount(t, ctx, conn, items, 1)
	assertCount(t, ctx, conn, orders+" WHERE id = 11", 1)

	// Composite foreign key: deleting both rows with a = 1 must clear both refs.
	if err := run(ctx, dsn, schema+".pairs", "a", "1", "plan", false, 1000); err != nil {
		t.Fatalf("composite cascade: %v", err)
	}
	assertCount(t, ctx, conn, schema+".pairs", 1)
	assertCount(t, ctx, conn, schema+".pair_refs", 1)
}

// Covers the topo-planned paths: self-referencing tree, CASCADE chain with a
// NO ACTION grandchild, SET NULL, and a multi-table cycle.
func TestCascadePlan(t *testing.T) {
	dsn := os.Getenv("PGTOOLS_TEST_DSN")
	if dsn == "" {
		t.Skip("PGTOOLS_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	schema := newTestSchema(t, ctx, conn)
	defer conn.Exec(ctx, "DROP SCHEMA "+quoteIdent(schema)+" CASCADE")

	mustExec(t, ctx, conn, `
CREATE TABLE `+schema+`.categories (id int PRIMARY KEY, parent_id int REFERENCES `+schema+`.categories(id));
CREATE TABLE `+schema+`.products (id int PRIMARY KEY, category_id int REFERENCES `+schema+`.categories(id));
CREATE TABLE `+schema+`.a (id int PRIMARY KEY);
CREATE TABLE `+schema+`.b (id int PRIMARY KEY, a_id int REFERENCES `+schema+`.a(id) ON DELETE CASCADE);
CREATE TABLE `+schema+`.c (id int PRIMARY KEY, b_id int REFERENCES `+schema+`.b(id));
CREATE TABLE `+schema+`.p (id int PRIMARY KEY);
CREATE TABLE `+schema+`.s (id int PRIMARY KEY, p_id int REFERENCES `+schema+`.p(id) ON DELETE SET NULL);
CREATE TABLE `+schema+`.cyc_a (id int PRIMARY KEY, b_id int);
CREATE TABLE `+schema+`.cyc_b (id int PRIMARY KEY, a_id int REFERENCES `+schema+`.cyc_a(id));
ALTER TABLE `+schema+`.cyc_a ADD FOREIGN KEY (b_id) REFERENCES `+schema+`.cyc_b(id);
INSERT INTO `+schema+`.categories VALUES (1, NULL), (2, 1), (3, 2);
INSERT INTO `+schema+`.products VALUES (10, 2), (11, 3);
INSERT INTO `+schema+`.a VALUES (1);
INSERT INTO `+schema+`.b VALUES (1, 1);
INSERT INTO `+schema+`.c VALUES (1, 1);
INSERT INTO `+schema+`.p VALUES (1);
INSERT INTO `+schema+`.s VALUES (1, 1);
INSERT INTO `+schema+`.cyc_a VALUES (1, NULL);
INSERT INTO `+schema+`.cyc_b VALUES (1, 1);
UPDATE `+schema+`.cyc_a SET b_id = 1;
`)

	// Self reference plus an external table pointing at a descendant.
	if err := run(ctx, dsn, schema+".categories", "id", "1", "plan", false, 1000); err != nil {
		t.Fatalf("tree cascade: %v", err)
	}
	assertCount(t, ctx, conn, schema+".categories", 0)
	assertCount(t, ctx, conn, schema+".products", 0)

	// CASCADE edge leads to a table with a NO ACTION child.
	if err := run(ctx, dsn, schema+".a", "id", "1", "plan", false, 1000); err != nil {
		t.Fatalf("cascade chain: %v", err)
	}
	assertCount(t, ctx, conn, schema+".a", 0)
	assertCount(t, ctx, conn, schema+".b", 0)
	assertCount(t, ctx, conn, schema+".c", 0)

	// SET NULL leaves the child row alive.
	if err := run(ctx, dsn, schema+".p", "id", "1", "plan", false, 1000); err != nil {
		t.Fatalf("set null: %v", err)
	}
	assertCount(t, ctx, conn, schema+".p", 0)
	assertCount(t, ctx, conn, schema+".s WHERE p_id IS NULL", 1)

	// Mutual foreign keys cannot be ordered; refuse before touching data.
	if err := run(ctx, dsn, schema+".cyc_a", "id", "1", "plan", false, 1000); err == nil {
		t.Fatal("cycle: expected an error")
	}
	assertCount(t, ctx, conn, schema+".cyc_a", 1)
	assertCount(t, ctx, conn, schema+".cyc_b", 1)
}

// Detail strategy: reproduce the TS error-text parsing path.
func TestCascadeDetail(t *testing.T) {
	dsn := os.Getenv("PGTOOLS_TEST_DSN")
	if dsn == "" {
		t.Skip("PGTOOLS_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	schema := newTestSchema(t, ctx, conn)
	defer conn.Exec(ctx, "DROP SCHEMA "+quoteIdent(schema)+" CASCADE")

	mustExec(t, ctx, conn, `
CREATE TABLE `+schema+`.users (id int PRIMARY KEY);
CREATE TABLE `+schema+`.orders (id int PRIMARY KEY, user_id int REFERENCES `+schema+`.users(id));
CREATE TABLE `+schema+`.order_items (id int PRIMARY KEY, order_id int REFERENCES `+schema+`.orders(id));
CREATE TABLE `+schema+`.a (id int PRIMARY KEY);
CREATE TABLE `+schema+`.b (id int PRIMARY KEY, a_id int REFERENCES `+schema+`.a(id) ON DELETE CASCADE);
CREATE TABLE `+schema+`.c (id int PRIMARY KEY, b_id int REFERENCES `+schema+`.b(id));
CREATE TABLE `+schema+`.categories (id int PRIMARY KEY, parent_id int REFERENCES `+schema+`.categories(id));
CREATE TABLE `+schema+`.products (id int PRIMARY KEY, category_id int REFERENCES `+schema+`.categories(id));
CREATE TABLE `+schema+`.pairs (a int, b int, PRIMARY KEY (a, b));
CREATE TABLE `+schema+`.pair_refs (pa int, pb int, FOREIGN KEY (pa, pb) REFERENCES `+schema+`.pairs(a, b));
CREATE TABLE `+schema+`.cyc_a (id int PRIMARY KEY, b_id int);
CREATE TABLE `+schema+`.cyc_b (id int PRIMARY KEY, a_id int REFERENCES `+schema+`.cyc_a(id));
ALTER TABLE `+schema+`.cyc_a ADD FOREIGN KEY (b_id) REFERENCES `+schema+`.cyc_b(id);
INSERT INTO `+schema+`.users VALUES (1), (2);
INSERT INTO `+schema+`.orders VALUES (10, 1), (11, 2);
INSERT INTO `+schema+`.order_items VALUES (100, 10), (101, 11);
INSERT INTO `+schema+`.a VALUES (1);
INSERT INTO `+schema+`.b VALUES (1, 1);
INSERT INTO `+schema+`.c VALUES (1, 1);
INSERT INTO `+schema+`.categories VALUES (1, NULL), (2, 1), (3, 2);
INSERT INTO `+schema+`.products VALUES (10, 2), (11, 3);
INSERT INTO `+schema+`.pairs VALUES (1, 1), (1, 2), (2, 1);
INSERT INTO `+schema+`.pair_refs VALUES (1, 1), (1, 2), (2, 1);
INSERT INTO `+schema+`.cyc_a VALUES (1, NULL);
INSERT INTO `+schema+`.cyc_b VALUES (1, 1);
UPDATE `+schema+`.cyc_a SET b_id = 1;
`)

	if err := run(ctx, dsn, schema+".users", "id", "1", "detail", true, 1000); err != nil {
		t.Fatalf("detail dry run: %v", err)
	}
	assertCount(t, ctx, conn, schema+".users", 2)
	assertCount(t, ctx, conn, schema+".orders", 2)
	assertCount(t, ctx, conn, schema+".order_items", 2)

	if err := run(ctx, dsn, schema+".users", "id", "1", "detail", false, 1000); err != nil {
		t.Fatalf("detail chain: %v", err)
	}
	assertCount(t, ctx, conn, schema+".users", 1)
	assertCount(t, ctx, conn, schema+".orders", 1)
	assertCount(t, ctx, conn, schema+".order_items", 1)

	// CASCADE hop: the detail value belongs to the blocked child, so it works.
	if err := run(ctx, dsn, schema+".a", "id", "1", "detail", false, 1000); err != nil {
		t.Fatalf("detail cascade chain: %v", err)
	}
	assertCount(t, ctx, conn, schema+".a", 0)
	assertCount(t, ctx, conn, schema+".b", 0)
	assertCount(t, ctx, conn, schema+".c", 0)

	// Self-referencing tree plus an external table.
	if err := run(ctx, dsn, schema+".categories", "id", "1", "detail", false, 1000); err != nil {
		t.Fatalf("detail tree: %v", err)
	}
	assertCount(t, ctx, conn, schema+".categories", 0)
	assertCount(t, ctx, conn, schema+".products", 0)

	// Composite keys are refused instead of using "1, 2" as one value.
	err = run(ctx, dsn, schema+".pairs", "a", "1", "detail", false, 1000)
	if err == nil || !strings.Contains(err.Error(), "composite") {
		t.Fatalf("detail composite: got %v, want a composite key error", err)
	}
	assertCount(t, ctx, conn, schema+".pairs", 3)
	assertCount(t, ctx, conn, schema+".pair_refs", 3)

	// Mutual foreign keys: the detail loop runs out of budget and rolls back.
	err = run(ctx, dsn, schema+".cyc_a", "id", "1", "detail", false, 40)
	if err == nil {
		t.Fatal("detail cycle: expected an error")
	}
	assertCount(t, ctx, conn, schema+".cyc_a", 1)
	assertCount(t, ctx, conn, schema+".cyc_b", 1)
}

func newTestSchema(t *testing.T, ctx context.Context, conn *pgx.Conn) string {
	t.Helper()
	schema := fmt.Sprintf("pgtools_test_%d", time.Now().UnixNano())
	mustExec(t, ctx, conn, "CREATE SCHEMA "+quoteIdent(schema))
	return schema
}

func mustExec(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func assertCount(t *testing.T, ctx context.Context, conn *pgx.Conn, from string, want int) {
	t.Helper()
	var got int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+from).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count(%s) = %d, want %d", from, got, want)
	}
}
