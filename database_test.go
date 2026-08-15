package pgmem_test

import (
	"context"
	"database/sql"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Database", func() {
	var database *pgmem.DB

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(database.Close()).To(Succeed())
	})

	It("runs a PostgreSQL-shaped create, insert, and select workflow", func() {
		public := database.Public()
		Expect(public.None(`
			CREATE TABLE public.users (
				id SERIAL PRIMARY KEY,
				name TEXT NOT NULL,
				active BOOLEAN NOT NULL DEFAULT TRUE
			)
		`)).To(Succeed())

		insert, err := public.Exec(
			`INSERT INTO public.users (name, active) VALUES ($1, $2)`,
			"Ada", true,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(insert.RowCount).To(Equal(int64(1)))
		Expect(insert.Command).To(Equal("INSERT"))

		row, err := public.One(`SELECT id, name, active FROM public.users`)
		Expect(err).NotTo(HaveOccurred())
		Expect(row).To(SatisfyAll(
			HaveKeyWithValue("id", BeNumerically("==", 1)),
			HaveKeyWithValue("name", "Ada"),
			HaveKeyWithValue("active", BeNumerically("==", 1)),
		))
	})

	It("enforces exact cardinality for One", func() {
		public := database.Public()
		_, err := public.One(`SELECT 1 WHERE FALSE`)
		Expect(err).To(MatchError(pgmem.ErrNoRows))

		_, err = public.One(`SELECT 1 UNION ALL SELECT 2`)
		Expect(err).To(MatchError(ContainSubstring("expected one row, got many")))
		Expect(errors.Is(err, pgmem.ErrTooManyRows)).To(BeTrue())
	})

	It("creates separately addressable schemas", func(ctx context.Context) {
		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(audit.Name()).To(Equal("audit"))
		Expect(database.SchemaNames()).To(Equal([]string{"audit", "public"}))
		Expect(audit.None(`CREATE TABLE audit.events (message TEXT NOT NULL)`)).To(Succeed())
		Expect(audit.None(`INSERT INTO audit.events VALUES ('created')`)).To(Succeed())
		Expect(audit.One(`SELECT message FROM audit.events`)).To(Equal(pgmem.Row{"message": "created"}))
	})

	It("folds API schema identifiers and rejects reserved names case-insensitively", func(ctx SpecContext) {
		audit, err := database.CreateSchema(ctx, "Audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(audit.Name()).To(Equal("audit"))
		lookedUp, exists := database.Schema("AUDIT")
		Expect(exists).To(BeTrue())
		Expect(lookedUp).To(BeIdenticalTo(audit))

		_, err = database.CreateSchema(ctx, "audit")
		Expect(err).To(HaveOccurred())
		var databaseError *pgmem.Error
		Expect(errors.As(err, &databaseError)).To(BeTrue())
		Expect(databaseError.SQLState()).To(Equal("42P06"))

		for _, reserved := range []string{"MAIN", "Temp", "Public"} {
			_, err = database.CreateSchema(ctx, reserved)
			Expect(errors.Is(err, pgmem.ErrInvalidSchema)).To(BeTrue(), reserved)
		}
	})

	It("does not deadlock a transaction while a schema waits for the engine connection", func(ctx SpecContext) {
		pool := database.Open()
		DeferCleanup(pool.Close)
		transaction, err := pool.BeginTx(ctx, &sql.TxOptions{})
		Expect(err).NotTo(HaveOccurred())

		created := make(chan error, 1)
		go func() {
			_, createErr := database.CreateSchema(context.Background(), "audit")
			created <- createErr
		}()
		Consistently(created, 25*time.Millisecond).ShouldNot(Receive())

		execContext, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err = transaction.ExecContext(execContext, `SELECT 1`)
		Expect(err).NotTo(HaveOccurred())
		Expect(transaction.Rollback()).To(Succeed())
		Eventually(created, time.Second).Should(Receive(BeNil()))
	})

	It("executes scripts atomically", func() {
		public := database.Public()
		Expect(public.None(`CREATE TABLE handles (name TEXT UNIQUE NOT NULL)`)).To(Succeed())

		err := public.None(`
			INSERT INTO handles VALUES ('first');
			INSERT INTO handles VALUES ('first');
		`)
		Expect(err).To(HaveOccurred())
		var databaseError *pgmem.Error
		Expect(errors.As(err, &databaseError)).To(BeTrue())
		Expect(databaseError.SQLState()).To(Equal("23505"))

		row, err := public.One(`SELECT count(*) AS count FROM handles`)
		Expect(err).NotTo(HaveOccurred())
		Expect(row).To(HaveKeyWithValue("count", BeNumerically("==", 0)))
	})

	It("returns and commits the final query in a script", func() {
		public := database.Public()
		result, err := public.Query(`
			CREATE TABLE messages (body TEXT NOT NULL);
			INSERT INTO messages VALUES ('hello');
			SELECT body FROM messages;
		`)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Rows).To(Equal([]pgmem.Row{{"body": "hello"}}))
		Expect(public.One(`SELECT count(*) AS count FROM messages`)).To(
			HaveKeyWithValue("count", BeNumerically("==", 1)),
		)
	})

	It("supports PostgreSQL RETURNING and ILIKE through AST translation", func() {
		public := database.Public()
		Expect(public.None(`CREATE TABLE notes (id SERIAL PRIMARY KEY, body TEXT NOT NULL)`)).To(Succeed())

		inserted, err := public.One(`INSERT INTO notes (body) VALUES ($1) RETURNING id, body`, "Hello")
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(SatisfyAll(
			HaveKeyWithValue("id", BeNumerically("==", 1)),
			HaveKeyWithValue("body", "Hello"),
		))
		Expect(public.One(`SELECT body FROM notes WHERE body ILIKE 'h%'`)).To(Equal(
			pgmem.Row{"body": "Hello"},
		))
		Expect(public.Many(`SELECT body FROM notes WHERE body NOT ILIKE 'h%'`)).To(BeEmpty())
	})

	It("derives command metadata through leading SQL comments", func() {
		result, err := database.Public().Query("/* fixture context */ SELECT 1 AS value")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Command).To(Equal("SELECT"))
	})

	It("reports PostgreSQL syntax SQLSTATEs", func() {
		err := database.Public().None(`SELECT FROM WHERE`)
		Expect(err).To(HaveOccurred())
		var databaseError *pgmem.Error
		Expect(errors.As(err, &databaseError)).To(BeTrue())
		Expect(databaseError.SQLState()).To(Equal("42601"))
	})

	It("rejects work after close", func() {
		public := database.Public()
		Expect(database.Close()).To(Succeed())
		Expect(database.Close()).To(Succeed())
		Expect(public.None(`SELECT 1`)).To(MatchError(pgmem.ErrClosed))
	})
})
