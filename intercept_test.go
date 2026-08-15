package pgmem_test

import (
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Query interception", func() {
	var database *pgmem.DB

	BeforeEach(func() {
		var err error
		database, err = pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)
	})

	It("returns ad-hoc rows before translation or execution", func(ctx SpecContext) {
		payload := pgmem.Result{Rows: []pgmem.Row{{"answer": int64(42)}}}
		database.Public().InterceptQueries(func(
			_ context.Context,
			statement string,
			arguments []any,
		) (pgmem.Result, bool, error) {
			Expect(statement).To(Equal("not valid SQL"))
			Expect(arguments).To(Equal([]any{"question"}))
			return payload, true, nil
		})

		result, err := database.Public().QueryContext(ctx, "not valid SQL", "question")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(pgmem.Result{
			Rows:     []pgmem.Row{{"answer": int64(42)}},
			RowCount: 1,
			Command:  "NOT",
		}))
	})

	It("passes through and preserves explicit statement results", func(ctx SpecContext) {
		public := database.Public()
		public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			return pgmem.Result{}, false, nil
		})
		Expect(public.OneContext(ctx, `SELECT 7 AS value`)).To(Equal(pgmem.Row{"value": int64(7)}))

		public.InterceptQueries(func(_ context.Context, statement string, _ []any) (pgmem.Result, bool, error) {
			if statement != "SYNTHETIC UPDATE" {
				return pgmem.Result{}, false, nil
			}
			return pgmem.Result{RowCount: 9, Command: "CUSTOM"}, true, nil
		})
		Expect(public.ExecContext(ctx, "SYNTHETIC UPDATE")).To(Equal(pgmem.Result{RowCount: 9, Command: "CUSTOM"}))
	})

	It("runs interceptors in order and deregisters idempotently", func(ctx SpecContext) {
		public := database.Public()
		var calls []string
		first := public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			calls = append(calls, "first")
			return pgmem.Result{}, false, nil
		})
		second := public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			calls = append(calls, "second")
			return pgmem.Result{Rows: []pgmem.Row{{"source": "second"}}}, true, nil
		})
		public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			calls = append(calls, "third")
			return pgmem.Result{Rows: []pgmem.Row{{"source": "third"}}}, true, nil
		})

		Expect(public.OneContext(ctx, "synthetic")).To(Equal(pgmem.Row{"source": "second"}))
		Expect(calls).To(Equal([]string{"first", "second"}))

		second.Unsubscribe()
		second.Unsubscribe()
		calls = nil
		Expect(public.OneContext(ctx, "synthetic")).To(Equal(pgmem.Row{"source": "third"}))
		Expect(calls).To(Equal([]string{"first", "third"}))
		first.Unsubscribe()
	})

	It("isolates registrations, arguments, and reusable results", func(ctx SpecContext) {
		public := database.Public()
		audit, err := database.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		shared := pgmem.Result{Rows: []pgmem.Row{{"payload": []byte("stable")}}}
		public.InterceptQueries(func(_ context.Context, _ string, arguments []any) (pgmem.Result, bool, error) {
			arguments[0].([]byte)[0] = 'X'
			return shared, true, nil
		})

		argument := []byte("caller")
		first, err := public.QueryContext(ctx, "synthetic", argument)
		Expect(err).NotTo(HaveOccurred())
		first.Rows[0]["payload"].([]byte)[0] = 'X'
		Expect(argument).To(Equal([]byte("caller")))
		Expect(public.OneContext(ctx, "synthetic", argument)).To(Equal(pgmem.Row{"payload": []byte("stable")}))
		_, err = audit.QueryContext(ctx, "synthetic", argument)
		Expect(err).To(HaveOccurred())
	})

	It("wraps interceptor errors and preserves their identity", func(ctx SpecContext) {
		failure := errors.New("fixture unavailable")
		database.Public().InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			return pgmem.Result{}, false, failure
		})

		_, err := database.Public().QueryContext(ctx, "synthetic")
		Expect(err).To(MatchError(ContainSubstring("intercept query")))
		Expect(errors.Is(err, failure)).To(BeTrue())
	})

	It("honors cancellation before invoking an interceptor", func() {
		invoked := false
		database.Public().InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			invoked = true
			return pgmem.Result{Rows: []pgmem.Row{{"unexpected": true}}}, true, nil
		})
		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := database.Public().QueryContext(canceled, "synthetic")
		Expect(err).To(MatchError(context.Canceled))
		Expect(invoked).To(BeFalse())
	})

	It("supports concurrent query and subscription churn", func(ctx SpecContext) {
		public := database.Public()
		public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
			return pgmem.Result{Rows: []pgmem.Row{{"ok": true}}}, true, nil
		})

		const workers = 8
		const iterations = 30
		failures := make(chan error, workers*iterations)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for iteration := 0; iteration < iterations; iteration++ {
					subscription := public.InterceptQueries(func(context.Context, string, []any) (pgmem.Result, bool, error) {
						return pgmem.Result{}, false, nil
					})
					if _, err := public.QueryContext(ctx, "synthetic"); err != nil {
						failures <- err
					}
					subscription.Unsubscribe()
					subscription.Unsubscribe()
				}
			}()
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("rejects nil interceptors", func() {
		Expect(func() {
			database.Public().InterceptQueries(nil)
		}).To(PanicWith("pgmem: query interceptor must not be nil"))
	})
})
