package pgmem_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Raw table API", func() {
	var database *pgmem.DB

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)
	})

	It("inserts maps and returns generated values", func() {
		public := database.Public()
		Expect(public.None(`
			CREATE TABLE people (
				id SERIAL PRIMARY KEY,
				name TEXT NOT NULL,
				role TEXT NOT NULL DEFAULT 'engineer'
			)
		`)).To(Succeed())
		table, err := public.GetTable("people")
		Expect(err).NotTo(HaveOccurred())

		inserted, err := table.Insert(pgmem.Row{"name": "Ada"})
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(SatisfyAll(
			HaveKeyWithValue("id", BeNumerically("==", 1)),
			HaveKeyWithValue("name", "Ada"),
			HaveKeyWithValue("role", "engineer"),
		))
		Expect(table.Name()).To(Equal("people"))
		Expect(table.Schema()).To(BeIdenticalTo(public))
	})

	It("finds rows using deterministic template predicates", func() {
		public := database.Public()
		Expect(public.None(`CREATE TABLE facts (id INTEGER, label TEXT, note TEXT)`)).To(Succeed())
		Expect(public.None(`
			INSERT INTO facts VALUES
				(1, 'match', NULL),
				(2, 'other', NULL),
				(3, 'match', 'present')
		`)).To(Succeed())
		table, err := public.Table("facts")
		Expect(err).NotTo(HaveOccurred())

		Expect(table.Find(pgmem.Row{"note": nil, "label": "match"})).To(Equal(
			[]pgmem.Row{{"id": int64(1), "label": "match", "note": nil}},
		))
	})

	It("quotes relation and column names from Go data", func() {
		public := database.Public()
		Expect(public.None(`CREATE TABLE "Audit ""Entry" ("Entry ""ID" INTEGER, "select" TEXT)`)).To(Succeed())
		table, err := public.Table(`Audit "Entry`)
		Expect(err).NotTo(HaveOccurred())
		Expect(table.Insert(pgmem.Row{`Entry "ID`: int64(7), "select": "safe"})).To(Equal(
			pgmem.Row{`Entry "ID`: int64(7), "select": "safe"},
		))
	})

	It("preserves context errors while looking up a table", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := database.Public().TableContext(ctx, "anything")
		Expect(err).To(MatchError(ContainSubstring(context.Canceled.Error())))
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
	})

	It("returns a PostgreSQL-shaped missing relation error", func() {
		_, err := database.Public().Table("missing")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, pgmem.ErrRelationNotFound)).To(BeTrue())
		var databaseError *pgmem.Error
		Expect(errors.As(err, &databaseError)).To(BeTrue())
		Expect(databaseError.SQLState()).To(Equal("42P01"))
	})

	It("keeps same-named tables isolated by schema", func(ctx SpecContext) {
		public := database.Public()
		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(public.None(`CREATE TABLE entries (value TEXT)`)).To(Succeed())
		Expect(audit.None(`CREATE TABLE entries (value TEXT)`)).To(Succeed())

		publicTable, err := public.TableContext(ctx, "entries")
		Expect(err).NotTo(HaveOccurred())
		auditTable, err := audit.TableContext(ctx, "entries")
		Expect(err).NotTo(HaveOccurred())
		Expect(publicTable.InsertContext(ctx, pgmem.Row{"value": "public"})).To(
			HaveKeyWithValue("value", "public"),
		)
		Expect(auditTable.InsertContext(ctx, pgmem.Row{"value": "audit"})).To(
			HaveKeyWithValue("value", "audit"),
		)
		Expect(publicTable.FindContext(ctx, nil)).To(Equal([]pgmem.Row{{"value": "public"}}))
		Expect(auditTable.FindContext(ctx, nil)).To(Equal([]pgmem.Row{{"value": "audit"}}))
	})
})
