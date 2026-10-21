package main

import (
	"context"
	"fmt"
	"os"
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

	schema := fmt.Sprintf("pgtools_test_%d", time.Now().UnixNano())
	mustExec(t, ctx, conn, "CREATE SCHEMA "+quoteIdent(schema))
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

	if err := run(ctx, dsn, users, "id", "1", true, 1000); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertCount(t, ctx, conn, users, 2)
	assertCount(t, ctx, conn, orders, 2)
	assertCount(t, ctx, conn, items, 2)

	if err := run(ctx, dsn, users, "id", "1", false, 1000); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	assertCount(t, ctx, conn, users, 1)
	assertCount(t, ctx, conn, orders, 1)
	assertCount(t, ctx, conn, items, 1)
	assertCount(t, ctx, conn, orders+" WHERE id = 11", 1)

	// Composite foreign key: deleting both rows with a = 1 must clear both refs.
	if err := run(ctx, dsn, schema+".pairs", "a", "1", false, 1000); err != nil {
		t.Fatalf("composite cascade: %v", err)
	}
	assertCount(t, ctx, conn, schema+".pairs", 1)
	assertCount(t, ctx, conn, schema+".pair_refs", 1)
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
