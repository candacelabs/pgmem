package pgmem_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("database/sql adapter", func() {
	var (
		database *pgmem.DB
		pool     *sql.DB
	)

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		pool = database.Open()
	})

	AfterEach(func() {
		Expect(pool.Close()).To(Succeed())
		Expect(database.Close()).To(Succeed())
	})

	It("executes PostgreSQL statements with positional arguments", func(ctx SpecContext) {
		Expect(pool.PingContext(ctx)).To(Succeed())
		_, err := pool.ExecContext(ctx, `
			CREATE TABLE public.users (
				id SERIAL PRIMARY KEY,
				name TEXT NOT NULL
			)
		`)
		Expect(err).NotTo(HaveOccurred())

		result, err := pool.ExecContext(ctx,
			`INSERT INTO public.users (name) VALUES ($1)`,
			"Ada",
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RowsAffected()).To(Equal(int64(1)))

		var (
			id   int64
			name string
		)
		Expect(pool.QueryRowContext(ctx,
			`SELECT id, name FROM public.users WHERE name = $1`,
			"Ada",
		).Scan(&id, &name)).To(Succeed())
		Expect(id).To(Equal(int64(1)))
		Expect(name).To(Equal("Ada"))
	})

	It("exposes pool lifecycle hooks through its connector", func(ctx SpecContext) {
		connection, err := database.Connector().Connect(ctx)
		Expect(err).NotTo(HaveOccurred())

		pinger, canPing := connection.(driver.Pinger)
		resetter, canReset := connection.(driver.SessionResetter)
		validator, canValidate := connection.(driver.Validator)
		Expect(canPing).To(BeTrue())
		Expect(canReset).To(BeTrue())
		Expect(canValidate).To(BeTrue())
		Expect(pinger.Ping(ctx)).To(Succeed())
		Expect(resetter.ResetSession(ctx)).To(Succeed())
		Expect(validator.IsValid()).To(BeTrue())

		Expect(connection.Close()).To(Succeed())
		Expect(validator.IsValid()).To(BeFalse())
		Expect(resetter.ResetSession(ctx)).To(MatchError(driver.ErrBadConn))
	})

	It("translates direct and prepared operations at their execution boundary", func(ctx SpecContext) {
		Expect(pool.Close()).To(Succeed())
		Expect(database.Close()).To(Succeed())

		controller := gomock.NewController(GinkgoT())
		translator := NewMockTranslator(controller)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "make table").
			Return(`CREATE TABLE translated (value TEXT NOT NULL)`, nil)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "add value").
			Return(`INSERT INTO translated (value) VALUES ($1)`, nil)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "find value").
			Return(`SELECT value FROM translated WHERE value = $1`, nil)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "prepare value").
			Return(`SELECT value FROM translated WHERE value = $1`, nil)

		var err error
		database, err = pgmem.New(pgmem.WithTranslator(translator))
		Expect(err).NotTo(HaveOccurred())
		pool = database.Open()

		_, err = pool.ExecContext(ctx, "make table")
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.ExecContext(ctx, "add value", "translated")
		Expect(err).NotTo(HaveOccurred())

		var direct string
		Expect(pool.QueryRowContext(ctx, "find value", "translated").Scan(&direct)).To(Succeed())
		Expect(direct).To(Equal("translated"))

		statement, err := pool.PrepareContext(ctx, "prepare value")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(statement.Close)
		var prepared string
		Expect(statement.QueryRowContext(ctx, "translated").Scan(&prepared)).To(Succeed())
		Expect(prepared).To(Equal("translated"))
	})

	It("keeps transaction operations on one pinned engine connection", func(ctx SpecContext) {
		_, err := pool.ExecContext(ctx, `CREATE TABLE public.entries (value TEXT NOT NULL)`)
		Expect(err).NotTo(HaveOccurred())

		transaction, err := pool.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = transaction.ExecContext(ctx, `INSERT INTO public.entries (value) VALUES ($1)`, "pending")
		Expect(err).NotTo(HaveOccurred())

		var inside int64
		Expect(transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM public.entries`).Scan(&inside)).To(Succeed())
		Expect(inside).To(Equal(int64(1)))

		blockedContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = database.Public().OneContext(blockedContext, `SELECT COUNT(*) AS count FROM public.entries`)
		Expect(err).To(MatchError(ContainSubstring(context.DeadlineExceeded.Error())))

		Expect(transaction.Rollback()).To(Succeed())
		var after int64
		Expect(pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM public.entries`).Scan(&after)).To(Succeed())
		Expect(after).To(BeZero())
	})

	It("commits work and supports statements prepared inside a transaction", func(ctx SpecContext) {
		_, err := pool.ExecContext(ctx, `CREATE TABLE public.entries (value TEXT NOT NULL)`)
		Expect(err).NotTo(HaveOccurred())
		transaction, err := pool.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())

		statement, err := transaction.PrepareContext(ctx, `INSERT INTO public.entries (value) VALUES ($1)`)
		Expect(err).NotTo(HaveOccurred())
		_, err = statement.ExecContext(ctx, "committed")
		Expect(err).NotTo(HaveOccurred())
		Expect(statement.Close()).To(Succeed())
		Expect(transaction.Commit()).To(Succeed())

		var value string
		Expect(pool.QueryRowContext(ctx, `SELECT value FROM public.entries`).Scan(&value)).To(Succeed())
		Expect(value).To(Equal("committed"))
	})

	It("opens an adapter with a named schema as its default", func(ctx SpecContext) {
		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		auditPool := audit.Open()
		DeferCleanup(auditPool.Close)

		_, err = auditPool.ExecContext(ctx, `CREATE TABLE events (message TEXT NOT NULL)`)
		Expect(err).NotTo(HaveOccurred())
		_, err = auditPool.ExecContext(ctx, `INSERT INTO events (message) VALUES ($1)`, "created")
		Expect(err).NotTo(HaveOccurred())

		var message string
		Expect(auditPool.QueryRowContext(ctx, `SELECT message FROM events`).Scan(&message)).To(Succeed())
		Expect(message).To(Equal("created"))
		_, err = database.Public().OneContext(ctx, `SELECT message FROM events`)
		Expect(err).To(HaveOccurred())
	})

	It("honors canceled contexts and rejects use after the engine closes", func() {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(pool.PingContext(canceled)).To(MatchError(context.Canceled))

		Expect(pool.Close()).To(Succeed())
		Expect(database.Close()).To(Succeed())
		pool = database.Open()
		Expect(pool.PingContext(context.Background())).To(MatchError(pgmem.ErrClosed))
		Expect(errors.Is(pool.PingContext(context.Background()), pgmem.ErrClosed)).To(BeTrue())
	})
})
