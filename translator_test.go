package pgmem_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Translator boundary", func() {
	It("uses a configured translator before execution", func(ctx SpecContext) {
		controller := gomock.NewController(GinkgoT())
		translator := NewMockTranslator(controller)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "meaning").
			Return("SELECT 42 AS answer", nil)

		database, err := pgmem.New(pgmem.WithTranslator(translator))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		Expect(database.Public().OneContext(ctx, "meaning")).To(Equal(pgmem.Row{"answer": int64(42)}))
	})

	It("accepts internal-dialect output when no functions need rewriting", func(ctx SpecContext) {
		controller := gomock.NewController(GinkgoT())
		translator := NewMockTranslator(controller)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "set internal version").
			Return("PRAGMA user_version = 7", nil)
		translator.EXPECT().
			Translate(gomock.Any(), "public", "read internal version").
			Return("PRAGMA user_version", nil)

		database, err := pgmem.New(pgmem.WithTranslator(translator))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		Expect(database.Public().NoneContext(ctx, "set internal version")).To(Succeed())
		Expect(database.Public().OneContext(ctx, "read internal version")).To(Equal(
			pgmem.Row{"user_version": int64(7)},
		))
	})

	It("preserves translator failures", func() {
		controller := gomock.NewController(GinkgoT())
		translator := NewMockTranslator(controller)
		failure := errors.New("unsupported syntax")
		translator.EXPECT().Translate(gomock.Any(), "public", "BROKEN").Return("", failure)

		database, err := pgmem.New(pgmem.WithTranslator(translator))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		Expect(database.Public().None("BROKEN")).To(MatchError(ContainSubstring("unsupported syntax")))
	})

	It("pins unqualified public relations to the public schema", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(audit.NoneContext(ctx, `CREATE TABLE events (message TEXT NOT NULL)`)).To(Succeed())
		Expect(audit.NoneContext(ctx, `INSERT INTO events VALUES ('audit')`)).To(Succeed())

		_, err = database.Public().OneContext(ctx, `SELECT message FROM events`)
		Expect(err).To(HaveOccurred())
	})

	It("keeps same-schema foreign keys valid while pinning table relations", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		public := database.Public()
		Expect(public.NoneContext(ctx, `CREATE TABLE parents (id INTEGER PRIMARY KEY)`)).To(Succeed())
		Expect(public.NoneContext(ctx, `CREATE TABLE children (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER REFERENCES parents(id)
		)`)).To(Succeed())
		Expect(public.NoneContext(ctx, `INSERT INTO children VALUES (1, 999)`)).NotTo(Succeed())
	})

	It("does not retarget cross-schema foreign keys to the created table's schema", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(database.Public().NoneContext(ctx, `CREATE TABLE parents (id INTEGER PRIMARY KEY)`)).To(Succeed())
		Expect(audit.NoneContext(ctx, `CREATE TABLE parents (id INTEGER PRIMARY KEY)`)).To(Succeed())

		// PostgreSQL resolves the unqualified parent through the receiver's public
		// search path, not through the explicitly qualified audit child. SQLite
		// cannot represent that cross-schema foreign key, so fail instead of
		// silently binding it to audit.parents.
		Expect(database.Public().NoneContext(ctx, `
			CREATE TABLE audit.children (
				id INTEGER PRIMARY KEY,
				parent_id INTEGER REFERENCES parents(id)
			)
		`)).NotTo(Succeed())
	})
})
