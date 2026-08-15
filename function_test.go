package pgmem_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/candacelabs/pgmem"
)

var _ = Describe("Scalar functions", func() {
	It("registers fixed and variadic overloads at runtime", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "calculate",
			Arity: 1,
			Implementation: func(arguments ...any) (any, error) {
				return arguments[0].(int64) + 1, nil
			},
		})).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "calculate",
			Arity: 2,
			Implementation: func(arguments ...any) (any, error) {
				return arguments[0].(int64) + arguments[1].(int64), nil
			},
		})).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:     "words",
			Arity:    1,
			Variadic: true,
			Implementation: func(arguments ...any) (any, error) {
				parts := make([]string, len(arguments))
				for index, argument := range arguments {
					parts[index] = argument.(string)
				}
				return strings.Join(parts, ":"), nil
			},
		})).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "words",
			Arity: 2,
			Implementation: func(...any) (any, error) {
				return "fixed", nil
			},
		})).To(Succeed())

		row, err := database.Public().OneContext(ctx, `
			SELECT calculate(4) AS unary,
			       calculate(4, 5) AS binary,
			       words('one', 'two') AS fixed,
			       words('one', 'two', 'three') AS variadic
		`)
		Expect(err).NotTo(HaveOccurred())
		Expect(row).To(Equal(pgmem.Row{
			"unary":    int64(5),
			"binary":   int64(9),
			"fixed":    "fixed",
			"variadic": "one:two:three",
		}))

		_, err = database.Public().OneContext(ctx, `SELECT words() AS invalid`)
		Expect(err).To(MatchError(ContainSubstring("no overload accepts 0 arguments")))
		var executionError *pgmem.Error
		Expect(errors.As(err, &executionError)).To(BeTrue())
		Expect(executionError.SQLState()).To(Equal("38000"))
	})

	It("isolates functions by database and schema and rewrites nested qualified calls", func(ctx SpecContext) {
		first, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(first.Close)
		second, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(second.Close)

		audit, err := first.CreateSchema(ctx, "audit")
		Expect(err).NotTo(HaveOccurred())
		analytics, err := first.CreateSchema(ctx, "analytics")
		Expect(err).NotTo(HaveOccurred())

		registerConstant := func(schema *pgmem.Schema, name, value string) {
			Expect(schema.RegisterFunction(pgmem.Function{
				Name:  name,
				Arity: 0,
				Implementation: func(...any) (any, error) {
					return value, nil
				},
			})).To(Succeed())
		}
		registerConstant(first.Public(), "source_name", "public")
		registerConstant(audit, "source_name", "audit")
		registerConstant(analytics, "source_name", "analytics")
		registerConstant(second.Public(), "source_name", "second")
		Expect(audit.RegisterFunction(pgmem.Function{
			Name:  "decorate",
			Arity: 1,
			Implementation: func(arguments ...any) (any, error) {
				return "[" + arguments[0].(string) + "]", nil
			},
		})).To(Succeed())

		row, err := first.Public().OneContext(ctx, `
			SELECT source_name() AS local,
			       audit.source_name() AS audit,
			       analytics.source_name() AS analytics,
			       audit.decorate(public.source_name()) AS nested
		`)
		Expect(err).NotTo(HaveOccurred())
		Expect(row).To(Equal(pgmem.Row{
			"local":     "public",
			"audit":     "audit",
			"analytics": "analytics",
			"nested":    "[public]",
		}))
		Expect(audit.OneContext(ctx, `SELECT source_name() AS value`)).To(Equal(pgmem.Row{"value": "audit"}))
		Expect(second.Public().OneContext(ctx, `SELECT source_name() AS value`)).To(Equal(pgmem.Row{"value": "second"}))
	})

	It("rejects direct calls to the internal dispatcher before execution", func(ctx SpecContext) {
		attacker, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(attacker.Close)
		target, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(target.Close)
		var callbackCalls atomic.Int64
		Expect(target.RegisterFunction(pgmem.Function{
			Name:  "private_value",
			Arity: 0,
			Implementation: func(...any) (any, error) {
				callbackCalls.Add(1)
				return "secret", nil
			},
		})).To(Succeed())

		_, err = attacker.Public().OneContext(ctx,
			`SELECT __pgmem_dispatch_749c9c9e('2', 'public', 'private_value')`,
		)
		Expect(err).To(MatchError(ContainSubstring("internal function dispatcher cannot be called directly")))
		var compatibilityError *pgmem.Error
		Expect(errors.As(err, &compatibilityError)).To(BeTrue())
		Expect(compatibilityError.SQLState()).To(Equal("0A000"))
		Expect(callbackCalls.Load()).To(BeZero())
	})

	It("converts callback failures and panics into query errors without poisoning the database", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		failure := errors.New("callback failed")
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "fail_safely",
			Arity: 0,
			Implementation: func(...any) (any, error) {
				return nil, failure
			},
		})).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "panic_safely",
			Arity: 0,
			Implementation: func(...any) (any, error) {
				panic("callback panic")
			},
		})).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "invalid_result",
			Arity: 0,
			Implementation: func(...any) (any, error) {
				return struct{}{}, nil
			},
		})).To(Succeed())

		_, err = database.Public().OneContext(ctx, `SELECT fail_safely() AS value`)
		Expect(err).To(MatchError(And(
			ContainSubstring("fail_safely failed"),
			ContainSubstring(failure.Error()),
		)))
		_, err = database.Public().OneContext(ctx, `SELECT panic_safely() AS value`)
		Expect(err).To(MatchError(And(
			ContainSubstring("panic_safely panicked"),
			ContainSubstring("callback panic"),
		)))
		_, err = database.Public().OneContext(ctx, `SELECT invalid_result() AS value`)
		Expect(err).To(MatchError(ContainSubstring("unsupported scalar type struct {}")))

		Expect(database.Public().OneContext(ctx, `SELECT 42 AS answer`)).To(Equal(pgmem.Row{"answer": int64(42)}))
	})

	It("atomically replaces implementations while queries are running", func() {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(database.Close)

		functionFor := func(value int64) pgmem.Function {
			return pgmem.Function{
				Name:  "changing_value",
				Arity: 0,
				Implementation: func(...any) (any, error) {
					return value, nil
				},
			}
		}
		Expect(database.RegisterFunction(functionFor(0))).To(Succeed())

		const writerCount = 3
		const readerCount = 6
		const iterations = 40
		failures := make(chan error, (writerCount+readerCount)*iterations)
		var workers sync.WaitGroup
		for writer := 0; writer < writerCount; writer++ {
			workers.Add(1)
			go func(offset int) {
				defer workers.Done()
				for iteration := 0; iteration < iterations; iteration++ {
					value := int64((offset + iteration) % 2)
					if err := database.RegisterFunction(functionFor(value)); err != nil {
						failures <- err
					}
				}
			}(writer)
		}
		for reader := 0; reader < readerCount; reader++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for iteration := 0; iteration < iterations; iteration++ {
					row, queryErr := database.Public().One(`SELECT changing_value() AS value`)
					if queryErr != nil {
						failures <- queryErr
						continue
					}
					value, ok := row["value"].(int64)
					if !ok || (value != 0 && value != 1) {
						failures <- fmt.Errorf("unexpected changing_value result: %#v", row["value"])
					}
				}
			}()
		}
		workers.Wait()
		close(failures)
		for failure := range failures {
			Expect(failure).NotTo(HaveOccurred())
		}
	})

	It("validates definitions and rejects registration after close", func() {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())

		validImplementation := func(...any) (any, error) { return nil, nil }
		Expect(database.RegisterFunction(pgmem.Function{
			Name:           "",
			Implementation: validImplementation,
		})).To(MatchError(ContainSubstring("invalid function name")))
		Expect(database.RegisterFunction(pgmem.Function{
			Name:           "invalid.name",
			Implementation: validImplementation,
		})).To(MatchError(ContainSubstring("invalid function name")))
		Expect(database.RegisterFunction(pgmem.Function{
			Name:           "negative_arity",
			Arity:          -1,
			Implementation: validImplementation,
		})).To(MatchError(ContainSubstring("arity must not be negative")))
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "missing_implementation",
			Arity: 1,
		})).To(MatchError(ContainSubstring("implementation must not be nil")))

		Expect(database.Close()).To(Succeed())
		Expect(database.RegisterFunction(pgmem.Function{
			Name:           "too_late",
			Implementation: validImplementation,
		})).To(MatchError(pgmem.ErrClosed))
	})

	It("does not deadlock Close against a callback registering another overload", func(ctx SpecContext) {
		database, err := pgmem.New()
		Expect(err).NotTo(HaveOccurred())
		entered := make(chan struct{})
		release := make(chan struct{})
		Expect(database.RegisterFunction(pgmem.Function{
			Name:  "register_during_close",
			Arity: 0,
			Implementation: func(...any) (any, error) {
				close(entered)
				<-release
				return nil, database.RegisterFunction(pgmem.Function{
					Name:  "late_registration",
					Arity: 0,
					Implementation: func(...any) (any, error) {
						return int64(1), nil
					},
				})
			},
		})).To(Succeed())

		queryDone := make(chan error, 1)
		go func() {
			_, queryErr := database.Public().One(`SELECT register_during_close() AS value`)
			queryDone <- queryErr
		}()
		Eventually(entered, time.Second).Should(BeClosed())

		closeDone := make(chan error, 1)
		go func() { closeDone <- database.Close() }()
		close(release)
		Eventually(queryDone, time.Second).Should(Receive())
		Eventually(closeDone, time.Second).Should(Receive())
		_, err = database.Public().OneContext(context.Background(), `SELECT 1`)
		Expect(errors.Is(err, pgmem.ErrClosed)).To(BeTrue())
	})
})
