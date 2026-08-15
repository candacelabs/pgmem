package pgmem_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

// These black-box fixtures are reduced adaptations of the pg-mem v3.0.14
// compatibility specs pinned in UPSTREAM.md. The source paths for each group
// are recorded beside the relevant table and in COMPATIBILITY.md. Assertions
// use pgmem's public Go API rather than pg-mem's TypeScript helpers.
var _ = Describe("pg-mem v3.0.14 compatibility", func() {
	var database *pgmem.DB

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)
	})

	type statementCase struct {
		setup     []string
		statement string
		arguments []any
		query     string
		expected  []pgmem.Row
		rowCount  int64
		command   string
	}

	// Adapted from upstream src/tests/simple-queries.spec.ts and the mutation
	// specs in src/tests/{insert,update,delete}.queries.spec.ts.
	DescribeTable("executes basic PostgreSQL-shaped CRUD",
		func(testCase statementCase) {
			public := database.Public()
			for _, statement := range testCase.setup {
				Expect(public.None(statement)).To(Succeed())
			}

			result, err := public.Exec(testCase.statement, testCase.arguments...)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RowCount).To(Equal(testCase.rowCount))
			Expect(result.Command).To(Equal(testCase.command))
			Expect(public.Many(testCase.query)).To(Equal(testCase.expected))
		},
		Entry("creates a table", statementCase{
			statement: `CREATE TABLE public.widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
			query:     `SELECT id, name FROM public.widgets`,
			expected:  []pgmem.Row{},
			rowCount:  0,
			command:   "CREATE",
		}),
		Entry("inserts with PostgreSQL parameters", statementCase{
			setup:     []string{`CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`},
			statement: `INSERT INTO widgets (id, name) VALUES ($1, $2)`,
			arguments: []any{int64(1), "Ada"},
			query:     `SELECT id, name FROM widgets ORDER BY id`,
			expected:  []pgmem.Row{{"id": int64(1), "name": "Ada"}},
			rowCount:  1,
			command:   "INSERT",
		}),
		Entry("updates selected rows", statementCase{
			setup: []string{
				`CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
				`INSERT INTO widgets (id, name) VALUES (1, 'Ada'), (2, 'Grace')`,
			},
			statement: `UPDATE widgets SET name = $1 WHERE id = $2`,
			arguments: []any{"Hopper", int64(2)},
			query:     `SELECT id, name FROM widgets ORDER BY id`,
			expected: []pgmem.Row{
				{"id": int64(1), "name": "Ada"},
				{"id": int64(2), "name": "Hopper"},
			},
			rowCount: 1,
			command:  "UPDATE",
		}),
		Entry("deletes selected rows", statementCase{
			setup: []string{
				`CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
				`INSERT INTO widgets (id, name) VALUES (1, 'Ada'), (2, 'Grace')`,
			},
			statement: `DELETE FROM widgets WHERE id = $1`,
			arguments: []any{int64(1)},
			query:     `SELECT id, name FROM widgets ORDER BY id`,
			expected:  []pgmem.Row{{"id": int64(2), "name": "Grace"}},
			rowCount:  1,
			command:   "DELETE",
		}),
	)

	// Adapted from upstream src/tests/nulls.spec.ts and the NULL fixtures in
	// src/tests/simple-queries.spec.ts.
	DescribeTable("preserves SQL three-valued NULL filtering",
		func(predicate string, expected []pgmem.Row) {
			public := database.Public()
			Expect(public.None(`CREATE TABLE nullable_values (id INTEGER PRIMARY KEY, value TEXT)`)).To(Succeed())
			Expect(public.None(`INSERT INTO nullable_values VALUES (1, NULL), (2, 'present')`)).To(Succeed())

			statement := `SELECT id FROM nullable_values WHERE ` + predicate + ` ORDER BY id`
			Expect(public.Many(statement)).To(Equal(expected))
		},
		Entry("does not equate a value to NULL", `value = NULL`, []pgmem.Row{}),
		Entry("does not make NULL unequal to NULL", `value != NULL`, []pgmem.Row{}),
		Entry("matches NULL with IS NULL", `value IS NULL`, []pgmem.Row{{"id": int64(1)}}),
		Entry("matches values with IS NOT NULL", `value IS NOT NULL`, []pgmem.Row{{"id": int64(2)}}),
	)

	// Adapted from upstream src/tests/where.queries.spec.ts,
	// src/tests/order-by.queries.spec.ts, and src/tests/limit.queries.spec.ts.
	DescribeTable("filters, orders, and limits rows",
		func(query string, expectedIDs ...int64) {
			public := database.Public()
			Expect(public.None(`CREATE TABLE scores (id INTEGER PRIMARY KEY, score INTEGER NOT NULL)`)).To(Succeed())
			Expect(public.None(`INSERT INTO scores VALUES (1, 10), (2, 30), (3, 20), (4, 30)`)).To(Succeed())

			rows, err := public.Many(query)
			Expect(err).NotTo(HaveOccurred())
			actualIDs := make([]int64, 0, len(rows))
			for _, row := range rows {
				actualIDs = append(actualIDs, row["id"].(int64))
			}
			Expect(actualIDs).To(Equal(expectedIDs))
		},
		Entry("filters with WHERE", `SELECT id FROM scores WHERE score = 20 ORDER BY id`, int64(3)),
		Entry("orders by multiple expressions", `SELECT id FROM scores ORDER BY score DESC, id ASC`, int64(2), int64(4), int64(3), int64(1)),
		Entry("applies LIMIT", `SELECT id FROM scores ORDER BY id LIMIT 2`, int64(1), int64(2)),
		Entry("applies LIMIT with OFFSET", `SELECT id FROM scores ORDER BY id LIMIT 2 OFFSET 1`, int64(2), int64(3)),
	)

	// Adapted from upstream src/tests/simple-queries.spec.ts and
	// src/tests/{insert,update,delete}.queries.spec.ts. pgmem exposes returned
	// rows through Query/Many/One; Exec intentionally exposes only metadata.
	DescribeTable("materializes mutation RETURNING rows",
		func(setup []string, statement string, arguments []any, expected []pgmem.Row) {
			public := database.Public()
			for _, setupStatement := range setup {
				Expect(public.None(setupStatement)).To(Succeed())
			}
			Expect(public.Many(statement, arguments...)).To(Equal(expected))
		},
		Entry("returns inserted serial values",
			[]string{`CREATE TABLE returning_items (id SERIAL PRIMARY KEY, name TEXT NOT NULL)`},
			`INSERT INTO returning_items (name) VALUES ($1) RETURNING id, name`,
			[]any{"Ada"},
			[]pgmem.Row{{"id": int64(1), "name": "Ada"}},
		),
		Entry("returns updated values",
			[]string{
				`CREATE TABLE returning_items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
				`INSERT INTO returning_items VALUES (1, 'Ada')`,
			},
			`UPDATE returning_items SET name = $1 WHERE id = $2 RETURNING id, name`,
			[]any{"Lovelace", int64(1)},
			[]pgmem.Row{{"id": int64(1), "name": "Lovelace"}},
		),
		Entry("returns deleted values",
			[]string{
				`CREATE TABLE returning_items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
				`INSERT INTO returning_items VALUES (1, 'Ada')`,
			},
			`DELETE FROM returning_items WHERE id = $1 RETURNING id, name`,
			[]any{int64(1)},
			[]pgmem.Row{{"id": int64(1), "name": "Ada"}},
		),
	)

	type constraintCase struct {
		setup        []string
		statement    string
		expectedCode string
		query        string
		expected     []pgmem.Row
	}

	// Reduced from upstream src/tests/publicapi.spec.ts,
	// src/tests/constraints.spec.ts, src/tests/foreign-keys.spec.ts, and the
	// default fixtures in src/tests/insert.queries.spec.ts.
	DescribeTable("rejects an entire statement on a constraint error",
		func(testCase constraintCase) {
			public := database.Public()
			for _, statement := range testCase.setup {
				Expect(public.None(statement)).To(Succeed())
			}

			err := public.None(testCase.statement)
			Expect(err).To(HaveOccurred())
			var executionError *pgmem.Error
			Expect(errors.As(err, &executionError)).To(BeTrue())
			Expect(executionError.Code).To(Equal(testCase.expectedCode))
			Expect(executionError.Statement).To(Equal(testCase.statement))
			Expect(public.Many(testCase.query)).To(Equal(testCase.expected))
		},
		Entry("keeps a multi-row unique violation atomic", constraintCase{
			setup: []string{
				`CREATE TABLE accounts (id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL)`,
				`INSERT INTO accounts VALUES (1, 'used@example.test')`,
			},
			statement:    `INSERT INTO accounts VALUES (2, 'new@example.test'), (3, 'used@example.test')`,
			expectedCode: "23505",
			query:        `SELECT id, email FROM accounts ORDER BY id`,
			expected:     []pgmem.Row{{"id": int64(1), "email": "used@example.test"}},
		}),
		Entry("keeps a multi-row foreign-key violation atomic", constraintCase{
			setup: []string{
				`CREATE TABLE parents (id INTEGER PRIMARY KEY)`,
				`CREATE TABLE children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id))`,
				`INSERT INTO parents VALUES (1)`,
			},
			statement:    `INSERT INTO children VALUES (1, 1), (2, 999)`,
			expectedCode: "23503",
			query:        `SELECT id, parent_id FROM children ORDER BY id`,
			expected:     []pgmem.Row{},
		}),
		Entry("keeps a multi-row not-null violation atomic", constraintCase{
			setup:        []string{`CREATE TABLE required_values (id INTEGER PRIMARY KEY, value TEXT NOT NULL DEFAULT 'fallback')`},
			statement:    `INSERT INTO required_values VALUES (1, 'present'), (2, NULL)`,
			expectedCode: "23502",
			query:        `SELECT id, value FROM required_values ORDER BY id`,
			expected:     []pgmem.Row{},
		}),
	)

	DescribeTable("applies column defaults without replacing explicit values",
		func(statement string, expected pgmem.Row) {
			public := database.Public()
			Expect(public.None(`CREATE TABLE default_values (id INTEGER PRIMARY KEY, value TEXT DEFAULT 'fallback')`)).To(Succeed())
			Expect(public.None(statement)).To(Succeed())
			Expect(public.One(`SELECT id, value FROM default_values`)).To(Equal(expected))
		},
		Entry("uses a default for an omitted column",
			`INSERT INTO default_values (id) VALUES (1)`,
			pgmem.Row{"id": int64(1), "value": "fallback"},
		),
		Entry("preserves an explicit NULL",
			`INSERT INTO default_values (id, value) VALUES (1, NULL)`,
			pgmem.Row{"id": int64(1), "value": nil},
		),
		Entry("preserves an explicit value",
			`INSERT INTO default_values (id, value) VALUES (1, 'chosen')`,
			pgmem.Row{"id": int64(1), "value": "chosen"},
		),
	)

	// Adapted from upstream src/tests/naming.spec.ts. This table deliberately
	// avoids implying PostgreSQL's case-distinct quoted-name behavior, which the
	// current SQLite-backed executor does not provide.
	DescribeTable("accepts PostgreSQL identifiers in the supported subset",
		func(create, insert, query string, expected pgmem.Row) {
			public := database.Public()
			Expect(public.None(create)).To(Succeed())
			Expect(public.None(insert)).To(Succeed())
			Expect(public.One(query)).To(Equal(expected))
		},
		Entry("folds unquoted names",
			`CREATE TABLE MixedName (CamelColumn TEXT NOT NULL)`,
			`INSERT INTO MIXEDNAME (CAMELCOLUMN) VALUES ('value')`,
			`SELECT camelcolumn FROM mixedname`,
			pgmem.Row{"camelcolumn": "value"},
		),
		Entry("preserves quoted names with spaces",
			`CREATE TABLE "Ledger Entry" ("Entry ID" INTEGER NOT NULL)`,
			`INSERT INTO "Ledger Entry" ("Entry ID") VALUES (42)`,
			`SELECT "Entry ID" FROM "Ledger Entry"`,
			pgmem.Row{"Entry ID": int64(42)},
		),
		Entry("accepts an escaped quote in a quoted name",
			`CREATE TABLE "a""b" (value TEXT NOT NULL)`,
			`INSERT INTO "a""b" VALUES ('quoted')`,
			`SELECT value FROM "a""b"`,
			pgmem.Row{"value": "quoted"},
		),
	)

	// Reduced from upstream src/tests/invalid-syntaxes.spec.ts. Syntax errors
	// are rejected by PostgreSQL's parser before the execution backend sees SQL.
	DescribeTable("returns PostgreSQL-shaped syntax errors",
		func(statement string) {
			err := database.Public().None(statement)
			Expect(err).To(HaveOccurred())
			var syntaxError *pgmem.Error
			Expect(errors.As(err, &syntaxError)).To(BeTrue())
			Expect(syntaxError).To(SatisfyAll(
				HaveField("Code", "42601"),
				HaveField("Statement", statement),
				HaveField("Cause", Not(BeNil())),
			))
		},
		Entry("rejects a misspelled statement", `SELEC 1`),
		Entry("rejects an unterminated string", `SELECT 'unfinished`),
		Entry("rejects a missing statement separator", `
			CREATE TABLE broken (value INTEGER);
			INSERT INTO broken VALUES (1)
			SELECT * FROM broken;
		`),
	)

	// Adapted from upstream src/db.ts and
	// src/tests/schema-manipulation.spec.ts. pgmem v0.1 creates schemas through
	// the Go API rather than CREATE SCHEMA SQL.
	DescribeTable("isolates named schemas",
		func(run func(*pgmem.DB)) {
			run(database)
		},
		Entry("qualifies unqualified statements with the schema receiver", func(db *pgmem.DB) {
			audit, err := db.CreateSchema(GinkgoT().Context(), "audit")
			Expect(err).NotTo(HaveOccurred())
			Expect(audit.None(`CREATE TABLE events (message TEXT NOT NULL)`)).To(Succeed())
			Expect(audit.None(`INSERT INTO events VALUES ('created')`)).To(Succeed())
			Expect(audit.One(`SELECT message FROM events`)).To(Equal(pgmem.Row{"message": "created"}))
		}),
		Entry("keeps same-named relations separate", func(db *pgmem.DB) {
			audit, err := db.CreateSchema(GinkgoT().Context(), "audit")
			Expect(err).NotTo(HaveOccurred())
			Expect(db.Public().None(`CREATE TABLE events (message TEXT NOT NULL)`)).To(Succeed())
			Expect(audit.None(`CREATE TABLE events (message TEXT NOT NULL)`)).To(Succeed())
			Expect(db.Public().None(`INSERT INTO events VALUES ('public')`)).To(Succeed())
			Expect(audit.None(`INSERT INTO events VALUES ('audit')`)).To(Succeed())

			Expect(db.Public().One(`SELECT message FROM public.events`)).To(Equal(pgmem.Row{"message": "public"}))
			Expect(db.Public().One(`SELECT message FROM audit.events`)).To(Equal(pgmem.Row{"message": "audit"}))
		}),
		Entry("addresses an explicit named schema from another receiver", func(db *pgmem.DB) {
			audit, err := db.CreateSchema(GinkgoT().Context(), "audit")
			Expect(err).NotTo(HaveOccurred())
			Expect(audit.None(`CREATE TABLE events (message TEXT NOT NULL)`)).To(Succeed())
			Expect(db.Public().None(`INSERT INTO audit.events VALUES ('cross-schema')`)).To(Succeed())
			Expect(audit.One(`SELECT message FROM audit.events`)).To(Equal(pgmem.Row{"message": "cross-schema"}))
		}),
	)
})
