package pgmem_test

import (
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Backup", func() {
	var database *pgmem.DB

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)
	})

	It("restores the public schema repeatedly from an immutable snapshot", func(ctx SpecContext) {
		public := database.Public()
		Expect(public.NoneContext(ctx, `
			CREATE TABLE public.documents (
				id INTEGER PRIMARY KEY,
				body BYTEA NOT NULL
			)
		`)).To(Succeed())
		original := []byte("saved")
		Expect(public.NoneContext(ctx, `INSERT INTO public.documents VALUES (1, $1)`, original)).To(Succeed())

		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		original[0] = 'X'

		Expect(public.NoneContext(ctx, `UPDATE public.documents SET body = $1 WHERE id = 1`, []byte("changed"))).To(Succeed())
		Expect(public.NoneContext(ctx, `INSERT INTO public.documents VALUES (2, $1)`, []byte("later"))).To(Succeed())
		Expect(backup.RestoreContext(ctx)).To(Succeed())
		Expect(public.ManyContext(ctx, `SELECT id, body FROM public.documents ORDER BY id`)).To(Equal([]pgmem.Row{
			{"id": int64(1), "body": []byte("saved")},
		}))

		Expect(public.NoneContext(ctx, `DELETE FROM public.documents`)).To(Succeed())
		Expect(backup.RestoreContext(ctx)).To(Succeed())
		Expect(public.OneContext(ctx, `SELECT body FROM public.documents`)).To(Equal(pgmem.Row{"body": []byte("saved")}))
	})

	It("snapshots and restores every named schema", func(ctx SpecContext) {
		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		Expect(database.Public().NoneContext(ctx, `CREATE TABLE public.values_ (value TEXT NOT NULL)`)).To(Succeed())
		Expect(audit.NoneContext(ctx, `CREATE TABLE audit.events (message TEXT NOT NULL)`)).To(Succeed())
		Expect(database.Public().NoneContext(ctx, `INSERT INTO public.values_ VALUES ('public-before')`)).To(Succeed())
		Expect(audit.NoneContext(ctx, `INSERT INTO audit.events VALUES ('audit-before')`)).To(Succeed())

		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(database.Public().NoneContext(ctx, `UPDATE public.values_ SET value = 'public-after'`)).To(Succeed())
		Expect(audit.NoneContext(ctx, `UPDATE audit.events SET message = 'audit-after'`)).To(Succeed())

		Expect(backup.RestoreContext(ctx)).To(Succeed())
		Expect(database.Public().OneContext(ctx, `SELECT value FROM public.values_`)).To(Equal(pgmem.Row{"value": "public-before"}))
		Expect(audit.OneContext(ctx, `SELECT message FROM audit.events`)).To(Equal(pgmem.Row{"message": "audit-before"}))

		Expect(audit.NoneContext(ctx, `DELETE FROM audit.events`)).To(Succeed())
		Expect(backup.RestoreContext(ctx)).To(Succeed())
		Expect(audit.OneContext(ctx, `SELECT message FROM audit.events`)).To(Equal(pgmem.Row{"message": "audit-before"}))
	})

	It("rejects restore after schema DDL without changing current data", func(ctx SpecContext) {
		public := database.Public()
		Expect(public.NoneContext(ctx, `CREATE TABLE public.entries (value TEXT NOT NULL)`)).To(Succeed())
		Expect(public.NoneContext(ctx, `INSERT INTO public.entries VALUES ('before')`)).To(Succeed())
		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(public.NoneContext(ctx, `ALTER TABLE public.entries ADD COLUMN note TEXT`)).To(Succeed())
		Expect(public.NoneContext(ctx, `UPDATE public.entries SET value = 'current'`)).To(Succeed())
		err = backup.RestoreContext(ctx)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, pgmem.ErrSchemaChanged)).To(BeTrue())
		Expect(public.OneContext(ctx, `SELECT value FROM public.entries`)).To(Equal(pgmem.Row{"value": "current"}))
	})

	It("rejects restore after named-schema topology changes", func(ctx SpecContext) {
		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = database.CreateSchema(ctx, "later")
		Expect(err).NotTo(HaveOccurred())

		err = backup.RestoreContext(ctx)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, pgmem.ErrSchemaChanged)).To(BeTrue())
	})

	It("serializes concurrent restores and ordinary operations", func(ctx SpecContext) {
		public := database.Public()
		Expect(public.NoneContext(ctx, `CREATE TABLE public.counter (value INTEGER NOT NULL)`)).To(Succeed())
		Expect(public.NoneContext(ctx, `INSERT INTO public.counter VALUES (0)`)).To(Succeed())
		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())

		const workers = 6
		const iterations = 12
		failures := make(chan error, workers*iterations*2)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for iteration := 0; iteration < iterations; iteration++ {
					if _, err := public.ExecContext(ctx, `UPDATE public.counter SET value = value + 1`); err != nil {
						failures <- err
					}
					if err := backup.RestoreContext(ctx); err != nil {
						failures <- err
					}
				}
			}()
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(backup.RestoreContext(ctx)).To(Succeed())
		Expect(public.OneContext(ctx, `SELECT value FROM public.counter`)).To(Equal(pgmem.Row{"value": int64(0)}))
	})

	It("rejects backup and restore after close", func() {
		backup, err := database.Backup()
		Expect(err).NotTo(HaveOccurred())
		Expect(database.Close()).To(Succeed())
		_, err = database.Backup()
		Expect(err).To(MatchError(pgmem.ErrClosed))
		Expect(backup.Restore()).To(MatchError(pgmem.ErrClosed))
	})

	It("honors cancellation before the restore commit without changing data", func(ctx SpecContext) {
		public := database.Public()
		Expect(public.NoneContext(ctx, `CREATE TABLE state (value TEXT NOT NULL)`)).To(Succeed())
		Expect(public.NoneContext(ctx, `INSERT INTO state VALUES ('saved')`)).To(Succeed())
		backup, err := database.BackupContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(public.NoneContext(ctx, `UPDATE state SET value = 'current'`)).To(Succeed())

		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		err = backup.RestoreContext(canceled)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(public.OneContext(ctx, `SELECT value FROM state`)).To(Equal(pgmem.Row{"value": "current"}))
	})
})
